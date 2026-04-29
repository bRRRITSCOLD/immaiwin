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
	"net/http"
	"strings"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/auth"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/mongodb"
	"github.com/gin-gonic/gin"
)

// authCookieName must match handler.authCookieName. Duplicated here
// to avoid the middleware importing the handler package (cycle).
const authCookieName = "immaiwin_auth"

// extractToken pulls the JWT from either the auth cookie or the
// Authorization: Bearer header, in that order. Cookie wins because
// it's harder to leak via accidental logging.
func extractToken(c *gin.Context) string {
	if cookie, err := c.Cookie(authCookieName); err == nil && cookie != "" {
		return cookie
	}
	hdr := c.GetHeader("Authorization")
	if strings.HasPrefix(hdr, "Bearer ") {
		return strings.TrimPrefix(hdr, "Bearer ")
	}
	return ""
}

// RequireAuth enforces a valid JWT on every request. Hands off the
// authenticated user/tenant to the handler via ctx.
func RequireAuth(jwtSecret []byte, users *mongodb.UserRepository) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := extractToken(c)
		if raw == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		claims, err := auth.ParseJWT(jwtSecret, raw)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token: " + err.Error()})
			return
		}
		// Look up the user so we have a stable email / can detect
		// account deletion mid-session. One DB hit per request — fine
		// at our current scale; cache later if it becomes hot.
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

// OptionalAuth tags the ctx when a valid token is present, otherwise
// lets the request through unauthenticated. No 401s.
func OptionalAuth(jwtSecret []byte, users *mongodb.UserRepository) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := extractToken(c)
		if raw == "" {
			c.Next()
			return
		}
		claims, err := auth.ParseJWT(jwtSecret, raw)
		if err != nil {
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
