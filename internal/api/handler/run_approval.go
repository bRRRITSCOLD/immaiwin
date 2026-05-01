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
	"log/slog"
	"net/http"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/auth"
	"github.com/bRRRITSCOLD/burrow/internal/workflow"
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

		decision := workflow.ApprovalDecision{
			ToolCallID: req.ToolCallID,
			Approved:   req.Approved,
			Reason:     req.Reason,
		}
		if applied, derr := applyDecisionAcrossPaths(c, exec, runStore, rec, decision); derr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": derr.Error()})
			return
		} else if !applied {
			cancelOrphanedApproval(c, runStore, rec)
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

// applyDecisionAcrossPaths routes a decision to the right resume
// mechanism based on whether the run is on the legacy in-process
// approval-channel path (block-on-Redis) or the new lease-yield path
// (decision persisted into ExecutionState.Pending; worker claims
// post-wakeup).
//
// Returns (true, nil) when the decision landed somewhere a worker can
// pick up. Returns (false, nil) only on the legacy path when no
// in-process listener exists — caller treats that as orphan + 410.
// I/O errors surface as (false, err).
//
// Yield-path detection: a non-nil ExecutionState.Pending on the run
// record means the executor yielded its lease + persisted the gate.
// The decision goes through ApplyApprovalDecision (writes into state
// + clears lease) + a wakeup publish; legacy Submit is skipped (no
// live waiter would ever see it).
func applyDecisionAcrossPaths(c *gin.Context, exec *workflow.WorkflowExecutor, runStore workflow.WorkflowRunStore, rec workflow.WorkflowRun, decision workflow.ApprovalDecision) (bool, error) {
	ctx := c.Request.Context()
	if rec.ExecutionState != nil && rec.ExecutionState.Pending != nil {
		// Yield path. Persist decision into state, publish wakeup.
		// Skip the legacy Submit — there's no in-process subscriber
		// for this run (the worker released its lease + exited the
		// BFS loop on yield).
		if err := runStore.ApplyApprovalDecision(ctx, rec.ID, decision); err != nil {
			return false, err
		}
		if exec != nil && exec.ApprovalBroker != nil {
			_, _ = exec.ApprovalBroker.PublishWithCount(ctx, workflow.WakeupChannel, []byte("1"))
		}
		return true, nil
	}
	// Legacy in-process path: publish on the per-run channel; the
	// agent loop's goroutine pumps it into the local approveCh. A
	// zero-subscriber publish means the api process restarted while
	// the run was paused — caller treats that as orphan.
	reg := workflow.NewApprovalRegistry(exec.ApprovalBroker)
	count, perr := reg.Submit(ctx, rec.ID, decision)
	if perr != nil {
		return false, perr
	}
	return count > 0, nil
}

// cancelOrphanedApproval marks a pending_approval run as errored when
// no Redis subscribers received the approval publish — symptom of an
// API process restart between gate-fire and user click. The
// in-process goroutine that held the wait is gone, so the decision
// has nowhere to land. Returns 410 Gone with an explanatory message
// + auto_cancelled=true so the UI can re-render terminal state and
// nudge the user to re-trigger.
//
// Shared by both the cookie-authed `/approval` endpoint and the
// magic-link `/approval/redeem` endpoint — both fail identically when
// the waiter is gone.
func cancelOrphanedApproval(c *gin.Context, runStore workflow.WorkflowRunStore, rec workflow.WorkflowRun) {
	rec.Status = workflow.RunStatusError
	rec.Error = "approval orphaned: api process restarted while paused; run cannot be resumed (re-trigger the workflow to retry)"
	rec.PendingApproval = nil
	now := time.Now().UTC()
	rec.FinishedAt = &now
	if uerr := runStore.Update(c.Request.Context(), rec); uerr != nil {
		slog.Warn("approval: persist orphan cancellation failed", "run_id", rec.ID, "err", uerr)
	}
	c.JSON(http.StatusGone, gin.H{
		"error":          "no live approval waiter — run was abandoned (api restarted while paused). Re-trigger the workflow to retry.",
		"auto_cancelled": true,
	})
}
