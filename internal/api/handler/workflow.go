package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
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
	SetEnabled(ctx context.Context, id string, enabled bool, reason string) (workflow.Workflow, error)
	SetName(ctx context.Context, id, name string) (workflow.Workflow, error)
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

		// Validate ConfigSchema (typed Config declaration) when set:
		// every entry must have a name + a recognised type, enum types
		// must declare at least one option, required entries must have
		// a non-empty value in Config (default counts when explicit
		// value missing). Empty schema = legacy free-form Config, no
		// validation.
		if len(wf.ConfigSchema) > 0 {
			validTypes := map[string]bool{"string": true, "number": true, "boolean": true, "enum": true}
			seen := map[string]bool{}
			for i, p := range wf.ConfigSchema {
				if p.Name == "" {
					c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("config_schema[%d]: name required", i)})
					return
				}
				if seen[p.Name] {
					c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("config_schema: duplicate name %q", p.Name)})
					return
				}
				seen[p.Name] = true
				if !validTypes[p.Type] {
					c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("config_schema[%s]: type must be one of string|number|boolean|enum", p.Name)})
					return
				}
				if p.Type == "enum" && len(p.Enum) == 0 {
					c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("config_schema[%s]: enum requires at least one option", p.Name)})
					return
				}
				if p.Required {
					val, has := wf.Config[p.Name]
					if !has || val == "" {
						if p.Default == "" {
							c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("config[%s] is required by config_schema but missing", p.Name)})
							return
						}
						// Backfill default so downstream nodes don't trip on
						// the missing key.
						if wf.Config == nil {
							wf.Config = map[string]string{}
						}
						wf.Config[p.Name] = p.Default
					}
				}
				// Enum value enforcement — if Config has the key, value must
				// be one of the declared options.
				if p.Type == "enum" {
					if val, has := wf.Config[p.Name]; has && val != "" {
						match := false
						for _, opt := range p.Enum {
							if val == opt {
								match = true
								break
							}
						}
						if !match {
							c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("config[%s]=%q not in enum %v", p.Name, val, p.Enum)})
							return
						}
					}
				}
			}
		}

		// One return node per workflow — the dispatch helper picks
		// the first one it finds, so multiple return nodes would
		// produce non-deterministic semantics across BFS orderings.
		// Refuse at save with a clear error.
		var returnCount int
		for _, n := range wf.Nodes {
			if n.Type == workflow.NodeTypeReturn {
				returnCount++
			}
		}
		if returnCount > 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("workflow has %d return nodes; at most one allowed", returnCount)})
			return
		}

		// Sub-workflow tenancy validation (defense-in-depth — the
		// engine also refuses cross-tenant dispatch at run time in
		// sub_workflow.go, but rejecting at save closes the
		// existence-probing vector and gives immediate UX feedback).
		// Empty workflow_id is allowed (unconfigured draft node).
		// Self-reference is allowed; cycle detection is a run-time
		// concern. Same error string for "not found" and
		// "foreign tenant" so the response can't be used to probe
		// for workflow ids belonging to other tenants.
		if tenantID, ok := auth.TenantFromCtx(c.Request.Context()); ok {
			for _, n := range wf.Nodes {
				if n.Type != workflow.NodeTypeSubWorkflow {
					continue
				}
				targetID, _ := n.Data["workflow_id"].(string)
				targetID = strings.TrimSpace(targetID)
				if targetID == "" || targetID == id {
					continue
				}
				if _, gerr := store.GetByIDForTenant(c.Request.Context(), targetID, tenantID); gerr != nil {
					c.JSON(http.StatusBadRequest, gin.H{
						"error":   fmt.Sprintf("sub_workflow node %q references unknown or inaccessible workflow", n.ID),
						"node_id": n.ID,
					})
					return
				}
			}
		}

		// Validate OutputSchemaJSON (parse-only — same rule as
		// InputSchemaJSON: must be a JSON object).
		if strings.TrimSpace(wf.OutputSchemaJSON) != "" {
			var probe map[string]any
			if jerr := json.Unmarshal([]byte(wf.OutputSchemaJSON), &probe); jerr != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("output_schema_json: not valid JSON: %s", jerr.Error())})
				return
			}
		}

		// Validate OutputSchema same way as InputSchema: name +
		// type + enum-needs-options + no-dups.
		if len(wf.OutputSchema) > 0 {
			validTypes := map[string]bool{"string": true, "number": true, "boolean": true, "enum": true}
			seenOut := map[string]bool{}
			for i, p := range wf.OutputSchema {
				if p.Name == "" {
					c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("output_schema[%d]: name required", i)})
					return
				}
				if seenOut[p.Name] {
					c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("output_schema: duplicate name %q", p.Name)})
					return
				}
				seenOut[p.Name] = true
				if !validTypes[p.Type] {
					c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("output_schema[%s]: type must be one of string|number|boolean|enum", p.Name)})
					return
				}
				if p.Type == "enum" && len(p.Enum) == 0 {
					c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("output_schema[%s]: enum requires at least one option", p.Name)})
					return
				}
			}
		}

		// Validate InputSchemaJSON (raw JSON Schema). Parse-only —
		// must be a JSON object. Deep schema validity (refs,
		// keyword soundness) is the consumer's problem; we only
		// guard against shipping invalid JSON that would crash
		// downstream validators at dispatch time.
		if strings.TrimSpace(wf.InputSchemaJSON) != "" {
			var probe map[string]any
			if jerr := json.Unmarshal([]byte(wf.InputSchemaJSON), &probe); jerr != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("input_schema_json: not valid JSON: %s", jerr.Error())})
				return
			}
		}

		// Validate InputSchema (typed RUN INPUT declaration). Same
		// rules as ConfigSchema minus the value-check: input is
		// per-run dynamic, so we can't validate any "current value"
		// at save time — that gates to the engine at dispatch.
		if len(wf.InputSchema) > 0 {
			validTypes := map[string]bool{"string": true, "number": true, "boolean": true, "enum": true}
			seenInput := map[string]bool{}
			for i, p := range wf.InputSchema {
				if p.Name == "" {
					c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("input_schema[%d]: name required", i)})
					return
				}
				if seenInput[p.Name] {
					c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("input_schema: duplicate name %q", p.Name)})
					return
				}
				seenInput[p.Name] = true
				if !validTypes[p.Type] {
					c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("input_schema[%s]: type must be one of string|number|boolean|enum", p.Name)})
					return
				}
				if p.Type == "enum" && len(p.Enum) == 0 {
					c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("input_schema[%s]: enum requires at least one option", p.Name)})
					return
				}
			}
		}

		// Validate ApprovalChannel — type allowlist + non-empty Target
		// for transports that need one. Empty target on "none" type is
		// fine (it's the explicit "no routing" signal).
		if wf.ApprovalChannel != nil {
			validApprovalTypes := map[string]bool{"smtp": true, "slack_webhook": true, "slack_bot": true, "none": true}
			t := wf.ApprovalChannel.Type
			if !validApprovalTypes[t] {
				c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("approval_channel.type must be one of smtp|slack_webhook|slack_bot|none, got %q", t)})
				return
			}
			if (t == "smtp" || t == "slack_webhook" || t == "slack_bot") && wf.ApprovalChannel.Target == "" {
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

		// Required-connection validation (defense-in-depth for the
		// worker-side refusal shipped in PR #78). Reject a half-wired
		// workflow at SAVE time so a direct API client can't bypass
		// the canvas's `requireExplicit` UX guard and ship a workflow
		// the worker will only error on at run-time:
		//   - mongo_request / redis_request   → connection_id required
		//     (platform Mongo/Redis are NEVER reachable from a workflow)
		//   - trigger (rabbitmq / redis_subscribe) → connection_id required
		//   - ai_agent                        → llm_connection_id required
		type missingConn struct {
			NodeID   string `json:"node_id"`
			NodeName string `json:"node_name,omitempty"`
			NodeType string `json:"node_type"`
			Missing  string `json:"missing_field"`
		}
		var missing []missingConn
		needs := func(data map[string]any, key string) bool {
			v, _ := data[key].(string)
			return strings.TrimSpace(v) == ""
		}
		for _, n := range wf.Nodes {
			name, _ := n.Data["name"].(string)
			switch n.Type {
			case workflow.NodeTypeMongoRequest, workflow.NodeTypeRedisRequest:
				if needs(n.Data, "connection_id") {
					missing = append(missing, missingConn{n.ID, name, string(n.Type), "connection_id"})
				}
			case workflow.NodeTypeTrigger:
				tt, _ := n.Data["trigger_type"].(string)
				if (tt == "rabbitmq" || tt == "redis_subscribe") && needs(n.Data, "connection_id") {
					missing = append(missing, missingConn{n.ID, name, string(n.Type) + ":" + tt, "connection_id"})
				}
			case workflow.NodeTypeAIAgent:
				if needs(n.Data, "llm_connection_id") {
					missing = append(missing, missingConn{n.ID, name, string(n.Type), "llm_connection_id"})
				}
			}
		}
		if len(missing) > 0 {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":   "workflow has nodes missing a required connection",
				"missing": missing,
			})
			return
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

// RunWorkflow dispatches the workflow asynchronously: a run record is
// persisted with status=running, a wakeup is published on the
// burrow:wakeup channel, and the handler returns 202 + run_id
// immediately. The workflow-executor worker (Phase 3 PR 3.2) claims
// the run via Mongo lease and drives the BFS to completion. The UI
// (or any caller) reads run-detail / runs-history / WS streams for
// progress + final status.
//
// Body fields:
//   - input:          optional initial input passed to trigger nodes
//   - resume_run_id:  when non-empty, resume a previously paused run.
//                     This branch keeps the legacy synchronous
//                     RunResumable path because paused-agent resume
//                     reuses an in-memory PausedAgent snapshot that
//                     hasn't been migrated to the lease/checkpoint
//                     model yet.
//   - stop_at:        DEPRECATED on the async path. Breakpoints are a
//                     canvas-WS concern; the headless `/run` endpoint
//                     ignores it.
//
// Response codes:
//   - 202: run dispatched (async). Body: `{run_id, status:"running"}`.
//   - 200: legacy sync resume completed. Body: `{run_id, status, steps}`.
//   - 4xx/5xx: validation / I/O failure as before.
func RunWorkflow(store WorkflowStore, runStore workflow.WorkflowRunStore, wakeup workflow.WakeupPublisher, exec *workflow.WorkflowExecutor) gin.HandlerFunc {
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

		// Legacy sync resume path: paused-agent runs still complete
		// in-process via RunResumable. Migration of resume to the lease
		// path is a follow-up; in the meantime callers can still wake
		// a paused run from this endpoint.
		if req.ResumeRunID != "" {
			outcome, rerr := exec.RunResumable(c.Request.Context(), wf, workflow.RunOpts{
				StopAt:      req.StopAt,
				Input:       req.Input,
				ResumeRunID: req.ResumeRunID,
			})
			if rerr != nil {
				c.JSON(http.StatusInternalServerError, gin.H{
					"error":  rerr.Error(),
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
			return
		}

		if runStore == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "run store not configured"})
			return
		}

		runID := ulid.Make().String()
		now := time.Now().UTC()
		rec := workflow.WorkflowRun{
			ID:           runID,
			WorkflowID:   wf.ID,
			TenantID:     wf.TenantID,
			// Dispatched but unclaimed — ClaimLease flips it to
			// running atomically when a worker picks it up. Without
			// the queued status the UI shows "running" the whole
			// time the run sits in the worker queue (which can be
			// noticeable under burst load or a cold worker pool).
			Status:       workflow.RunStatusQueued,
			QueuedAt:     now,
			// StartedAt stays nil until ClaimLease stamps it on first
			// worker pickup — duration math then excludes queue time.
			Config: wf.Config,
			TriggerInput: req.Input,
		}
		if _, cerr := runStore.Create(c.Request.Context(), rec); cerr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "create run: " + cerr.Error()})
			return
		}
		// Best-effort wakeup; nil publisher = no Redis wired = worker
		// picks it up on next tick instead.
		workflow.PublishWakeup(c.Request.Context(), wakeup)
		c.JSON(http.StatusAccepted, gin.H{
			"run_id": runID,
			"status": workflow.RunStatusQueued,
		})
	}
}

// WorkflowEnableDeps wraps the dependencies needed by the
// patch-enabled handler so the privileged action can be audit-logged.
type WorkflowEnableDeps struct {
	Store WorkflowStore
	Audit *mongodb.AuditRepository
}

// PatchWorkflowEnabled toggles a workflow's `enabled` flag. Disabled
// workflows drop from every trigger worker's active set (cron, RMQ,
// Redis-subscribe, future websocket) on their next sync tick, but
// stay fully editable + manually runnable from the canvas Run button.
//
// Tenant-scoped: refuses cross-tenant patches with 404 (same shape as
// the workflow read endpoints — avoids leaking id existence).
//
// Body: { "enabled": bool, "reason"?: string }. `reason` is optional
// free-text persisted alongside `disabled_at` for the UI / audit
// trail when disabling. Cleared on enable.
//
// Manual runs (POST /run) deliberately bypass this gate — the
// user explicitly clicked Run, the disable is a TRIGGER-routing
// rule, not a workflow-wide ban.
func PatchWorkflowEnabled(deps WorkflowEnableDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if id == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
			return
		}
		var req struct {
			Enabled bool   `json:"enabled"`
			Reason  string `json:"reason"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		// Tenant ownership check — same 404-on-cross-tenant rule as
		// the read endpoints. Avoids leaking id existence to a
		// non-owning tenant.
		if tenantID, ok := auth.TenantFromCtx(c.Request.Context()); ok {
			existing, gerr := deps.Store.GetByIDForTenant(c.Request.Context(), id, tenantID)
			if gerr != nil {
				if errors.Is(gerr, mongo.ErrNoDocuments) {
					c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
					return
				}
				c.JSON(http.StatusInternalServerError, gin.H{"error": gerr.Error()})
				return
			}
			// no-op short-circuit — avoids a write + audit row when
			// the toggle would be a no-change request.
			if existing.Enabled == req.Enabled {
				c.JSON(http.StatusOK, existing)
				return
			}
		}

		updated, err := deps.Store.SetEnabled(c.Request.Context(), id, req.Enabled, req.Reason)
		if err != nil {
			if errors.Is(err, mongo.ErrNoDocuments) {
				c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		action := mongodb.AuditWorkflowEnabled
		if !req.Enabled {
			action = mongodb.AuditWorkflowDisabled
		}
		recordAudit(c, deps.Audit, action,
			map[string]any{"type": "workflow", "id": id, "name": updated.Name},
			map[string]any{"reason": req.Reason})
		c.JSON(http.StatusOK, updated)
	}
}

// PatchWorkflowName rewrites a workflow's display name without
// round-tripping the rest of the graph. Same tenant-isolation +
// audit-log shape as PatchWorkflowEnabled.
//
// Body: { "name": "<new name>" }. Name is trimmed; empty after
// trim → 400. Length capped at 200 chars to keep the sidebar /
// breadcrumb / list views honest.
func PatchWorkflowName(deps WorkflowEnableDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if id == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
			return
		}
		var req struct {
			Name string `json:"name"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		name := strings.TrimSpace(req.Name)
		if name == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name required"})
			return
		}
		if len(name) > 200 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name too long (max 200 chars)"})
			return
		}

		var oldName string
		if tenantID, ok := auth.TenantFromCtx(c.Request.Context()); ok {
			existing, gerr := deps.Store.GetByIDForTenant(c.Request.Context(), id, tenantID)
			if gerr != nil {
				if errors.Is(gerr, mongo.ErrNoDocuments) {
					c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
					return
				}
				c.JSON(http.StatusInternalServerError, gin.H{"error": gerr.Error()})
				return
			}
			oldName = existing.Name
			if existing.Name == name {
				// No-op short-circuit — avoids a write + audit row.
				c.JSON(http.StatusOK, existing)
				return
			}
		}

		updated, err := deps.Store.SetName(c.Request.Context(), id, name)
		if err != nil {
			if errors.Is(err, mongo.ErrNoDocuments) {
				c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		recordAudit(c, deps.Audit, mongodb.AuditWorkflowRenamed,
			map[string]any{"type": "workflow", "id": id, "name": updated.Name},
			map[string]any{"old_name": oldName, "new_name": updated.Name})
		c.JSON(http.StatusOK, updated)
	}
}
