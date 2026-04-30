// Audit log read endpoint — surfaces what handlers stamped via the
// recordAudit helpers. Owner/admin-only on the active tenant since
// audit entries can leak member emails + action timing.
//
//	GET /api/v1/audit_log
//	  ?action=...    filter to one action (optional)
//	  ?since=RFC3339 lower bound on ts
//	  ?until=RFC3339 upper bound on ts
//	  ?limit=N       page size, capped server-side
//	  ?skip=N        offset for pagination

package handler

import (
	"net/http"
	"strconv"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/auth"
	"github.com/bRRRITSCOLD/burrow/internal/mongodb"
	"github.com/gin-gonic/gin"
)

// AuditLogDeps wraps the audit repo + tenants repo (for the role check).
type AuditLogDeps struct {
	Audit   *mongodb.AuditRepository
	Tenants *mongodb.TenantRepository
}

// ListAuditLog returns audit entries for the caller's active tenant.
// Always scopes to ctx tenant — there's no admin path to view all
// tenants (intentionally; cross-tenant audit is a future ops feature
// that needs a dedicated super-admin role).
func ListAuditLog(deps AuditLogDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		uctx, ok := auth.UserFromCtx(c.Request.Context())
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "not authenticated"})
			return
		}
		tenantID, ok := auth.TenantFromCtx(c.Request.Context())
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "no active tenant"})
			return
		}
		if deps.Audit == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "audit not configured"})
			return
		}
		// Owner/admin gate. Members shouldn't see who-did-what.
		role, err := deps.Tenants.GetMemberRole(c.Request.Context(), tenantID, uctx.ID)
		if err != nil {
			c.JSON(http.StatusForbidden, gin.H{"error": "not a member"})
			return
		}
		if role != mongodb.TenantRoleOwner && role != mongodb.TenantRoleAdmin {
			c.JSON(http.StatusForbidden, gin.H{"error": "owner/admin role required"})
			return
		}

		filter := mongodb.AuditFilter{TenantID: tenantID}
		if a := c.Query("action"); a != "" {
			filter.Action = mongodb.AuditAction(a)
		}
		if v := c.Query("since"); v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				filter.Since = t
			}
		}
		if v := c.Query("until"); v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				filter.Until = t
			}
		}
		if v := c.Query("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				filter.Limit = n
			}
		}
		if v := c.Query("skip"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				filter.Skip = n
			}
		}

		entries, err := deps.Audit.ListWithFilter(c.Request.Context(), filter)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if entries == nil {
			entries = []mongodb.AuditEntry{}
		}
		c.JSON(http.StatusOK, gin.H{"entries": entries})
	}
}
