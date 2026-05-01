package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/auth"
	"github.com/bRRRITSCOLD/burrow/internal/mongodb"
	"github.com/bRRRITSCOLD/burrow/internal/workflow"
	"github.com/gin-gonic/gin"
	"github.com/oklog/ulid/v2"
	cronlib "github.com/robfig/cron/v3"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// WorkflowStore is the persistence interface for workflow graphs.
// `List` + `GetByID` are unscoped (workers + cross-tenant lookups);
// `ListForTenant` + `GetByIDForTenant` enforce per-tenant scoping
// for UI requests. Handlers should use the scoped variants whenever
// the caller is a logged-in user.
type WorkflowStore interface {
	List(ctx context.Context) ([]workflow.Workflow, error)
	GetByID(ctx context.Context, id string) (workflow.Workflow, error)
	ListForTenant(ctx context.Context, tenantID string) ([]workflow.Workflow, error)
	GetByIDForTenant(ctx context.Context, id, tenantID string) (workflow.Workflow, error)
	Upsert(ctx context.Context, wf workflow.Workflow) (workflow.Workflow, error)
	Delete(ctx context.Context, id string) error
}

// ListWorkflows returns workflows scoped to the caller's tenant.
// When no tenant is in ctx (legacy unauthenticated probe during the
// auth-rollout transition), returns the full list — Phase G will
// flip those to RequireAuth.
func ListWorkflows(store WorkflowStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		tenantID, hasTenant := auth.TenantFromCtx(c.Request.Context())
		var wfs []workflow.Workflow
		var err error
		if hasTenant {
			wfs, err = store.ListForTenant(c.Request.Context(), tenantID)
		} else {
			wfs, err = store.List(c.Request.Context())
		}
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

		// Tenant scoping: if the request is authed, stamp the active
		// tenant onto the workflow + verify ownership on update. Drops
		// to legacy unscoped behaviour during the auth-rollout phase
		// when no tenant in ctx (Phase G removes that fallback).
		if tenantID, ok := auth.TenantFromCtx(c.Request.Context()); ok {
			existing, gerr := store.GetByIDForTenant(c.Request.Context(), id, tenantID)
			if gerr == nil {
				// Update path — keep existing tenant + created_at.
				wf.TenantID = existing.TenantID
				wf.CreatedAt = existing.CreatedAt
			} else if errors.Is(gerr, mongo.ErrNoDocuments) {
				// New workflow OR foreign-tenant takeover attempt.
				// Probe unscoped: if it exists for someone else,
				// reject; if it doesn't exist anywhere, create new.
				_, anyErr := store.GetByID(c.Request.Context(), id)
				if anyErr == nil {
					c.JSON(http.StatusForbidden, gin.H{"error": "workflow exists in another tenant"})
					return
				}
				wf.TenantID = tenantID
			} else {
				c.JSON(http.StatusInternalServerError, gin.H{"error": gerr.Error()})
				return
			}
		}

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

		// Validate ApprovalChannel — type allowlist + non-empty Target
		// for transports that need one. Empty target on "none" type is
		// fine (it's the explicit "no routing" signal).
		if wf.ApprovalChannel != nil {
			validApprovalTypes := map[string]bool{"smtp": true, "slack_webhook": true, "none": true}
			t := wf.ApprovalChannel.Type
			if !validApprovalTypes[t] {
				c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("approval_channel.type must be one of smtp|slack_webhook|none, got %q", t)})
				return
			}
			if (t == "smtp" || t == "slack_webhook") && wf.ApprovalChannel.Target == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("approval_channel.target required when type=%q", t)})
				return
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

// GetWorkflow returns a single workflow by ID, scoped to the caller's
// tenant when authed. Foreign-tenant lookups return 404.
func GetWorkflow(store WorkflowStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if id == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
			return
		}
		var wf workflow.Workflow
		var err error
		if tenantID, ok := auth.TenantFromCtx(c.Request.Context()); ok {
			wf, err = store.GetByIDForTenant(c.Request.Context(), id, tenantID)
		} else {
			wf, err = store.GetByID(c.Request.Context(), id)
		}
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, wf)
	}
}

// DeleteWorkflow removes the workflow with the given ID. Tenant-scoped
// when authed: a workflow that doesn't belong to the caller's tenant
// returns 404 (not 403 — don't confirm existence).
func DeleteWorkflow(store WorkflowStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if id == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
			return
		}
		if tenantID, ok := auth.TenantFromCtx(c.Request.Context()); ok {
			if _, gerr := store.GetByIDForTenant(c.Request.Context(), id, tenantID); gerr != nil {
				c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
				return
			}
		}
		if err := store.Delete(c.Request.Context(), id); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// WorkflowDuplicateDeps wraps the dependencies needed by the
// workflow-duplicate handler so we can audit-log the privileged
// action without expanding the existing WorkflowStore signature.
type WorkflowDuplicateDeps struct {
	Store WorkflowStore
	Audit *mongodb.AuditRepository
}

// DuplicateWorkflow forks the workflow at :id into a brand-new
// workflow within the caller's tenant. The new doc's ID is a fresh
// ULID, name carries a " (copy)" suffix, version resets to 1 (Mongo's
// `$inc` initialises it on insert), created_at / updated_at are
// stamped server-side. Run history isn't copied — runs are keyed by
// workflow_id and stay with the source. Audit-logged.
//
// Optional body: `{"new_id": "<ulid>"}` overrides the generated ID
// (lets the UI pre-allocate it for navigation).
func DuplicateWorkflow(deps WorkflowDuplicateDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if id == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
			return
		}

		tenantID, hasTenant := auth.TenantFromCtx(c.Request.Context())
		if !hasTenant {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "auth required"})
			return
		}

		src, err := deps.Store.GetByIDForTenant(c.Request.Context(), id, tenantID)
		if err != nil {
			if errors.Is(err, mongo.ErrNoDocuments) {
				c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		var body struct {
			NewID string `json:"new_id"`
		}
		_ = json.NewDecoder(c.Request.Body).Decode(&body)
		newID := body.NewID
		if newID == "" {
			newID = ulid.Make().String()
		}

		// Defensive: if the caller supplied a new_id that already exists
		// (re-tries, races, or a malicious attempt to overwrite somebody
		// else's doc), refuse rather than silently merging into it.
		if _, gerr := deps.Store.GetByID(c.Request.Context(), newID); gerr == nil {
			c.JSON(http.StatusConflict, gin.H{"error": "new_id already in use"})
			return
		} else if !errors.Is(gerr, mongo.ErrNoDocuments) {
			c.JSON(http.StatusInternalServerError, gin.H{"error": gerr.Error()})
			return
		}

		dup := src
		dup.ID = newID
		dup.Name = src.Name + " (copy)"
		dup.Version = 0 // server-stamped via $inc to 1 on insert
		dup.CreatedAt = time.Time{}
		dup.UpdatedAt = time.Time{}

		saved, err := deps.Store.Upsert(c.Request.Context(), dup)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		recordAudit(c, deps.Audit, mongodb.AuditWorkflowDuplicated,
			map[string]any{
				"source_id":    id,
				"duplicate_id": saved.ID,
			},
			map[string]any{"name": saved.Name},
		)

		c.JSON(http.StatusCreated, saved)
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
