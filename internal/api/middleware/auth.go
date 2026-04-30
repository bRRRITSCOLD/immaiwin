// Auth middleware — extracts the JWT (cookie or Bearer header),
// validates, looks up the user, and stamps user/tenant onto the
// request ctx so downstream handlers can read via auth.UserFromCtx /
// auth.TenantFromCtx.
//
// Two modes:
//   - RequireAuth: rejects unauthenticated requests with 401. Use
//     for everything that touches user data.
//   - OptionalAuth: tags the ctx when a valid token is present,
//     otherwise lets the request through unauthenticated. Use for
//     endpoints that gate behavior conditionally (e.g. "show user
//     menu vs login button" probes).

package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/bRRRITSCOLD/burrow/internal/auth"
	"github.com/bRRRITSCOLD/burrow/internal/mongodb"
	"github.com/gin-gonic/gin"
)

// authCookieName must match handler.authCookieName. Duplicated here
// to avoid the middleware importing the handler package (cycle).
const authCookieName = "burrow_auth"

// extractToken pulls the auth token from (in order) the auth cookie,
// Authorization: Bearer header, or the `?token=<...>` query string.
// Returns the raw value + a flag indicating whether it's an API key
// (`iwk_<...>`) or a JWT.
//
// The query-string fallback exists so WebSocket clients can pass a
// token at upgrade time — the browser WebSocket API doesn't allow
// custom request headers, and httpOnly auth cookies don't always
// travel cross-origin on the upgrade handshake. UI obtains a short-
// lived JWT via POST /api/v1/auth/ws_token and tacks it onto the
// WS URL.
func extractToken(c *gin.Context) (raw string, isAPIKey bool) {
	if cookie, err := c.Cookie(authCookieName); err == nil && cookie != "" {
		return cookie, false
	}
	hdr := c.GetHeader("Authorization")
	if strings.HasPrefix(hdr, "Bearer ") {
		raw = strings.TrimPrefix(hdr, "Bearer ")
		return raw, strings.HasPrefix(raw, mongodb.APIKeyPrefix)
	}
	if q := c.Query("token"); q != "" {
		return q, strings.HasPrefix(q, mongodb.APIKeyPrefix)
	}
	return "", false
}

// resolveAPIKey looks up the API key + injects the matching user +
// tenant into ctx. Returns the new ctx + nil on success, or empty
// ctx + error on miss.
func resolveAPIKey(c *gin.Context, raw string, users *mongodb.UserRepository, keys *mongodb.APIKeyRepository) (context.Context, error) {
	if keys == nil {
		return nil, errAPIKeysNotConfigured
	}
	k, err := keys.LookupByRaw(c.Request.Context(), raw)
	if err != nil {
		return nil, err
	}
	u, err := users.GetByID(c.Request.Context(), k.UserID)
	if err != nil {
		return nil, err
	}
	go keys.TouchLastUsed(c.Request.Context(), k.ID) // best-effort
	ctx := auth.WithUser(c.Request.Context(), auth.UserCtx{ID: u.ID, Email: u.Email})
	if k.TenantID != "" {
		ctx = auth.WithTenant(ctx, k.TenantID)
	}
	return ctx, nil
}

var errAPIKeysNotConfigured = errors.New("api keys not configured")

// RequireAuth enforces a valid JWT or API key on every request.
// Hands off the authenticated user/tenant to the handler via ctx.
func RequireAuth(jwtSecret []byte, users *mongodb.UserRepository, keys *mongodb.APIKeyRepository) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, isAPIKey := extractToken(c)
		if raw == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		if isAPIKey {
			ctx, err := resolveAPIKey(c, raw, users, keys)
			if err != nil {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid api key"})
				return
			}
			c.Request = c.Request.WithContext(ctx)
			c.Next()
			return
		}
		claims, err := auth.ParseJWT(jwtSecret, raw)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token: " + err.Error()})
			return
		}
		// Special-purpose tokens (password_reset, invite, etc) MUST NOT
		// authenticate a session — otherwise a leaked reset link
		// becomes a permanent session.
		if claims.Purpose != "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "token not valid for session use"})
			return
		}
		u, err := users.GetByID(c.Request.Context(), claims.UserID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "user not found"})
			return
		}
		ctx := auth.WithUser(c.Request.Context(), auth.UserCtx{ID: u.ID, Email: u.Email})
		if claims.TenantID != "" {
			ctx = auth.WithTenant(ctx, claims.TenantID)
		}
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// OptionalAuth tags the ctx when a valid JWT or API key is present;
// otherwise lets the request through unauthenticated. No 401s.
func OptionalAuth(jwtSecret []byte, users *mongodb.UserRepository, keys *mongodb.APIKeyRepository) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, isAPIKey := extractToken(c)
		if raw == "" {
			c.Next()
			return
		}
		if isAPIKey {
			ctx, err := resolveAPIKey(c, raw, users, keys)
			if err == nil {
				c.Request = c.Request.WithContext(ctx)
			}
			c.Next()
			return
		}
		claims, err := auth.ParseJWT(jwtSecret, raw)
		if err != nil {
			c.Next()
			return
		}
		if claims.Purpose != "" {
			// Purpose-bound tokens never count as a session; pass through unauth.
			c.Next()
			return
		}
		u, err := users.GetByID(c.Request.Context(), claims.UserID)
		if err != nil {
			c.Next()
			return
		}
		ctx := auth.WithUser(c.Request.Context(), auth.UserCtx{ID: u.ID, Email: u.Email})
		if claims.TenantID != "" {
			ctx = auth.WithTenant(ctx, claims.TenantID)
		}
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
