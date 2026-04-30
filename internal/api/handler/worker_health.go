// Worker health endpoint — surfaces heartbeats written by the worker
// registry so ops dashboards + alerting can spot stuck/errored workers
// without grepping logs across pids.
//
//	GET /api/v1/workers/health
//
// Returns the full set of worker_health rows, sorted by last_heartbeat
// descending. Auth-gated (RequireAuth) since worker names + error
// strings can leak internal architecture.
//
// Stale-detection is intentionally client-side: the row carries a
// status field + last_heartbeat timestamp, and the caller decides
// what "stale" means. (Different deployments tolerate different
// heartbeat gaps — a webhook worker that fires on demand may legitimately
// idle for hours; cron sweepers should never miss a tick.)

package handler

import (
	"net/http"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/mongodb"
	"github.com/gin-gonic/gin"
)

// WorkerHealthDeps is the handler's repo dependency.
type WorkerHealthDeps struct {
	Health *mongodb.WorkerHealthRepository
}

// ListWorkerHealth returns all worker heartbeat rows.
func ListWorkerHealth(deps WorkerHealthDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps.Health == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "worker health not configured"})
			return
		}
		rows, err := deps.Health.List(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		// Always emit an array — null-safe for callers iterating
		// `rows.map(...)` in the UI.
		if rows == nil {
			rows = []mongodb.WorkerHealth{}
		}
		c.JSON(http.StatusOK, gin.H{"workers": rows})
	}
}
