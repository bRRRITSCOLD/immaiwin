// Canvas Continue + set_breakpoints control bridge — REST endpoints
// that publish RunControlMessages onto `burrow:run_control:<runID>`
// for the worker that holds the run's lease to consume. The old WS
// frame path was severed when the canvas-WS / lease unification
// landed (the WS handler is now a pure event subscriber); this is
// the documented Phase-2 follow-up.
//
//	POST /api/v1/workflow_runs/:id/continue
//	PUT  /api/v1/workflow_runs/:id/breakpoints   { "node_ids": ["a","b"] }
//
// Tenant-gated. Cross-tenant access reads as 404 to avoid leaking
// run-id existence (same posture as /cancel + /approval).
//
// Publish is best-effort: a zero-subscriber publish means the
// worker that held the lease died or the run finished; either way
// nothing happens. We still return 200 because there's no graceful
// "no listener" semantics the UI could act on differently from
// "listener silently ignored". The next /runs/:id poll will show
// the actual state.

package handler

import (
	"encoding/json"
	"net/http"

	"github.com/bRRRITSCOLD/burrow/internal/auth"
	"github.com/bRRRITSCOLD/burrow/internal/workflow"
	"github.com/gin-gonic/gin"
)

// ContinueRun releases a paused pre-exec breakpoint by publishing
// a `{type:"continue"}` control message on the run's control
// channel. The worker's bridge goroutine wakes the runEnv's
// continueCh; the BFS then proceeds past the breakpoint.
func ContinueRun(exec *workflow.WorkflowExecutor, runStore workflow.WorkflowRunStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		runID := c.Param("id")
		if runID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
			return
		}
		if runStore == nil || exec == nil || exec.ApprovalBroker == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "run control not configured"})
			return
		}
		rec, err := runStore.Get(c.Request.Context(), runID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "run not found"})
			return
		}
		if tenantID, ok := auth.TenantFromCtx(c.Request.Context()); ok {
			if rec.TenantID != tenantID {
				c.JSON(http.StatusNotFound, gin.H{"error": "run not found"})
				return
			}
		}
		// Terminal runs have no worker to nudge; refuse so a stale
		// UI tab doesn't endlessly POST /continue after the run
		// already finished.
		switch rec.Status {
		case workflow.RunStatusSuccess,
			workflow.RunStatusError,
			workflow.RunStatusCancelled:
			c.JSON(http.StatusBadRequest, gin.H{"error": "run already terminal: " + string(rec.Status)})
			return
		}

		payload, _ := json.Marshal(workflow.RunControlMessage{Type: "continue"})
		count, perr := exec.ApprovalBroker.PublishWithCount(c.Request.Context(),
			workflow.RunControlChannel(runID), payload)
		if perr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": perr.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true, "delivered_to": count})
	}
}

// SetRunBreakpoints replaces the run's breakpoint set with the
// supplied node IDs. Empty list clears all breakpoints. Publishes
// a `{type:"set_breakpoints", node_ids: [...]}` control message
// on the run's control channel; the worker's bridge mutates the
// runEnv's stopAtSet which subsequent BFS steps check.
func SetRunBreakpoints(exec *workflow.WorkflowExecutor, runStore workflow.WorkflowRunStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		runID := c.Param("id")
		if runID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
			return
		}
		if runStore == nil || exec == nil || exec.ApprovalBroker == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "run control not configured"})
			return
		}
		var body struct {
			NodeIDs []string `json:"node_ids"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON: " + err.Error()})
			return
		}

		rec, err := runStore.Get(c.Request.Context(), runID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "run not found"})
			return
		}
		if tenantID, ok := auth.TenantFromCtx(c.Request.Context()); ok {
			if rec.TenantID != tenantID {
				c.JSON(http.StatusNotFound, gin.H{"error": "run not found"})
				return
			}
		}
		switch rec.Status {
		case workflow.RunStatusSuccess,
			workflow.RunStatusError,
			workflow.RunStatusCancelled:
			c.JSON(http.StatusBadRequest, gin.H{"error": "run already terminal: " + string(rec.Status)})
			return
		}

		payload, _ := json.Marshal(workflow.RunControlMessage{
			Type:    "set_breakpoints",
			NodeIDs: body.NodeIDs,
		})
		count, perr := exec.ApprovalBroker.PublishWithCount(c.Request.Context(),
			workflow.RunControlChannel(runID), payload)
		if perr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": perr.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true, "delivered_to": count, "node_ids": body.NodeIDs})
	}
}
