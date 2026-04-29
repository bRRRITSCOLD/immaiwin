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
// the server level is one struct, not five separate args.
type AuthDeps struct {
	Users    *mongodb.UserRepository
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
		// Phase B: also create a personal tenant + add as owner here,
		// then issue JWT with tenant_id populated. For now, JWT carries
		// only user_id.
		tok, err := auth.IssueJWT(deps.JWTBytes, u.ID, "", deps.TTL)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		setAuthCookie(c, deps, tok)
		c.JSON(http.StatusOK, gin.H{
			"user":  u,
			"token": tok,
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
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid email or password"})
			return
		}
		_ = deps.Users.TouchLastLogin(c.Request.Context(), u.ID)
		tok, err := auth.IssueJWT(deps.JWTBytes, u.ID, "", deps.TTL)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		setAuthCookie(c, deps, tok)
		c.JSON(http.StatusOK, gin.H{
			"user":  u,
			"token": tok,
		})
	}
}

// Logout clears the cookie. Bearer-token clients can also call this
// for symmetry — it's a no-op server-side beyond the Set-Cookie
// header. (Token revocation list deferred to backlog.)
func Logout(deps AuthDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
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
		c.JSON(http.StatusOK, gin.H{
			"user":      full,
			"tenant_id": tenantID,
		})
	}
}
