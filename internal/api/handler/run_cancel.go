// Manual run-cancel endpoint — user safety valve for runs stuck in
// non-terminal state (running / pending_approval / paused) due to
// worker crash, network drop, abandoned approval, etc.
//
//	POST /api/v1/workflow_runs/:id/cancel
//
// Flips the run record to status=cancelled with an explanatory error,
// clears any pending_approval state, and publishes a synthetic
// "rejected" decision onto the OOB approval channel so any agent
// waiting on this run unblocks if it's still alive somewhere. The
// publish is best-effort — if no subscriber is listening (the typical
// "stuck after worker died" case), the message just drops.

package handler

import (
	"net/http"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/workflow"
	"github.com/gin-gonic/gin"
)

// CancelRun marks a non-terminal run as cancelled.
func CancelRun(exec *workflow.WorkflowExecutor, runStore workflow.WorkflowRunStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		runID := c.Param("id")
		if runID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
			return
		}
		if runStore == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "run store not configured"})
			return
		}
		rec, err := runStore.Get(c.Request.Context(), runID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "run not found"})
			return
		}
		switch rec.Status {
		case workflow.RunStatusSuccess,
			workflow.RunStatusError,
			workflow.RunStatusCancelled:
			c.JSON(http.StatusBadRequest, gin.H{"error": "run already terminal: " + string(rec.Status)})
			return
		}

		// Best-effort: nudge any live waiter unblocked. If the run is
		// in pending_approval, a "rejected" decision unblocks the
		// agent's gate — the agent then writes its own terminal
		// status. We still write `cancelled` here as a safety net for
		// the dead-worker case.
		if rec.Status == workflow.RunStatusPendingApproval && exec != nil && exec.ApprovalBroker != nil {
			reg := workflow.NewApprovalRegistry(exec.ApprovalBroker)
			_ = reg.Submit(c.Request.Context(), runID, workflow.ApprovalDecision{
				Approved: false,
				Reason:   "force-cancelled by user",
			})
		}

		rec.Status = workflow.RunStatusCancelled
		now := time.Now().UTC()
		rec.FinishedAt = &now
		rec.PendingApproval = nil
		if rec.Error == "" {
			rec.Error = "force-cancelled by user"
		}
		if uerr := runStore.Update(c.Request.Context(), rec); uerr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": uerr.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true, "status": string(rec.Status)})
	}
}
