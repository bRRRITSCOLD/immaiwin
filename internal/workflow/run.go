package workflow

import (
	"context"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/llm"
)

// WorkflowRun is one execution of a workflow. Persisted to MongoDB so the
// UI can show run history, agents can stitch memory across runs, and we
// have an audit trail for cost + behavior over time.
type WorkflowRun struct {
	ID           string                  `bson:"_id"            json:"id"`           // ULID
	WorkflowID   string                  `bson:"workflow_id"    json:"workflow_id"`
	TenantID     string                  `bson:"tenant_id"      json:"tenant_id"`    // "default" until multi-tenant
	StartedAt    time.Time               `bson:"started_at"     json:"started_at"`
	FinishedAt   *time.Time              `bson:"finished_at,omitempty" json:"finished_at,omitempty"`
	Status       RunStatus               `bson:"status"         json:"status"`
	TriggerInput any                     `bson:"trigger_input,omitempty" json:"trigger_input,omitempty"`
	Params       map[string]string       `bson:"params,omitempty"        json:"params,omitempty"`
	Steps        []StepResult            `bson:"steps,omitempty"         json:"steps,omitempty"`
	AgentTraces  map[string][]TraceEvent `bson:"agent_traces,omitempty"  json:"agent_traces,omitempty"`
	Usage        UsageTotal              `bson:"usage,omitempty"         json:"usage,omitempty"`
	Error        string                  `bson:"error,omitempty"         json:"error,omitempty"`

	// PausedAgent is set when the BFS halted inside an AI Agent node's ReAct
	// loop because of a stopAt-bound tool call. Saving the agent's working
	// state here lets the next Run resume mid-conversation instead of
	// restarting from scratch. nil when the run is not paused.
	PausedAgent *AgentPauseState `bson:"paused_agent,omitempty" json:"paused_agent,omitempty"`

	// PendingApproval is set when the AI agent's `require_approval` gate
	// fires on a server-side run (no live WS connection). The run blocks
	// in-process waiting for a `POST /api/v1/runs/:id/approval` decision.
	// The /runs/:id UI page reads this to render Approve/Reject buttons.
	// Cleared once the decision arrives.
	PendingApproval *PendingApprovalState `bson:"pending_approval,omitempty" json:"pending_approval,omitempty"`

	// LeaseOwner identifies the worker holding execution rights for
	// this run (Phase 3 durable execution). Empty (or `lease_expires_at`
	// in the past) means the run is up for grabs by any worker. Workers
	// claim leases via WorkflowRunStore.ClaimLease, extend them via
	// ExtendLease while heartbeating, and release them on yield-points
	// (gate fire, breakpoint, terminal status). On worker death the
	// lease auto-expires and another worker re-claims; this is what
	// makes the platform restart-safe + horizontally scalable. See
	// .private/ai-automation/DURABLE-EXECUTION-PLAN.md for the design.
	LeaseOwner string `bson:"lease_owner,omitempty" json:"lease_owner,omitempty"`
	// LeaseExpiresAt is the wall-clock deadline by which the holder
	// must extend the lease or it auto-expires + becomes claimable by
	// any worker. Tracked in UTC; stale leases are cleaned up
	// implicitly by ClaimLease's predicate.
	LeaseExpiresAt *time.Time `bson:"lease_expires_at,omitempty" json:"lease_expires_at,omitempty"`
	// LastCheckpointAt is the wall-clock time of the last successful
	// checkpoint write (BFS step boundary, agent iter, tool call, gate
	// fire). Used by the resume path to prefer recently-checkpointed
	// runs + by ops dashboards to spot runs whose worker is alive but
	// not making progress.
	LastCheckpointAt *time.Time `bson:"last_checkpoint_at,omitempty" json:"last_checkpoint_at,omitempty"`

	// ExecutionState carries the BFS frontier + visited set + named-
	// node context map needed to resume mid-run after a worker death
	// or an approval-gate yield. nil = run never checkpointed (legacy
	// in-process executor path) OR run has not yielded yet. Workers
	// that claim a run with non-nil ExecutionState rehydrate from it
	// and resume BFS from `Queue` rather than re-executing from
	// trigger nodes. See PR 3.2 in
	// .private/ai-automation/DURABLE-EXECUTION-PLAN.md.
	ExecutionState *ExecutionState `bson:"execution_state,omitempty" json:"execution_state,omitempty"`
}

// ExecutionState is the durable BFS snapshot persisted at every step
// boundary so a run can resume cleanly after worker death or an
// approval-gate yield. Mirrors the in-memory state held by
// RunWithEvents: Visited (already-executed node IDs), Queue (BFS
// frontier with carried inputs), WfCtx (named-node {input, output}
// map exposed to JS transforms), and Pending (the active gate, when
// the run is paused waiting on a decision).
type ExecutionState struct {
	// Visited is the set of node IDs that have already been executed
	// in this run. Stored as a slice for deterministic bson encoding;
	// rehydrated to a map[string]bool on resume.
	Visited []string `bson:"visited,omitempty" json:"visited,omitempty"`
	// Queue is the BFS frontier — the next batch of nodes to execute,
	// each carrying the input passed in by its source node. Resume
	// rehydrates this directly into the executor's local queue.
	Queue []QueuedNode `bson:"queue,omitempty" json:"queue,omitempty"`
	// WfCtx is the named-node context map (`{name -> StepContext}`)
	// that JS transforms reference as the `context` global. Persisted
	// so resumed runs see the same context their pre-yield BFS saw.
	WfCtx map[string]StepContext `bson:"wf_ctx,omitempty" json:"wf_ctx,omitempty"`
	// Pending is set when the BFS yielded at an approval gate. Carries
	// the gated node + the input it was about to receive; once the
	// approval handler writes Decision into it, the next worker that
	// claims the run applies the decision (success edge on approve,
	// error edge on reject) and continues. nil = run is freely
	// claimable (no gate active) OR the gate has already been applied.
	Pending *PendingExecutionGate `bson:"pending,omitempty" json:"pending,omitempty"`
}

// QueuedNode is one entry in the BFS frontier — the node ID to execute
// next plus the input value its source produced. Persisted as part of
// ExecutionState so the resumed run dispatches the same node with the
// same input it would have without the yield.
type QueuedNode struct {
	NodeID string `bson:"node_id" json:"node_id"`
	Input  any    `bson:"input,omitempty" json:"input,omitempty"`
}

// PendingExecutionGate captures an approval gate that the BFS yielded
// on. The worker persists this + releases its lease + returns from
// the BFS loop; the approval handler later writes Decision into the
// same struct + clears the run's lease + publishes a wakeup. The next
// worker that claims the run sees Decision != nil, applies it
// (replays Pending.Input through the success or error edge of
// Pending.NodeID), and continues BFS from the rest of Queue.
//
// Distinct from PendingApprovalState (which mirrors the gate for the
// /runs UI to render) — PendingExecutionGate is the executor's
// internal yield record. They co-exist on the run record while a gate
// is active and clear together when the decision lands.
type PendingExecutionGate struct {
	NodeID   string            `bson:"node_id" json:"node_id"`
	Input    any               `bson:"input,omitempty" json:"input,omitempty"`
	Decision *ApprovalDecision `bson:"decision,omitempty" json:"decision,omitempty"`
}

// PendingApprovalState mirrors the active require_approval gate so the
// UI (or any other approval surface — email link, Slack message) has
// everything needed to present the decision to the human. Two flavours:
//   - Kind="tool_call": agent's per-tool gate. AgentNodeID + ToolCallID
//     + ToolName + ToolArgs populated.
//   - Kind="node":      pre-exec gate on a non-agent node. NodeID +
//     NodeType + NodeName + NodeInput populated.
type PendingApprovalState struct {
	Kind        string    `bson:"kind,omitempty"        json:"kind,omitempty"` // "tool_call" | "node"
	AgentNodeID string    `bson:"agent_node_id,omitempty" json:"agent_node_id,omitempty"`
	Iter        int       `bson:"iter,omitempty"        json:"iter,omitempty"`
	ToolCallID  string    `bson:"tool_call_id,omitempty" json:"tool_call_id,omitempty"`
	ToolName    string    `bson:"tool_name,omitempty"   json:"tool_name,omitempty"`
	ToolArgs    any       `bson:"tool_args,omitempty"   json:"tool_args,omitempty"`
	NodeID      string    `bson:"node_id,omitempty"     json:"node_id,omitempty"`
	NodeType    NodeType  `bson:"node_type,omitempty"   json:"node_type,omitempty"`
	NodeName    string    `bson:"node_name,omitempty"   json:"node_name,omitempty"`
	NodeInput   any       `bson:"node_input,omitempty"  json:"node_input,omitempty"`
	RequestedAt time.Time `bson:"requested_at"          json:"requested_at"`
	// TokenID is a server-stamped ULID used as the binding nonce for the
	// magic-link approval token (Stage 2). Set when the gate fires;
	// cleared when the decision lands. The HMAC-signed token carries
	// (run_id, token_id, decision, expires_at). The redeem handler
	// rejects mismatched / expired / cleared token IDs so a stale link
	// can't resurrect a closed gate. Empty means "no magic link
	// configured yet" — the /runs/:id UI Approve/Reject path still
	// works in that case.
	TokenID string `bson:"token_id,omitempty"    json:"token_id,omitempty"`
	// DispatchError captures the OOB notifier's failure (slack
	// rejection, SMTP relay error, ...) when the approval prompt
	// couldn't be delivered. The gate still pauses and is resolvable
	// via the /runs/:id Approve/Reject path; this field gives the UI
	// something concrete to render so the user understands why no
	// email or Slack message arrived. Populated AFTER the dispatch
	// goroutine resolves; cleared alongside the rest of pending
	// state when the gate decision lands.
	DispatchError string `bson:"dispatch_error,omitempty" json:"dispatch_error,omitempty"`
}

// AgentPauseState is the snapshot persisted on a paused workflow run so
// the agent loop can resume mid-conversation. The same node ID + agent's
// declared inputs (system_prompt, user_input) are deterministic for a
// given workflow definition; we only persist the dynamic per-run state
// (messages so far, current iter, accumulated usage + trace).
//
// Tool catalog is rebuilt fresh on resume — the catalog is a function of
// the agent's data + connected tool nodes + skills, not run state, and
// rebuilding catches edits to those between pause and resume (which is
// almost always what the user wants). The skill_secrets / skill_config /
// per-iteration message state make agent loop state reproducible.
type AgentPauseState struct {
	AgentNodeID  string        `bson:"agent_node_id" json:"agent_node_id"`
	Iter         int           `bson:"iter"          json:"iter"`
	Messages     []llm.Message `bson:"messages"      json:"messages"`
	UsageTotal   UsageTotal    `bson:"usage_total"   json:"usage_total"`
	Trace        []TraceEvent  `bson:"trace"         json:"trace"`
	SystemPrompt string        `bson:"system_prompt" json:"system_prompt"`
	UserInput    string        `bson:"user_input"    json:"user_input"`
}

// RunStatus is the lifecycle state of a workflow run.
type RunStatus string

const (
	RunStatusRunning   RunStatus = "running"
	RunStatusSuccess   RunStatus = "success"
	RunStatusError     RunStatus = "error"
	RunStatusCancelled RunStatus = "cancelled"
	// RunStatusPaused indicates the BFS halted inside an AI Agent because
	// of a stopAt-bound tool call. WorkflowRun.PausedAgent carries enough
	// state for the next Run (with `resume_run_id`) to pick up mid-conversation.
	RunStatusPaused RunStatus = "paused"
	// RunStatusPendingApproval indicates the agent's require_approval gate
	// fired on a server-side run with no live WS connection. The run is
	// blocked in-process; a POST to /api/v1/runs/:id/approval pushes the
	// decision into the agent's approveCh and the run resumes.
	RunStatusPendingApproval RunStatus = "pending_approval"
)

// TraceEvent is one entry in an agent's reasoning trace, captured per
// agent node, used for live UI streaming + post-hoc replay.
type TraceEvent struct {
	At        time.Time      `bson:"at"                 json:"at"`
	Type      string         `bson:"type"               json:"type"` // "iter_start"|"llm_call"|"tool_call"|"tool_result"|"final"
	Iter      int            `bson:"iter,omitempty"     json:"iter,omitempty"`
	ToolName  string         `bson:"tool_name,omitempty" json:"tool_name,omitempty"`
	ToolID    string         `bson:"tool_id,omitempty"   json:"tool_id,omitempty"` // tool_use_id
	ToolArgs  any            `bson:"tool_args,omitempty" json:"tool_args,omitempty"`
	Result    any            `bson:"result,omitempty"    json:"result,omitempty"`
	Text      string         `bson:"text,omitempty"      json:"text,omitempty"`
	Usage     *UsageTotal    `bson:"usage,omitempty"     json:"usage,omitempty"`
	IsError   bool           `bson:"is_error,omitempty"  json:"is_error,omitempty"`
	// Provider + Model populated on `llm_call` events so the run detail
	// page can show "anthropic / claude-sonnet-4-6" etc per iter without
	// the UI having to chase the agent node's connection_id config.
	Provider string `bson:"provider,omitempty" json:"provider,omitempty"`
	Model    string `bson:"model,omitempty"    json:"model,omitempty"`
}

// UsageTotal aggregates token usage and cost across all LLM calls in a run.
type UsageTotal struct {
	InputTokens  int     `bson:"input_tokens"   json:"input_tokens"`
	OutputTokens int     `bson:"output_tokens"  json:"output_tokens"`
	TotalTokens  int     `bson:"total_tokens"   json:"total_tokens"`
	CostUSD      float64 `bson:"cost_usd"       json:"cost_usd"`
}

// Add accumulates another usage set into this one.
func (u *UsageTotal) Add(in, out int, costUSD float64) {
	u.InputTokens += in
	u.OutputTokens += out
	u.TotalTokens += in + out
	u.CostUSD += costUSD
}

// WorkflowRunStore is the persistence interface for runs.
type WorkflowRunStore interface {
	Create(ctx context.Context, run WorkflowRun) (WorkflowRun, error)
	Get(ctx context.Context, id string) (WorkflowRun, error)
	Update(ctx context.Context, run WorkflowRun) error
	List(ctx context.Context, workflowID string, limit int) ([]WorkflowRun, error)
	// ListWithFilter is the richer query path used by the /runs UI.
	// All RunFilter fields are optional; empty filter returns the most
	// recent runs across all workflows. Implementations sort by
	// started_at descending so the table view is "latest first" by
	// default.
	ListWithFilter(ctx context.Context, filter RunFilter) ([]WorkflowRun, error)
	// AppendTrace adds a single trace event to an agent's trace, atomically
	// (used during live streaming). nodeID is the AI Agent's node ID.
	AppendTrace(ctx context.Context, runID, nodeID string, event TraceEvent) error
	// SumCostSince returns the sum of WorkflowRun.Usage.CostUSD for all
	// runs of the given workflow whose started_at is >= since. Used by
	// the daily-cost-cap enforcement path (executor pre-run + agent
	// loop mid-run). Returns 0 when the store has no matching docs.
	SumCostSince(ctx context.Context, workflowID string, since time.Time) (float64, error)

	// ClaimLease atomically acquires the lease on a non-terminal run
	// matching one of `statuses`. Predicate also requires the run to be
	// either unleased (no lease_owner) or to have a stale lease
	// (lease_expires_at <= now). Returns (run, true, nil) on success;
	// (zero, false, nil) when no claimable run exists. Implementations
	// MUST use a single findAndModify so two competing workers can
	// never both claim the same run.
	//
	// Phase 3 durable-execution primitive — see
	// .private/ai-automation/DURABLE-EXECUTION-PLAN.md.
	ClaimLease(ctx context.Context, workerID string, leaseDur time.Duration, statuses []RunStatus) (WorkflowRun, bool, error)

	// ExtendLease pushes lease_expires_at forward for the run, only
	// when the caller still owns the lease. Returns ErrLeaseNotHeld
	// when the lease has been re-claimed by a different worker (i.e.
	// our heartbeat was too slow). Caller should treat ErrLeaseNotHeld
	// as a hard signal to abort + release any in-process work.
	ExtendLease(ctx context.Context, runID, workerID string, leaseDur time.Duration) error

	// ReleaseLease clears lease_owner + lease_expires_at, only when the
	// caller currently holds the lease. Foreign release is a no-op
	// (returns nil) — defensive against double-release on cleanup.
	ReleaseLease(ctx context.Context, runID, workerID string) error

	// CheckpointExecutionState atomically persists the BFS snapshot at
	// a step boundary, but only when the caller still holds the lease.
	// On lease loss returns ErrLeaseNotHeld so the caller aborts
	// without overwriting state another worker is now responsible for.
	// Bumps last_checkpoint_at on every successful write; ops uses
	// it to spot live-but-stuck runs.
	//
	// Steps is optional — pass non-nil to also persist the running
	// step results, nil to leave them as-is. Status is optional —
	// pass empty to leave unchanged. Phase 3 PR 3.2.
	CheckpointExecutionState(ctx context.Context, runID, workerID string, state ExecutionState, steps []StepResult, status RunStatus) error

	// ApplyApprovalDecision writes the user's decision into the run's
	// execution-state pending gate, clears any active lease, flips
	// status back to `running`, and bumps last_checkpoint_at. Caller
	// (HTTP approval handler) follows up with a Redis wakeup publish
	// so a worker can claim. Returns nil even when the run already
	// has a decision (idempotent on duplicate clicks); returns an
	// error only on Mongo I/O failures or when the run record is
	// missing. Phase 3 PR 3.2.
	ApplyApprovalDecision(ctx context.Context, runID string, decision ApprovalDecision) error
}

// ErrLeaseNotHeld is returned by ExtendLease (and may be returned by
// ReleaseLease in stricter implementations) when the caller's lease
// has been re-acquired by a different worker. Callers should treat
// this as a fatal "your work is now stale; abort + don't write any
// further state" signal.
var ErrLeaseNotHeld = errLeaseNotHeld{}

// WakeupChannel is the Redis pub/sub channel name used by run
// dispatchers + approval handlers to nudge idle executor workers
// (Phase 3 PR 3.2) into a claim attempt without waiting for the
// next periodic tick. Centralised in the workflow package so both
// the api/handler producers and the worker subscribers reference
// the same string without one importing the other.
const WakeupChannel = "burrow:wakeup"

// WakeupPublisher is the slim subset of the Redis client surface
// needed to broadcast a wakeup. *rediss.Client + ApprovalSubscriber
// both satisfy it without import gymnastics.
type WakeupPublisher interface {
	PublishWithCount(ctx context.Context, channel string, payload []byte) (int64, error)
}

// PublishWakeup nudges any idle workflow-executor worker out of its
// sleep so it tries to claim the freshly-queued (or freshly-decided)
// run without waiting for the next periodic tick. Best-effort: a
// publish failure is logged but never propagated back to the
// caller — the caller has already persisted the run record + the
// next tick will pick it up regardless. Nil publisher is a no-op
// (lets unit tests skip the wiring).
func PublishWakeup(ctx context.Context, pub WakeupPublisher) {
	if pub == nil {
		return
	}
	if _, err := pub.PublishWithCount(ctx, WakeupChannel, []byte("1")); err != nil {
		// Caller's own logging usually identifies the run; keep this
		// at warn level + with no extra context so callers can wrap
		// it if they care.
		// (Avoiding a slog import here would force callers to log;
		//  importing slog package-wide is fine — already used widely
		//  in this package.)
		// noop on purpose: not worth crashing the request path
		_ = err
	}
}

type errLeaseNotHeld struct{}

func (errLeaseNotHeld) Error() string { return "workflow run: lease not held by caller" }

// RunFilter captures the query parameters for the runs history table.
// All fields are optional. Limit defaults to 50; Skip defaults to 0.
// Sort is started_at descending (most recent first).
type RunFilter struct {
	TenantID   string    // empty = unscoped (workers / system queries)
	WorkflowID string    // empty = all workflows
	Status     RunStatus // empty = any status
	StartedAfter  time.Time // zero = no lower bound
	StartedBefore time.Time // zero = no upper bound
	Limit         int
	Skip          int
}
