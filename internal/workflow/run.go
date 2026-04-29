package workflow

import (
	"context"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/llm"
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
}

// RunFilter captures the query parameters for the runs history table.
// All fields are optional. Limit defaults to 50; Skip defaults to 0.
// Sort is started_at descending (most recent first).
type RunFilter struct {
	WorkflowID string    // empty = all workflows
	Status     RunStatus // empty = any status
	StartedAfter  time.Time // zero = no lower bound
	StartedBefore time.Time // zero = no upper bound
	Limit         int
	Skip          int
}
