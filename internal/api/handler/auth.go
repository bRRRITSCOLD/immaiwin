// Auth endpoints — Phase A foundation.
//
//	POST /api/v1/auth/register   { email, password }
//	POST /api/v1/auth/login      { email, password }
//	POST /api/v1/auth/logout     (clears cookie)
//	GET  /api/v1/auth/me         (returns current user from JWT)
//
// Phase A issues a JWT on register/login. Phase B will populate the
// tenant_id claim once the tenant model lands. UI can already start
// reading /auth/me to gate routes.

package handler

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/auth"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/config"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/mongodb"
	"github.com/gin-gonic/gin"
	"github.com/oklog/ulid/v2"
)

// AuthDeps bundles everything the auth handlers need so the wiring at
// the server level is one struct, not many separate args.
type AuthDeps struct {
	Users    *mongodb.UserRepository
	Tenants  *mongodb.TenantRepository
	Audit    *mongodb.AuditRepository
	Cfg      config.AuthConfig
	JWTBytes []byte
	TTL      time.Duration
}

// authCookieName is the cookie key the API sets/reads for browser
// sessions. Bearer-token API clients carry the same JWT in
// Authorization headers — both flow through the same middleware.
const authCookieName = "immaiwin_auth"

// setAuthCookie writes the JWT to the browser as an httpOnly cookie.
// SameSite=Lax is right for first-party UI; flip to Strict if you
// don't need cross-tab redirect flows. Secure flag toggled by config
// (off in dev/HTTP, on in prod/HTTPS).
func setAuthCookie(c *gin.Context, deps AuthDeps, token string) {
	maxAge := int(deps.TTL.Seconds())
	sameSite := http.SameSiteLaxMode
	c.SetSameSite(sameSite)
	c.SetCookie(authCookieName, token, maxAge, "/", deps.Cfg.CookieDomain, deps.Cfg.CookieSecure, true)
}

// clearAuthCookie removes the auth cookie on logout.
func clearAuthCookie(c *gin.Context, deps AuthDeps) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(authCookieName, "", -1, "/", deps.Cfg.CookieDomain, deps.Cfg.CookieSecure, true)
}

// Register handles POST /api/v1/auth/register. Disabled when
// AllowRegistration=false (closed deployments).
func Register(deps AuthDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !deps.Cfg.AllowRegistration {
			c.JSON(http.StatusForbidden, gin.H{"error": "registration disabled"})
			return
		}
		if deps.Users == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "user store not configured"})
			return
		}
		var req struct {
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		req.Email = strings.ToLower(strings.TrimSpace(req.Email))
		if req.Email == "" || !strings.Contains(req.Email, "@") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "valid email required"})
			return
		}
		hash, err := auth.PasswordHash(req.Password)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		u, err := deps.Users.Create(c.Request.Context(), mongodb.User{
			ID:           ulid.Make().String(),
			Email:        req.Email,
			PasswordHash: hash,
		})
		if err != nil {
			if errors.Is(err, mongodb.ErrUserExists) {
				c.JSON(http.StatusConflict, gin.H{"error": "email already registered"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		// Mint a personal tenant + add the user as owner. Every user
		// gets at least one tenant on signup so the JWT can always
		// carry an active tenant_id. Additional tenants come via
		// invite (post-launch backlog).
		var tenantID string
		if deps.Tenants != nil {
			t, terr := deps.Tenants.CreateWithOwner(c.Request.Context(), mongodb.Tenant{
				ID:      ulid.Make().String(),
				Name:    req.Email + "'s workspace",
				OwnerID: u.ID,
			})
			if terr != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "create tenant: " + terr.Error()})
				return
			}
			tenantID = t.ID
		}
		tok, err := auth.IssueJWT(deps.JWTBytes, u.ID, tenantID, deps.TTL)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		setAuthCookie(c, deps, tok)
		c.JSON(http.StatusOK, gin.H{
			"user":      u,
			"tenant_id": tenantID,
			"token":     tok,
		})
	}
}

// Login authenticates by email + password. Returns JWT both as
// httpOnly cookie (UI) and in the body (API clients). Constant-time
// equal via bcrypt avoids timing leaks.
func Login(deps AuthDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps.Users == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "user store not configured"})
			return
		}
		var req struct {
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		req.Email = strings.ToLower(strings.TrimSpace(req.Email))

		u, err := deps.Users.GetByEmail(c.Request.Context(), req.Email)
		if err != nil {
			// Mask "no such user" vs "wrong password" so attackers
			// can't probe email enumeration.
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid email or password"})
			return
		}
		if !auth.PasswordVerify(req.Password, u.PasswordHash) {
			// Stamp userID so the row lands inside the user's tenant
			// view post-login. tenantID still empty here (we haven't
			// resolved a membership for the failed attempt) — that's
			// fine; the row is still discoverable by user_id index.
			recordAuditUnauth(c, deps.Audit, mongodb.AuditLoginFailure, req.Email, u.ID, "", map[string]any{"reason": "wrong_password"})
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid email or password"})
			return
		}
		_ = deps.Users.TouchLastLogin(c.Request.Context(), u.ID)
		// Pick the user's first membership as the active tenant. UI's
		// tenant switcher (Phase F) sends a refresh request to
		// re-issue the JWT with a different active tenant.
		var tenantID string
		if deps.Tenants != nil {
			memberships, _ := deps.Tenants.ListMembershipsForUser(c.Request.Context(), u.ID)
			if len(memberships) > 0 {
				tenantID = memberships[0].Tenant.ID
			} else {
				// Edge case: user has no tenant (shouldn't happen post-
				// register, but defensive). Mint one on first login so
				// downstream features don't crash on empty tenant_id.
				t, terr := deps.Tenants.CreateWithOwner(c.Request.Context(), mongodb.Tenant{
					ID:      ulid.Make().String(),
					Name:    u.Email + "'s workspace",
					OwnerID: u.ID,
				})
				if terr == nil {
					tenantID = t.ID
				}
			}
		}
		tok, err := auth.IssueJWT(deps.JWTBytes, u.ID, tenantID, deps.TTL)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		setAuthCookie(c, deps, tok)
		recordAuditUnauth(c, deps.Audit, mongodb.AuditLoginSuccess, u.Email, u.ID, tenantID, nil)
		c.JSON(http.StatusOK, gin.H{
			"user":      u,
			"tenant_id": tenantID,
			"token":     tok,
		})
	}
}

// Logout clears the cookie. Bearer-token clients can also call this
// for symmetry — it's a no-op server-side beyond the Set-Cookie
// header. (Token revocation list deferred to backlog.)
func Logout(deps AuthDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Audit BEFORE clearing the cookie — recordAudit reads
		// user/tenant from ctx, which OptionalAuth populated upstream.
		recordAudit(c, deps.Audit, mongodb.AuditLogout, nil, nil)
		clearAuthCookie(c, deps)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

// Me returns the authenticated user. Auth middleware must run upstream;
// returns 401 when no user in ctx.
func Me(deps AuthDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		u, ok := auth.UserFromCtx(c.Request.Context())
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "not authenticated"})
			return
		}
		// Refresh from DB so /me always reflects current state.
		full, err := deps.Users.GetByID(c.Request.Context(), u.ID)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "user not found"})
			return
		}
		tenantID, _ := auth.TenantFromCtx(c.Request.Context())
		var memberships []mongodb.Membership
		if deps.Tenants != nil {
			memberships, _ = deps.Tenants.ListMembershipsForUser(c.Request.Context(), u.ID)
		}
		c.JSON(http.StatusOK, gin.H{
			"user":        full,
			"tenant_id":   tenantID,
			"memberships": memberships,
		})
	}
}

// WSToken mints a short-lived JWT for use as a `?token=` query
// parameter on WebSocket upgrades. Browser WebSocket API can't set
// request headers and the auth cookie doesn't always travel cross-
// origin on the upgrade handshake, so the UI calls this just before
// opening the socket and appends the returned token to the WS URL.
//
// 60-second TTL keeps the leakage window tight if the URL ends up in
// a server log or proxy cache.
func WSToken(deps AuthDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		uctx, ok := auth.UserFromCtx(c.Request.Context())
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "not authenticated"})
			return
		}
		tenantID, _ := auth.TenantFromCtx(c.Request.Context())
		tok, err := auth.IssueJWT(deps.JWTBytes, uctx.ID, tenantID, 60*time.Second)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"token": tok, "expires_in": 60})
	}
}

// ChangePassword swaps the current user's bcrypt hash. Requires the
// current password as proof-of-identity even though the request is
// already authenticated — defense against session hijack + lost
// device. Mirrors GitHub/Stripe's "re-auth on sensitive action".
func ChangePassword(deps AuthDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		uctx, ok := auth.UserFromCtx(c.Request.Context())
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "not authenticated"})
			return
		}
		var req struct {
			CurrentPassword string `json:"current_password"`
			NewPassword     string `json:"new_password"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		// Pull full user to verify current password against the stored
		// hash — the ctx user only carries id/email.
		full, err := deps.Users.GetByID(c.Request.Context(), uctx.ID)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "user not found"})
			return
		}
		// Users created via OAuth-only have no password_hash. Allow
		// "set initial password" by skipping the verify step in that
		// case — surfaced clearly in the response so the UI can label
		// the form differently.
		isInitialSet := full.PasswordHash == ""
		if !isInitialSet {
			if !auth.PasswordVerify(req.CurrentPassword, full.PasswordHash) {
				c.JSON(http.StatusUnauthorized, gin.H{"error": "current password incorrect"})
				return
			}
		}
		newHash, err := auth.PasswordHash(req.NewPassword)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := deps.Users.UpdatePasswordHash(c.Request.Context(), uctx.ID, newHash); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		recordAudit(c, deps.Audit, mongodb.AuditPasswordChange, nil, map[string]any{"initial_set": isInitialSet})
		c.JSON(http.StatusOK, gin.H{"ok": true, "initial_set": isInitialSet})
	}
}

// UnlinkOAuth removes a provider link from the current user. Refuses
// when removing it would orphan the account (no password set + no
// other OAuth providers) — preventing accidental lockout.
func UnlinkOAuth(deps AuthDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		uctx, ok := auth.UserFromCtx(c.Request.Context())
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "not authenticated"})
			return
		}
		provider := c.Param("provider")
		if provider == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "provider required"})
			return
		}
		full, err := deps.Users.GetByID(c.Request.Context(), uctx.ID)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "user not found"})
			return
		}
		// Lockout guard: if user has no password AND only this one
		// provider linked, removing it leaves no way back in.
		hasPassword := full.PasswordHash != ""
		linkedCount := 0
		for _, link := range full.OAuthProviders {
			if link.Provider != "" {
				linkedCount++
			}
		}
		isOnlyProvider := linkedCount == 1 && len(full.OAuthProviders) > 0 && full.OAuthProviders[0].Provider == provider
		if !hasPassword && isOnlyProvider {
			c.JSON(http.StatusConflict, gin.H{
				"error": "cannot unlink the only sign-in method — set a password first",
			})
			return
		}
		if err := deps.Users.UnlinkOAuth(c.Request.Context(), uctx.ID, provider); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		recordAudit(c, deps.Audit, mongodb.AuditOAuthUnlinked, map[string]any{"provider": provider}, nil)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

// SwitchTenant re-issues the JWT with a different active tenant.
// Verifies the user is a member before issuing.
func SwitchTenant(deps AuthDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		uctx, ok := auth.UserFromCtx(c.Request.Context())
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "not authenticated"})
			return
		}
		if deps.Tenants == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "tenants not configured"})
			return
		}
		var req struct {
			TenantID string `json:"tenant_id"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := deps.Tenants.IsMember(c.Request.Context(), req.TenantID, uctx.ID); err != nil {
			c.JSON(http.StatusForbidden, gin.H{"error": "not a member of that tenant"})
			return
		}
		tok, err := auth.IssueJWT(deps.JWTBytes, uctx.ID, req.TenantID, deps.TTL)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		setAuthCookie(c, deps, tok)
		recordAudit(c, deps.Audit, mongodb.AuditTenantSwitch, map[string]any{"to_tenant_id": req.TenantID}, nil)
		c.JSON(http.StatusOK, gin.H{"tenant_id": req.TenantID, "token": tok})
	}
}
