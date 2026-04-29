// Workflow run history endpoints — back the `/runs` table + detail UI.
//
//	GET /api/v1/workflow_runs
//	    ?workflow_id=<id>           filter to one workflow (optional)
//	    &status=<running|success|error|cancelled|paused>  (optional)
//	    &started_after=<RFC3339>    (optional)
//	    &started_before=<RFC3339>   (optional)
//	    &limit=<n>                  default 50, max 200
//	    &skip=<n>                   pagination offset, default 0
//
//	GET /api/v1/workflow_runs/:id   single run with full traces
//
// Both endpoints return the persisted `WorkflowRun` shape (see
// internal/workflow/run.go). The detail handler also stitches in the
// referenced workflow document so the UI can render node names + types
// without a follow-up RPC.

package handler

import (
	"net/http"
	"strconv"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/workflow"
	"github.com/gin-gonic/gin"
)

// ListWorkflowRuns serves the runs history table.
func ListWorkflowRuns(store workflow.WorkflowRunStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		if store == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "workflow runs not configured"})
			return
		}
		filter := workflow.RunFilter{
			WorkflowID: c.Query("workflow_id"),
			Status:     workflow.RunStatus(c.Query("status")),
		}
		if v := c.Query("started_after"); v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				filter.StartedAfter = t
			}
		}
		if v := c.Query("started_before"); v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				filter.StartedBefore = t
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

		runs, err := store.ListWithFilter(c.Request.Context(), filter)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if runs == nil {
			runs = []workflow.WorkflowRun{}
		}
		c.JSON(http.StatusOK, runs)
	}
}

// DailyTotal serves the per-workflow daily-spend chip on /runs.
//
//	GET /api/v1/workflow_runs/daily_total?workflow_id=<id>
//	→ { workflow_id, spent_usd, cap_usd, limit_pct, since }
//
// `cap_usd` is the workflow's `CostLimits.MaxDailyUSD` (0 when no cap).
// `limit_pct` is `100 * spent / cap` rounded to 2 decimals (omitted when
// cap is 0). `since` is the start of UTC today — same boundary the agent
// loop's pre-call gate uses, so the chip can't disagree with what the
// executor enforces.
func DailyTotal(store workflow.WorkflowRunStore, wfStore WorkflowStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		if store == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "workflow runs not configured"})
			return
		}
		workflowID := c.Query("workflow_id")
		if workflowID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "workflow_id required"})
			return
		}

		// Start-of-UTC-day boundary mirrors `executor.startOfUTCDay`. We
		// recompute here rather than exporting it; the cap chip drifts at
		// most a millisecond from the executor's check, which is within
		// the slop window for "today's spend."
		now := time.Now().UTC()
		since := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

		spent, err := store.SumCostSince(c.Request.Context(), workflowID, since)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		// Pull the cap from the workflow doc when available. Best-effort —
		// missing workflow returns spent-only (cap=0, no limit shown).
		var capUSD float64
		if wfStore != nil {
			if wf, gerr := wfStore.GetByID(c.Request.Context(), workflowID); gerr == nil {
				if wf.CostLimits != nil {
					capUSD = wf.CostLimits.MaxDailyUSD
				}
			}
		}

		resp := gin.H{
			"workflow_id": workflowID,
			"spent_usd":   spent,
			"cap_usd":     capUSD,
			"since":       since.Format(time.RFC3339),
		}
		if capUSD > 0 {
			pct := 100.0 * spent / capUSD
			// Round to 2 decimals so the chip text doesn't show "73.4719281…".
			resp["limit_pct"] = float64(int(pct*100)) / 100.0
		}

		c.JSON(http.StatusOK, resp)
	}
}

// DailyTotals serves the cross-workflow rollup the /runs page renders
// when the filter is "All workflows": one row per workflow + an aggregate
// total. Driven by the same `SumCostSince` aggregation as the per-
// workflow chip, so caps + spend stay coherent across views.
//
//	GET /api/v1/workflow_runs/daily_totals
//	→ {
//	    since:    "2026-04-29T00:00:00Z",
//	    total:    { spent_usd, run_count },
//	    by_workflow: [
//	      { workflow_id, name, spent_usd, cap_usd, limit_pct? },
//	      ...
//	    ],
//	  }
//
// Iterates the workflow list once + issues one SumCostSince per workflow.
// O(N_workflows) DB calls; fine for the current scale, swap for a single
// `$match`-then-`$group` Mongo aggregation once N grows past ~50.
func DailyTotals(store workflow.WorkflowRunStore, wfStore WorkflowStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		if store == nil || wfStore == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "workflow runs not configured"})
			return
		}

		now := time.Now().UTC()
		since := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

		wfs, err := wfStore.List(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		type byWorkflow struct {
			WorkflowID string  `json:"workflow_id"`
			Name       string  `json:"name"`
			SpentUSD   float64 `json:"spent_usd"`
			CapUSD     float64 `json:"cap_usd"`
			LimitPct   float64 `json:"limit_pct,omitempty"`
		}

		out := make([]byWorkflow, 0, len(wfs))
		var total float64

		for _, wf := range wfs {
			spent, sumErr := store.SumCostSince(c.Request.Context(), wf.ID, since)
			if sumErr != nil {
				continue
			}
			row := byWorkflow{
				WorkflowID: wf.ID,
				Name:       wf.Name,
				SpentUSD:   spent,
			}
			if wf.CostLimits != nil && wf.CostLimits.MaxDailyUSD > 0 {
				row.CapUSD = wf.CostLimits.MaxDailyUSD
				pct := 100.0 * spent / row.CapUSD
				row.LimitPct = float64(int(pct*100)) / 100.0
			}
			out = append(out, row)
			total += spent
		}

		c.JSON(http.StatusOK, gin.H{
			"since":       since.Format(time.RFC3339),
			"total":       gin.H{"spent_usd": total},
			"by_workflow": out,
		})
	}
}

// GetWorkflowRun serves a single run with full traces. Stitches in the
// referenced workflow doc so the UI has node names without a follow-up
// fetch — lookup is best-effort, missing workflow doesn't fail the call.
func GetWorkflowRun(store workflow.WorkflowRunStore, wfStore WorkflowStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		if store == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "workflow runs not configured"})
			return
		}
		id := c.Param("id")
		if id == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
			return
		}
		run, err := store.Get(c.Request.Context(), id)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		var workflowDoc *workflow.Workflow
		if wfStore != nil && run.WorkflowID != "" {
			if wf, err := wfStore.GetByID(c.Request.Context(), run.WorkflowID); err == nil {
				workflowDoc = &wf
			}
		}
		c.JSON(http.StatusOK, gin.H{
			"run":      run,
			"workflow": workflowDoc,
		})
	}
}
