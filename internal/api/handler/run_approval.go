// Out-of-band run-approval endpoint — completes the require_approval
// gate when no live WS is connected (cron / event-trigger / future
// email / Slack approval-link path).
//
//	POST /api/v1/workflow_runs/:id/approval
//	  body: {"tool_call_id": "...", "approved": true|false, "reason": "..."}
//
// Cross-process: agent runs in the worker pid; this endpoint runs in
// the api pid. Both share Redis pub/sub via the executor's
// ApprovalBroker, so a publish here lands on the worker-side
// subscription. The handler pre-checks Mongo for the run's
// `pending_approval` status — without a live registry we can't tell
// from the producer side whether anyone is listening, so the Mongo
// check is the source of truth for "is this run actually waiting?".

package handler

import (
	"net/http"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/auth"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/workflow"
	"github.com/gin-gonic/gin"
)

// SubmitRunApproval routes a user verdict into the executor's approval
// registry. The executor must be wired with an ApprovalBroker (Redis-
// backed); the run record's status must be `pending_approval` for the
// publish to make sense (otherwise we'd silently drop the message
// onto a channel no one's listening on).
func SubmitRunApproval(exec *workflow.WorkflowExecutor, runStore workflow.WorkflowRunStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		runID := c.Param("id")
		if runID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
			return
		}
		var req struct {
			ToolCallID string `json:"tool_call_id"`
			Approved   bool   `json:"approved"`
			Reason     string `json:"reason,omitempty"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if exec == nil || exec.ApprovalBroker == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "approval broker not configured"})
			return
		}
		if runStore == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "run store not configured"})
			return
		}
		// Pre-check Mongo: only publish if the run is genuinely
		// waiting on an approval. Avoids feeding decisions into
		// channels no one is reading and lets the UI get a 404 for
		// stale clicks (e.g. clicked Approve, hit refresh, clicked
		// again after the agent already resumed).
		rec, err := runStore.Get(c.Request.Context(), runID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "run not found"})
			return
		}
		// Tenant ownership: a run from another tenant must look like it
		// doesn't exist (404, not 403) so the endpoint can't be used to
		// probe for run-id existence across tenants.
		if tenantID, ok := auth.TenantFromCtx(c.Request.Context()); ok {
			if rec.TenantID != tenantID {
				c.JSON(http.StatusNotFound, gin.H{"error": "run not found"})
				return
			}
		}
		if rec.Status != workflow.RunStatusPendingApproval || rec.PendingApproval == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "no pending approval for this run"})
			return
		}

		// Need the registry shape that exposes Submit. Lazy-init via
		// the executor so the api process has its own registry too.
		// Even though only the worker pid registers a subscription,
		// Submit just publishes — registering a sub here would be a
		// waste, so we go directly through the broker.
		decision := workflow.ApprovalDecision{
			ToolCallID: req.ToolCallID,
			Approved:   req.Approved,
			Reason:     req.Reason,
		}
		// Reuse the registry's Submit so the channel/payload format
		// stays identical to what the worker subscribed to. The api-
		// side registry never holds local state beyond this call.
		reg := workflow.NewApprovalRegistry(exec.ApprovalBroker)
		if perr := reg.Submit(c.Request.Context(), runID, decision); perr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": perr.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}
