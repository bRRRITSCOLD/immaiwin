package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/api/handler"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/api/middleware"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/config"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/mongodb"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/rediss"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/sandbox"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/workflow"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

type Server struct {
	cfg                config.APIConfig
	broadcaster        *rediss.Broadcaster
	newsBroadcaster    *rediss.Broadcaster
	optsBroadcaster    *rediss.Broadcaster
	futuresBroadcaster *rediss.Broadcaster
	server             *http.Server
}

type marketsClient interface {
	handler.MarketsGetter
	handler.MarketsSearcher
	handler.EventsGetter
	handler.EventsSearcher
}

func NewServer(
	cfg config.APIConfig,
	authCfg config.AuthConfig,
	rc *rediss.Client,
	pm marketsClient,
	wl handler.WatchlistStore,
	tr handler.TradesLister,
	nr handler.NewsLister,
	schwabAuth handler.SchwabAuthorizer,
	owl handler.OptionsWatchlistStore,
	fwl handler.FuturesWatchlistStore,
	sc handler.ScraperConfigStore,
	wfStore handler.WorkflowStore,
	wfRunStore workflow.WorkflowRunStore,
	wfExec *workflow.WorkflowExecutor,
	connStore handler.ConnectionStore,
	connInvalidator handler.ConnectionInvalidator,
	skillBackend *handler.SkillBackend,
	evalDeps handler.EvalDeps,
	users *mongodb.UserRepository,
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
		Cfg:      authCfg,
		JWTBytes: []byte(authCfg.JWTSecret),
		TTL:      authTTL,
	}
	// OptionalAuth on every request so handlers can read the user
	// from ctx when present (Phase B will swap to RequireAuth on
	// data routes once tenant scoping is in). Skipped during Phase A
	// when users repo is nil (degraded boot).
	if users != nil && len(authDeps.JWTBytes) > 0 {
		r.Use(middleware.OptionalAuth(authDeps.JWTBytes, users))
	}

	r.GET("/health", handler.Health)

	// User auth — public except /me which requires a valid token.
	r.POST("/api/v1/auth/register", handler.Register(authDeps))
	r.POST("/api/v1/auth/login", handler.Login(authDeps))
	r.POST("/api/v1/auth/logout", handler.Logout(authDeps))
	if users != nil && len(authDeps.JWTBytes) > 0 {
		r.GET("/api/v1/auth/me",
			middleware.RequireAuth(authDeps.JWTBytes, users),
			handler.Me(authDeps))
	}

	// Schwab OAuth (legacy — predates user auth)
	r.GET("/auth/schwab", handler.SchwabAuthorize(schwabAuth))
	r.GET("/auth/schwab/callback", handler.SchwabCallback(schwabAuth))
	r.GET("/api/v1/auth/schwab/status", handler.SchwabStatus(schwabAuth))
	r.DELETE("/api/v1/auth/schwab", handler.SchwabDisconnect(schwabAuth))

	// Trades (Polymarket)
	r.GET("/api/v1/trades/stream", handler.StreamTrades(tr, b))
	r.GET("/api/v1/trades", handler.GetTrades(tr))

	// News
	r.GET("/api/v1/news", handler.GetNews(nr))
	r.GET("/api/v1/news/stream", handler.StreamNews(nb))
	r.GET("/api/v1/news/scrapers", handler.ListScraperConfigs(sc))
	r.PATCH("/api/v1/news/scrapers/:source", handler.PatchScraperConfig(sc))
	r.DELETE("/api/v1/news/scrapers/:source", handler.DeleteScraperConfig(sc))
	r.DELETE("/api/v1/news/scrapers/:source/script", handler.DeleteScraperScript(sc))
	r.POST("/api/v1/news/scrapers/validate", handler.ValidateScript())

	// Workflows
	r.GET("/api/v1/workflows", handler.ListWorkflows(wfStore))
	r.GET("/api/v1/workflows/:id", handler.GetWorkflow(wfStore))
	r.PUT("/api/v1/workflows/:id", handler.UpsertWorkflow(wfStore))
	r.DELETE("/api/v1/workflows/:id", handler.DeleteWorkflow(wfStore))
	r.POST("/api/v1/workflows/:id/run", handler.RunWorkflow(wfStore, wfExec))
	r.GET("/api/v1/workflows/:id/run/stream", handler.RunWorkflowWS(wfStore, wfExec))
	r.GET("/api/v1/workflows/:id/ws-preview", handler.PreviewWorkflowWS(wfStore, connStore, db))

	// Workflow runs (history page). Register the static `daily_total`
	// route BEFORE `:id` so gin's radix tree doesn't route the literal
	// segment through the wildcard handler.
	r.GET("/api/v1/workflow_runs", handler.ListWorkflowRuns(wfRunStore))
	r.GET("/api/v1/workflow_runs/daily_total", handler.DailyTotal(wfRunStore, wfStore))
	r.GET("/api/v1/workflow_runs/daily_totals", handler.DailyTotals(wfRunStore, wfStore))
	r.GET("/api/v1/workflow_runs/:id", handler.GetWorkflowRun(wfRunStore, wfStore))
	r.POST("/api/v1/workflow_runs/:id/approval", handler.SubmitRunApproval(wfExec, wfRunStore))
	r.POST("/api/v1/workflow_runs/:id/cancel", handler.CancelRun(wfExec, wfRunStore))

	// Webhook trigger — POST /api/v1/webhooks/:slug runs the workflow
	// whose trigger node has matching webhook_slug. Body becomes the
	// trigger output (JSON or raw string).
	r.POST("/api/v1/webhooks/:slug", handler.HandleWebhook(wfStore, wfExec))

	// Workflow templates — bundled at compile time, served read-only.
	r.GET("/api/v1/workflow_templates", handler.ListWorkflowTemplates())

	// Connections
	r.GET("/api/v1/connections", handler.ListConnections(connStore))
	r.PUT("/api/v1/connections/:id", handler.UpsertConnection(connStore, connInvalidator))
	r.DELETE("/api/v1/connections/:id", handler.DeleteConnection(connStore, connInvalidator))
	r.POST("/api/v1/connections/test", handler.TestConnection(db))

	// Skills (P1.10/P1.12). When skills are disabled at boot the backend is
	// nil and the handlers respond with empty/disabled responses; the routes
	// are still registered so the UI can call them unconditionally.
	r.GET("/api/v1/skills", handler.ListSkills(skillBackend))
	r.POST("/api/v1/skills/refresh", handler.RefreshSkills(skillBackend))

	// Evals (P-eval). Disabled when EvalDeps is empty; handlers respond
	// with 503 so the UI can fail open.
	r.GET("/api/v1/evals", handler.ListEvals(evalDeps))
	r.PUT("/api/v1/evals/:id", handler.UpsertEval(evalDeps))
	r.GET("/api/v1/evals/:id", handler.GetEval(evalDeps))
	r.DELETE("/api/v1/evals/:id", handler.DeleteEval(evalDeps))
	r.POST("/api/v1/evals/:id/run", handler.RunEval(evalDeps))
	r.GET("/api/v1/eval_runs", handler.ListEvalRuns(evalDeps))
	r.GET("/api/v1/eval_runs/:id", handler.GetEvalRun(evalDeps))

	// Connection OAuth (generic)
	r.GET("/auth/connections/:id/callback", handler.ConnectionOAuthCallback(connStore, db))
	r.GET("/api/v1/connections/:id/oauth/url", handler.ConnectionOAuthURL(connStore, db, cfg.BaseURL))
	r.GET("/api/v1/connections/:id/oauth/status", handler.ConnectionOAuthStatus(connStore, db))

	// Polymarket markets
	r.GET("/api/v1/markets", handler.GetMarkets(pm))
	r.GET("/api/v1/markets/search", handler.SearchMarkets(pm))
	r.GET("/api/v1/events", handler.GetEvents(pm))
	r.GET("/api/v1/events/search", handler.SearchEvents(pm))

	// Polymarket watchlist
	r.GET("/api/v1/watchlist", handler.GetWatchlist(wl))
	r.PUT("/api/v1/watchlist", handler.SyncWatchlist(wl))
	r.PATCH("/api/v1/watchlist/:market_id/config", handler.UpdateWatchlistConfig(wl))

	// Options watchlist + stream
	r.GET("/api/v1/options/watchlist", handler.GetOptionsWatchlist(owl))
	r.PUT("/api/v1/options/watchlist", handler.SyncOptionsWatchlist(owl))
	r.GET("/api/v1/options/stream", handler.StreamOptions(ob))

	// Futures watchlist + stream
	r.GET("/api/v1/futures/watchlist", handler.GetFuturesWatchlist(fwl))
	r.PUT("/api/v1/futures/watchlist", handler.SyncFuturesWatchlist(fwl))
	r.GET("/api/v1/futures/stream", handler.StreamFutures(fb))

	// Sandbox (WebSocket)
	r.GET("/api/v1/sandbox/debug", handler.DebugSandbox(sandboxRT))
	r.GET("/api/v1/sandbox/run", handler.RunSandbox(sandboxRT))

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

func (s *Server) Shutdown(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}

func (s *Server) Addr() string {
	return s.server.Addr
}
