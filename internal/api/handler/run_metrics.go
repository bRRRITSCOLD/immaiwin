// Run-level metrics endpoint — aggregate stats over the workflow_runs
// collection, scoped to the active tenant. Owner/admin-only since
// run counts + cost burn can leak business activity.
//
//	GET /api/v1/runs/metrics
//	  ?since=RFC3339   lower started_at bound (default: 30d ago)
//	  ?until=RFC3339   upper started_at bound (default: now)
//
// Response shape:
//   { since, until, total_runs, total_cost_usd,
//     by_status: [{status, count}, ...],
//     top_workflows: [{workflow_id, name, count, cost_usd}, ...] }
//
// The handler hydrates workflow names client-side via a single $in
// query so the UI doesn't N+1 on top_workflows.

package handler

import (
	"net/http"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/auth"
	"github.com/bRRRITSCOLD/burrow/internal/mongodb"
	"github.com/gin-gonic/gin"
)

// RunMetricsDeps wraps the repos this endpoint reads from.
type RunMetricsDeps struct {
	Runs      *mongodb.WorkflowRunRepository
	Tenants   *mongodb.TenantRepository
	Workflows *mongodb.WorkflowRepository
}

// hydratedWorkflowRollup adds a friendly name to each top-workflow row.
type hydratedWorkflowRollup struct {
	WorkflowID string  `json:"workflow_id"`
	Name       string  `json:"name"`
	Count      int64   `json:"count"`
	CostUSD    float64 `json:"cost_usd"`
}

// GetRunMetrics returns the aggregate dashboard payload.
func GetRunMetrics(deps RunMetricsDeps) gin.HandlerFunc {
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
		// Owner/admin gate. Non-admins see their own /runs view but
		// shouldn't aggregate spend across the team.
		role, err := deps.Tenants.GetMemberRole(c.Request.Context(), tenantID, uctx.ID)
		if err != nil {
			c.JSON(http.StatusForbidden, gin.H{"error": "not a member"})
			return
		}
		if role != mongodb.TenantRoleOwner && role != mongodb.TenantRoleAdmin {
			c.JSON(http.StatusForbidden, gin.H{"error": "owner/admin role required"})
			return
		}

		now := time.Now().UTC()
		filter := mongodb.RunMetricsFilter{
			TenantID: tenantID,
			Since:    now.Add(-30 * 24 * time.Hour),
			Until:    now,
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

		metrics, err := deps.Runs.AggregateMetrics(c.Request.Context(), filter)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		// Hydrate workflow names. Single ListForTenant call + map lookup.
		// At small dashboard volumes this is cheaper than N+1 GetByID.
		nameByID := map[string]string{}
		if deps.Workflows != nil {
			wfs, _ := deps.Workflows.ListForTenant(c.Request.Context(), tenantID)
			for _, wf := range wfs {
				nameByID[wf.ID] = wf.Name
			}
		}
		hydrated := make([]hydratedWorkflowRollup, 0, len(metrics.TopWorkflows))
		for _, w := range metrics.TopWorkflows {
			hydrated = append(hydrated, hydratedWorkflowRollup{
				WorkflowID: w.WorkflowID,
				Name:       nameByID[w.WorkflowID], // empty when workflow was deleted
				Count:      w.Count,
				CostUSD:    w.CostUSD,
			})
		}

		// Always emit arrays (not null) so the UI doesn't need null checks.
		byStatus := metrics.ByStatus
		if byStatus == nil {
			byStatus = []mongodb.StatusCount{}
		}

		c.JSON(http.StatusOK, gin.H{
			"since":          filter.Since,
			"until":          filter.Until,
			"total_runs":     metrics.TotalRuns,
			"total_cost_usd": metrics.TotalCostUSD,
			"by_status":      byStatus,
			"top_workflows":  hydrated,
		})
	}
}
