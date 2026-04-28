package workflow

import (
	"context"
	"time"
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
}

// RunStatus is the lifecycle state of a workflow run.
type RunStatus string

const (
	RunStatusRunning   RunStatus = "running"
	RunStatusSuccess   RunStatus = "success"
	RunStatusError     RunStatus = "error"
	RunStatusCancelled RunStatus = "cancelled"
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
	// AppendTrace adds a single trace event to an agent's trace, atomically
	// (used during live streaming). nodeID is the AI Agent's node ID.
	AppendTrace(ctx context.Context, runID, nodeID string, event TraceEvent) error
}
