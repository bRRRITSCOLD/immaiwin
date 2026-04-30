// API key endpoints — list, create, revoke. Routes are RequireAuth
// since key management is a per-user concern.
//
//	GET    /api/v1/api_keys           list user's keys (no raw values)
//	POST   /api/v1/api_keys           create — returns raw value ONCE
//	DELETE /api/v1/api_keys/:id       revoke
//
// The raw key value is shown in the create response only and never
// persisted in plaintext; lose it = revoke + create new.

package handler

import (
	"errors"
	"net/http"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/auth"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/mongodb"
	"github.com/gin-gonic/gin"
	"github.com/oklog/ulid/v2"
)

// APIKeyDeps wraps the api_key repo for handler injection.
type APIKeyDeps struct {
	Keys *mongodb.APIKeyRepository
}

// ListAPIKeys returns every key the caller has issued. Hashes never
// returned — only id, name, prefix (so users can identify in their
// .env files), created_at, last_used_at.
func ListAPIKeys(deps APIKeyDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		u, ok := auth.UserFromCtx(c.Request.Context())
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "not authenticated"})
			return
		}
		if deps.Keys == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "api keys not configured"})
			return
		}
		keys, err := deps.Keys.ListForUser(c.Request.Context(), u.ID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if keys == nil {
			keys = []mongodb.APIKey{}
		}
		c.JSON(http.StatusOK, keys)
	}
}

// CreateAPIKey mints a new key for the active tenant. Returns the
// full raw value ONCE — caller's responsibility to copy it. Body:
// {"name": "<friendly label>"}.
func CreateAPIKey(deps APIKeyDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		u, ok := auth.UserFromCtx(c.Request.Context())
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "not authenticated"})
			return
		}
		tenantID, hasTenant := auth.TenantFromCtx(c.Request.Context())
		if !hasTenant {
			c.JSON(http.StatusBadRequest, gin.H{"error": "no active tenant on this session"})
			return
		}
		if deps.Keys == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "api keys not configured"})
			return
		}
		var req struct {
			Name string `json:"name"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if req.Name == "" {
			req.Name = "untitled"
		}
		raw, err := mongodb.GenerateKey()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		k := mongodb.APIKey{
			ID:       ulid.Make().String(),
			UserID:   u.ID,
			TenantID: tenantID,
			Name:     req.Name,
		}
		saved, err := deps.Keys.Create(c.Request.Context(), k, raw)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		// Surface the raw value ONLY here. Subsequent ListAPIKeys
		// calls return only the prefix + hash-derived metadata.
		c.JSON(http.StatusOK, gin.H{
			"key": saved,
			"raw": raw,
			"warning": "copy this value now — it will never be shown again",
		})
	}
}

// RevokeAPIKey deletes a key by ID. Scoped to the owning user.
func RevokeAPIKey(deps APIKeyDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		u, ok := auth.UserFromCtx(c.Request.Context())
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "not authenticated"})
			return
		}
		if deps.Keys == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "api keys not configured"})
			return
		}
		id := c.Param("id")
		if err := deps.Keys.Revoke(c.Request.Context(), id, u.ID); err != nil {
			if errors.Is(err, mongodb.ErrAPIKeyNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "key not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.Status(http.StatusNoContent)
	}
}
