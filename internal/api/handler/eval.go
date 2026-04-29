// Eval HTTP surface.
//
// Routes (server.go wires these in):
//
//	GET    /api/v1/evals?workflow_id=<id>     list evals (optionally per workflow)
//	PUT    /api/v1/evals/:id                  upsert eval definition
//	GET    /api/v1/evals/:id                  get one eval
//	DELETE /api/v1/evals/:id                  delete eval
//	POST   /api/v1/evals/:id/run              kick off an EvalRun, returns it
//	GET    /api/v1/eval_runs/:id              fetch a completed run
//	GET    /api/v1/eval_runs?eval_id=<id>     list runs for an eval (latest 50)

package handler

import (
	"net/http"
	"strconv"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/workflow"
	"github.com/gin-gonic/gin"
)

// EvalDeps is the small bundle of stores + runner the eval handlers need.
// Keeping it as a value reduces server.go arg count from 3 → 1 per route.
type EvalDeps struct {
	Store  workflow.EvalStore
	Runner *workflow.EvalRunner
}

// ListEvals serves GET /api/v1/evals.
func ListEvals(deps EvalDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps.Store == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "evals not configured"})
			return
		}
		out, err := deps.Store.ListEvals(c.Request.Context(), c.Query("workflow_id"))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if out == nil {
			out = []workflow.Eval{}
		}
		c.JSON(http.StatusOK, out)
	}
}

// UpsertEval serves PUT /api/v1/evals/:id.
func UpsertEval(deps EvalDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps.Store == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "evals not configured"})
			return
		}
		id := c.Param("id")
		var eval workflow.Eval
		if err := c.ShouldBindJSON(&eval); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		eval.ID = id
		if eval.WorkflowID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "workflow_id is required"})
			return
		}
		if eval.Name == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
			return
		}
		saved, err := deps.Store.UpsertEval(c.Request.Context(), eval)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, saved)
	}
}

// GetEval serves GET /api/v1/evals/:id.
func GetEval(deps EvalDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps.Store == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "evals not configured"})
			return
		}
		eval, err := deps.Store.GetEval(c.Request.Context(), c.Param("id"))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, eval)
	}
}

// DeleteEval serves DELETE /api/v1/evals/:id.
func DeleteEval(deps EvalDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps.Store == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "evals not configured"})
			return
		}
		if err := deps.Store.DeleteEval(c.Request.Context(), c.Param("id")); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// RunEval serves POST /api/v1/evals/:id/run. Synchronous — blocks the
// request until every case completes. Acceptable for evals up to ~50
// cases at a few seconds each; long evals would warrant the WS-streaming
// pattern from RunWorkflowWS, but that's Tier-D scope.
func RunEval(deps EvalDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps.Store == nil || deps.Runner == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "evals not configured"})
			return
		}
		eval, err := deps.Store.GetEval(c.Request.Context(), c.Param("id"))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		run, err := deps.Runner.Run(c.Request.Context(), eval)
		if err != nil {
			// Surface the error AND any partial run record we got back so
			// the UI can render something actionable instead of a bare
			// 500.
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": err.Error(),
				"run":   run,
			})
			return
		}
		c.JSON(http.StatusOK, run)
	}
}

// GetEvalRun serves GET /api/v1/eval_runs/:id.
func GetEvalRun(deps EvalDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps.Store == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "evals not configured"})
			return
		}
		run, err := deps.Store.GetEvalRun(c.Request.Context(), c.Param("id"))
		if err != nil {
			// Mongo layer returns errors.New("eval run not found") which
			// allocates a fresh sentinel each call — errors.Is can't match
			// it. Fall back to message compare for the not-found path.
			if err.Error() == "eval run not found" {
				c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, run)
	}
}

// ListEvalRuns serves GET /api/v1/eval_runs?eval_id=<id>&limit=<n>.
func ListEvalRuns(deps EvalDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps.Store == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "evals not configured"})
			return
		}
		evalID := c.Query("eval_id")
		if evalID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "eval_id required"})
			return
		}
		limit := 0
		if v := c.Query("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				limit = n
			}
		}
		out, err := deps.Store.ListEvalRuns(c.Request.Context(), evalID, limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if out == nil {
			out = []workflow.EvalRun{}
		}
		c.JSON(http.StatusOK, out)
	}
}
