package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/api/handler"
	"github.com/bRRRITSCOLD/burrow/internal/api/middleware"
	"github.com/bRRRITSCOLD/burrow/internal/config"
	"github.com/bRRRITSCOLD/burrow/internal/email"
	"github.com/bRRRITSCOLD/burrow/internal/mongodb"
	"github.com/bRRRITSCOLD/burrow/internal/rediss"
	"github.com/bRRRITSCOLD/burrow/internal/sandbox"
	"github.com/bRRRITSCOLD/burrow/internal/workflow"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// warnRedirectHostMismatch flags the silent OAuth-cookie-loss footgun:
// the post-callback Set-Cookie binds to the REDIRECT_URL host, but the
// UI talks to the API at API_BASE_URL host. If those don't match
// (classic localhost vs localhost), the cookie travels nowhere useful
// and the user bounces back to /login.
func warnRedirectHostMismatch(apiBaseURL, provider string, p config.OAuthProviderConfig) {
	if p.ClientID == "" || p.RedirectURL == "" || apiBaseURL == "" {
		return
	}
	api, err1 := url.Parse(apiBaseURL)
	red, err2 := url.Parse(p.RedirectURL)
	if err1 != nil || err2 != nil {
		return
	}
	if api.Hostname() != red.Hostname() {
		slog.Warn("oauth: REDIRECT_URL host differs from API_BASE_URL host — auth cookie won't travel on /auth/me from UI",
			"provider", provider,
			"api_host", api.Hostname(),
			"redirect_host", red.Hostname(),
			"fix", "set AUTH_OAUTH_"+provider+"_REDIRECT_URL host to match API_BASE_URL host",
		)
	}
}

type Server struct {
	cfg                config.APIConfig
	broadcaster        *rediss.Broadcaster
	newsBroadcaster    *rediss.Broadcaster
	optsBroadcaster    *rediss.Broadcaster
	futuresBroadcaster *rediss.Broadcaster
	server             *http.Server
}

func NewServer(
	cfg config.APIConfig,
	authCfg config.AuthConfig,
	rc *rediss.Client,
	wfStore handler.WorkflowStore,
	wfRunStore workflow.WorkflowRunStore,
	wfExec *workflow.WorkflowExecutor,
	connStore handler.ConnectionStore,
	connInvalidator handler.ConnectionInvalidator,
	skillBackend *handler.SkillBackend,
	evalDeps handler.EvalDeps,
	users *mongodb.UserRepository,
	tenants *mongodb.TenantRepository,
	apiKeys *mongodb.APIKeyRepository,
	workerHealth *mongodb.WorkerHealthRepository,
	invites *mongodb.InviteRepository,
	audit *mongodb.AuditRepository,
	emailSender email.Sender,
	db *mongo.Database,
	sandboxRT sandbox.Runtime,
) *Server {
	b := rediss.NewBroadcaster(rc, rediss.TradesChannel)
	nb := rediss.NewBroadcaster(rc, rediss.NewsChannel)
	ob := rediss.NewBroadcaster(rc, rediss.OptionsChannel)
	fb := rediss.NewBroadcaster(rc, rediss.FuturesChannel)

	r := gin.New()
	// CORS w/ credentials so the UI's httpOnly auth cookie rides over
	// cross-origin requests in dev (UI on :3000, API on :8080). Allow
	// any origin since dev has no fixed host; tighten via config in
	// prod once we add an UI base-URL env var.
	corsCfg := cors.Config{
		AllowOriginFunc:  func(string) bool { return true },
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "PATCH", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Accept", "Authorization"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}
	r.Use(gin.Logger(), gin.Recovery(), cors.New(corsCfg))

	// Auth dependencies — wired once, threaded through both the
	// /api/v1/auth handlers and the request-scoped middleware that
	// validates JWTs on protected routes.
	authTTL := 168 * time.Hour
	if authCfg.JWTTTL != "" {
		if d, err := time.ParseDuration(authCfg.JWTTTL); err == nil && d > 0 {
			authTTL = d
		}
	}
	authDeps := handler.AuthDeps{
		Users:    users,
		Tenants:  tenants,
		Audit:    audit,
		Cfg:      authCfg,
		JWTBytes: []byte(authCfg.JWTSecret),
		TTL:      authTTL,
	}
	// OptionalAuth on every request so handlers can read the user
	// from ctx when present. Skipped during degraded boot when users
	// repo is nil.
	if users != nil && len(authDeps.JWTBytes) > 0 {
		r.Use(middleware.OptionalAuth(authDeps.JWTBytes, users, apiKeys))
	}

	// requireAuth is the gate we attach to tenant-scoped data routes.
	// In degraded boot (no users repo / no JWT secret) it falls back
	// to a no-op so the API still serves — useful for local hacking
	// without configuring auth, and matches the "skip OptionalAuth"
	// behaviour above. In any non-dev deployment AUTH_JWT_SECRET is
	// required so this branch is always live.
	requireAuth := gin.HandlerFunc(func(c *gin.Context) { c.Next() })
	if users != nil && len(authDeps.JWTBytes) > 0 {
		requireAuth = middleware.RequireAuth(authDeps.JWTBytes, users, apiKeys)
	}

	r.GET("/health", handler.Health)

	// Per-IP rate limits on auth surfaces. Redis-backed; fails open
	// when Redis is down so an outage doesn't take auth offline.
	// Tuned for human flow: a typo-retry burst of 5 fits inside the
	// minute window, but a brute-force loop hits 429 inside a second.
	loginLimit := middleware.RateLimit(rc, middleware.RateLimitConfig{
		Name: "login", Max: 10, Window: time.Minute,
	})
	registerLimit := middleware.RateLimit(rc, middleware.RateLimitConfig{
		Name: "register", Max: 5, Window: time.Minute,
	})
	oauthStartLimit := middleware.RateLimit(rc, middleware.RateLimitConfig{
		Name: "oauth_start", Max: 20, Window: time.Minute,
	})
	pwResetLimit := middleware.RateLimit(rc, middleware.RateLimitConfig{
		Name: "password_reset", Max: 5, Window: time.Minute,
	})

	// User auth — public except /me which requires a valid token.
	r.POST("/api/v1/auth/register", registerLimit, handler.Register(authDeps))
	r.POST("/api/v1/auth/login", loginLimit, handler.Login(authDeps))
	r.POST("/api/v1/auth/logout", handler.Logout(authDeps))

	// Password reset — public; rate-limited so an attacker can't
	// enumerate emails or burn through tokens by spamming Confirm.
	if users != nil && len(authDeps.JWTBytes) > 0 {
		pwDeps := handler.PasswordResetDeps{
			Users:     users,
			Audit:     audit,
			JWTBytes:  authDeps.JWTBytes,
			UIBaseURL: authCfg.UIBaseURL,
			Email:     emailSender, // dev default; swap for SMTP later
			Redis:     rc,
		}
		r.POST("/api/v1/auth/password_reset/request", pwResetLimit, handler.PasswordResetRequest(pwDeps))
		r.POST("/api/v1/auth/password_reset/confirm", pwResetLimit, handler.PasswordResetConfirm(pwDeps))
	}

	// OAuth (Google + GitHub). Public — no auth needed since the
	// flow IS the auth. Disabled providers return 404 from handlers.
	if users != nil && tenants != nil && len(authDeps.JWTBytes) > 0 {
		// Cookie-host sanity check: if OAuth REDIRECT_URL host differs
		// from API_BASE_URL host (e.g. localhost vs localhost), the
		// browser stores the post-callback auth cookie on the redirect
		// host and won't send it on UI's /auth/me calls. The OAuth
		// flow looks broken (back to /login) even though the user got
		// created. Loud warning at boot beats a silent bounce loop.
		warnRedirectHostMismatch(cfg.BaseURL, "google", authCfg.OAuthGoogle)
		warnRedirectHostMismatch(cfg.BaseURL, "github", authCfg.OAuthGithub)
		oauthDeps := handler.OAuthDeps{
			Cfg:      authCfg,
			JWTBytes: authDeps.JWTBytes,
			TTL:      authDeps.TTL,
			Users:    users,
			Tenants:  tenants,
			Audit:    audit,
		}
		r.GET("/auth/oauth/:provider/start", oauthStartLimit, handler.OAuthStart(oauthDeps))
		r.GET("/auth/oauth/:provider/callback", handler.OAuthCallback(oauthDeps))
	}
	if users != nil && len(authDeps.JWTBytes) > 0 {
		r.GET("/api/v1/auth/me",
			middleware.RequireAuth(authDeps.JWTBytes, users, apiKeys),
			handler.Me(authDeps))
		r.POST("/api/v1/auth/switch_tenant",
			middleware.RequireAuth(authDeps.JWTBytes, users, apiKeys),
			handler.SwitchTenant(authDeps))
		r.POST("/api/v1/auth/change_password",
			middleware.RequireAuth(authDeps.JWTBytes, users, apiKeys),
			handler.ChangePassword(authDeps))
		r.DELETE("/api/v1/auth/oauth/:provider/unlink",
			middleware.RequireAuth(authDeps.JWTBytes, users, apiKeys),
			handler.UnlinkOAuth(authDeps))
		// WS-token mint — short-lived JWT used as ?token= on WebSocket
		// upgrades (browser WS API can't set headers, cookies don't
		// always travel cross-origin on upgrade).
		r.POST("/api/v1/auth/ws_token",
			middleware.RequireAuth(authDeps.JWTBytes, users, apiKeys),
			handler.WSToken(authDeps))
		// API keys (programmatic access). All routes require auth via
		// JWT cookie OR existing API key. Listing/revoking from an
		// API-key context is allowed so a CLI can manage its own keys.
		apiKeyDeps := handler.APIKeyDeps{Keys: apiKeys, Audit: audit}
		r.GET("/api/v1/api_keys",
			middleware.RequireAuth(authDeps.JWTBytes, users, apiKeys),
			handler.ListAPIKeys(apiKeyDeps))
		r.POST("/api/v1/api_keys",
			middleware.RequireAuth(authDeps.JWTBytes, users, apiKeys),
			handler.CreateAPIKey(apiKeyDeps))
		r.DELETE("/api/v1/api_keys/:id",
			middleware.RequireAuth(authDeps.JWTBytes, users, apiKeys),
			handler.RevokeAPIKey(apiKeyDeps))
	}

	// Workflows — tenant-scoped CRUD + run-time. WS routes accept
	// ?token=<short-lived JWT> via the middleware's query fallback.
	r.GET("/api/v1/workflows", requireAuth, handler.ListWorkflows(wfStore))
	r.GET("/api/v1/workflows/:id", requireAuth, handler.GetWorkflow(wfStore))
	r.PUT("/api/v1/workflows/:id", requireAuth, handler.UpsertWorkflow(wfStore))
	r.DELETE("/api/v1/workflows/:id", requireAuth, handler.DeleteWorkflow(wfStore))
	r.POST("/api/v1/workflows/:id/run", requireAuth, handler.RunWorkflow(wfStore, wfExec))
	r.GET("/api/v1/workflows/:id/run/stream", requireAuth, handler.RunWorkflowWS(wfStore, wfExec))

	// Workflow runs (history page). Register the static `daily_total`
	// route BEFORE `:id` so gin's radix tree doesn't route the literal
	// segment through the wildcard handler.
	r.GET("/api/v1/workflow_runs", requireAuth, handler.ListWorkflowRuns(wfRunStore))
	r.GET("/api/v1/workflow_runs/daily_total", requireAuth, handler.DailyTotal(wfRunStore, wfStore))
	r.GET("/api/v1/workflow_runs/daily_totals", requireAuth, handler.DailyTotals(wfRunStore, wfStore))
	r.GET("/api/v1/workflow_runs/:id", requireAuth, handler.GetWorkflowRun(wfRunStore, wfStore))
	r.POST("/api/v1/workflow_runs/:id/approval", requireAuth, handler.SubmitRunApproval(wfExec, wfRunStore))
	r.POST("/api/v1/workflow_runs/:id/cancel", requireAuth, handler.CancelRun(wfExec, wfRunStore))

	// Webhook trigger — POST /api/v1/webhooks/:slug runs the workflow
	// whose trigger node has matching webhook_slug. Body becomes the
	// trigger output (JSON or raw string).
	r.POST("/api/v1/webhooks/:slug", handler.HandleWebhook(wfStore, wfExec))

	// Workflow templates — bundled at compile time, served read-only.
	r.GET("/api/v1/workflow_templates", handler.ListWorkflowTemplates())

	// Connections — tenant-scoped.
	r.GET("/api/v1/connections", requireAuth, handler.ListConnections(connStore))
	r.PUT("/api/v1/connections/:id", requireAuth, handler.UpsertConnection(connStore, connInvalidator))
	r.DELETE("/api/v1/connections/:id", requireAuth, handler.DeleteConnection(connStore, connInvalidator))
	r.POST("/api/v1/connections/test", requireAuth, handler.TestConnection(db))

	// Skills (P1.10/P1.12). When skills are disabled at boot the backend is
	// nil and the handlers respond with empty/disabled responses; the routes
	// are still registered so the UI can call them unconditionally.
	r.GET("/api/v1/skills", requireAuth, handler.ListSkills(skillBackend))
	r.POST("/api/v1/skills/refresh", requireAuth, handler.RefreshSkills(skillBackend))

	// Worker observability — dashboards/alerts read live worker
	// heartbeats. Auth-gated; worker names + last_error strings can
	// leak internal architecture.
	r.GET("/api/v1/workers/health", requireAuth, handler.ListWorkerHealth(handler.WorkerHealthDeps{Health: workerHealth}))

	// Tenant invites + member management. All require auth + tenant
	// context; specific endpoints additionally enforce owner/admin via
	// requireTenantAdmin in the handler.
	if invites != nil && tenants != nil && users != nil {
		inviteDeps := handler.InviteDeps{
			Invites:   invites,
			Tenants:   tenants,
			Users:     users,
			Audit:     audit,
			Email:     emailSender,
			UIBaseURL: authCfg.UIBaseURL,
		}
		r.POST("/api/v1/tenants/invites", requireAuth, handler.CreateInvite(inviteDeps))
		r.GET("/api/v1/tenants/invites", requireAuth, handler.ListInvites(inviteDeps))
		r.DELETE("/api/v1/tenants/invites/:id", requireAuth, handler.RevokeInvite(inviteDeps))
		r.GET("/api/v1/tenants/members", requireAuth, handler.ListMembers(inviteDeps))
		r.DELETE("/api/v1/tenants/members/:user_id", requireAuth, handler.RemoveMember(inviteDeps))
		r.POST("/api/v1/tenants/transfer", requireAuth, handler.TransferOwnership(inviteDeps))
		// Public preview (no auth) — UI uses this to show invite
		// metadata before signup; accept requires auth.
		r.GET("/api/v1/invites/:token/preview", handler.PreviewInvite(inviteDeps))
		r.POST("/api/v1/invites/:token/accept", requireAuth, handler.AcceptInvite(inviteDeps))
	}

	// Audit log read endpoint. Owner/admin only — audit entries can
	// leak member emails + action timing.
	if audit != nil && tenants != nil {
		r.GET("/api/v1/audit_log", requireAuth,
			handler.ListAuditLog(handler.AuditLogDeps{Audit: audit, Tenants: tenants}))
	}

	// Run-level metrics dashboard payload. Owner/admin only — cost
	// burn + activity counts are sensitive to non-admin members.
	if wfRunStore != nil && tenants != nil {
		runRepo, _ := wfRunStore.(*mongodb.WorkflowRunRepository)
		wfRepoConcrete, _ := wfStore.(*mongodb.WorkflowRepository)
		if runRepo != nil {
			r.GET("/api/v1/runs/metrics", requireAuth,
				handler.GetRunMetrics(handler.RunMetricsDeps{
					Runs:      runRepo,
					Tenants:   tenants,
					Workflows: wfRepoConcrete,
				}))
		}
	}

	// Evals (P-eval). Disabled when EvalDeps is empty; handlers respond
	// with 503 so the UI can fail open.
	r.GET("/api/v1/evals", requireAuth, handler.ListEvals(evalDeps))
	r.PUT("/api/v1/evals/:id", requireAuth, handler.UpsertEval(evalDeps))
	r.GET("/api/v1/evals/:id", requireAuth, handler.GetEval(evalDeps))
	r.DELETE("/api/v1/evals/:id", requireAuth, handler.DeleteEval(evalDeps))
	r.POST("/api/v1/evals/:id/run", requireAuth, handler.RunEval(evalDeps))
	r.GET("/api/v1/eval_runs", requireAuth, handler.ListEvalRuns(evalDeps))
	r.GET("/api/v1/eval_runs/:id", requireAuth, handler.GetEvalRun(evalDeps))

	// Sandbox (WebSocket) — executes user code; must be auth-gated.
	// WS upgrades pass a short-lived ?token= via the middleware's
	// query-string fallback (browser WS API can't set headers).
	r.GET("/api/v1/sandbox/debug", requireAuth, handler.DebugSandbox(sandboxRT))
	r.GET("/api/v1/sandbox/run", requireAuth, handler.RunSandbox(sandboxRT))

	return &Server{
		cfg:                cfg,
		broadcaster:        b,
		newsBroadcaster:    nb,
		optsBroadcaster:    ob,
		futuresBroadcaster: fb,
		server: &http.Server{
			Addr:    fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
			Handler: r,
		},
	}
}

// Start launches Redis subscriber broadcasters then serves HTTP (or HTTPS if TLS configured).
func (s *Server) Start(ctx context.Context) error {
	go s.broadcaster.Run(ctx)
	go s.newsBroadcaster.Run(ctx)
	go s.optsBroadcaster.Run(ctx)
	go s.futuresBroadcaster.Run(ctx)
	if s.cfg.TLSCertFile != "" && s.cfg.TLSKeyFile != "" {
		return s.server.ListenAndServeTLS(s.cfg.TLSCertFile, s.cfg.TLSKeyFile)
	}
	return s.server.ListenAndServe()
}

// Handler returns the wired-up gin router. Exposed so integration
// tests can mount the full server (real routes + middleware chain)
// behind an httptest.NewServer without binding a port via Start.
func (s *Server) Handler() http.Handler {
	return s.server.Handler
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}

func (s *Server) Addr() string {
	return s.server.Addr
}
