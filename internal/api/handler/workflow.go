package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/workflow"
	"github.com/gin-gonic/gin"
	cronlib "github.com/robfig/cron/v3"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// WorkflowStore is the persistence interface for workflow graphs.
type WorkflowStore interface {
	List(ctx context.Context) ([]workflow.Workflow, error)
	GetByID(ctx context.Context, id string) (workflow.Workflow, error)
	Upsert(ctx context.Context, wf workflow.Workflow) (workflow.Workflow, error)
	Delete(ctx context.Context, id string) error
}

// ListWorkflows returns all stored workflows.
func ListWorkflows(store WorkflowStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		wfs, err := store.List(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if wfs == nil {
			wfs = []workflow.Workflow{}
		}
		c.JSON(http.StatusOK, wfs)
	}
}

// UpsertWorkflow creates or replaces the workflow with the given ID.
func UpsertWorkflow(store WorkflowStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if id == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
			return
		}

		var wf workflow.Workflow
		if err := c.ShouldBindJSON(&wf); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		wf.ID = id

		// Validate ParamsSchema (typed Params declaration) when set:
		// every entry must have a name + a recognised type, enum types
		// must declare at least one option, required entries must have
		// a non-empty value in Params (default counts when explicit
		// value missing). Empty schema = legacy free-form Params, no
		// validation.
		if len(wf.ParamsSchema) > 0 {
			validTypes := map[string]bool{"string": true, "number": true, "boolean": true, "enum": true}
			seen := map[string]bool{}
			for i, p := range wf.ParamsSchema {
				if p.Name == "" {
					c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("params_schema[%d]: name required", i)})
					return
				}
				if seen[p.Name] {
					c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("params_schema: duplicate name %q", p.Name)})
					return
				}
				seen[p.Name] = true
				if !validTypes[p.Type] {
					c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("params_schema[%s]: type must be one of string|number|boolean|enum", p.Name)})
					return
				}
				if p.Type == "enum" && len(p.Enum) == 0 {
					c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("params_schema[%s]: enum requires at least one option", p.Name)})
					return
				}
				if p.Required {
					val, has := wf.Params[p.Name]
					if !has || val == "" {
						if p.Default == "" {
							c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("params[%s] is required by params_schema but missing", p.Name)})
							return
						}
						// Backfill default so downstream nodes don't trip on
						// the missing key.
						if wf.Params == nil {
							wf.Params = map[string]string{}
						}
						wf.Params[p.Name] = p.Default
					}
				}
				// Enum value enforcement — if Params has the key, value must
				// be one of the declared options.
				if p.Type == "enum" {
					if val, has := wf.Params[p.Name]; has && val != "" {
						match := false
						for _, opt := range p.Enum {
							if val == opt {
								match = true
								break
							}
						}
						if !match {
							c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("params[%s]=%q not in enum %v", p.Name, val, p.Enum)})
							return
						}
					}
				}
			}
		}

		// Validate cron expressions on trigger nodes w/ trigger_type=cron
		// 6-field parser w/ optional seconds — must mirror the worker's
		// scheduler bits or expressions valid here would still reject at
		// schedule-time. `*/5 * * * * *` (every 5s) becomes valid.
		cronParser := cronlib.NewParser(cronlib.SecondOptional | cronlib.Minute | cronlib.Hour | cronlib.Dom | cronlib.Month | cronlib.Dow)
		for _, n := range wf.Nodes {
			if n.Type != workflow.NodeTypeTrigger {
				continue
			}
			tt, _ := n.Data["trigger_type"].(string)
			if tt != "cron" {
				continue
			}
			expr, _ := n.Data["cron"].(string)
			if expr == "" {
				continue
			}
			if _, err := cronParser.Parse(expr); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{
					"error":   "invalid cron expression",
					"node_id": n.ID,
					"detail":  err.Error(),
				})
				return
			}
		}

		saved, err := store.Upsert(c.Request.Context(), wf)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, saved)
	}
}

// GetWorkflow returns a single workflow by ID. Used by /runs detail page
// when only a workflow_id is on hand and the page wants node names.
func GetWorkflow(store WorkflowStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if id == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
			return
		}
		wf, err := store.GetByID(c.Request.Context(), id)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, wf)
	}
}

// DeleteWorkflow removes the workflow with the given ID.
func DeleteWorkflow(store WorkflowStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if id == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
			return
		}
		if err := store.Delete(c.Request.Context(), id); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// RunWorkflow executes the workflow graph and returns per-step results.
//
// Body fields:
//   - stop_at:        node ID to halt after (debug breakpoint)
//   - input:          optional initial input passed to trigger nodes
//   - resume_run_id:  when non-empty, resume the previously paused run
//                     (agent loop hydrates from saved messages/iter; non-
//                     agent nodes already executed are skipped).
//
// Response includes run_id + status so the UI can switch the Run button
// to "Continue" while the run is paused.
func RunWorkflow(store WorkflowStore, exec *workflow.WorkflowExecutor) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if id == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
			return
		}

		var req struct {
			StopAt      string `json:"stop_at"`
			Input       any    `json:"input,omitempty"`
			ResumeRunID string `json:"resume_run_id,omitempty"`
		}
		_ = json.NewDecoder(c.Request.Body).Decode(&req)

		wf, err := store.GetByID(c.Request.Context(), id)
		if err != nil {
			if errors.Is(err, mongo.ErrNoDocuments) {
				c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		outcome, err := exec.RunResumable(c.Request.Context(), wf, workflow.RunOpts{
			StopAt:      req.StopAt,
			Input:       req.Input,
			ResumeRunID: req.ResumeRunID,
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error":  err.Error(),
				"run_id": outcome.RunID,
				"status": outcome.Status,
				"steps":  outcome.Steps,
			})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"steps":  outcome.Steps,
			"run_id": outcome.RunID,
			"status": outcome.Status,
		})
	}
}
