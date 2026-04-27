package handler

import (
	"context"
	"encoding/json"
	"errors"
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

		// Validate cron expressions on trigger nodes w/ trigger_type=cron
		cronParser := cronlib.NewParser(cronlib.Minute | cronlib.Hour | cronlib.Dom | cronlib.Month | cronlib.Dow)
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
func RunWorkflow(store WorkflowStore, exec *workflow.WorkflowExecutor) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if id == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
			return
		}

		var req struct {
			StopAt string `json:"stop_at"`
			Input  any    `json:"input,omitempty"`
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

		var steps []workflow.StepResult
		if req.Input != nil {
			steps, err = exec.Run(c.Request.Context(), wf, req.StopAt, req.Input)
		} else {
			steps, err = exec.Run(c.Request.Context(), wf, req.StopAt)
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"steps": steps})
	}
}
