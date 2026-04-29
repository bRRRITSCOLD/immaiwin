package workflow

import (
	"context"
	"time"
)

// Eval is a named reproducibility harness for a workflow: a set of seed
// inputs + assertions that the workflow's output must satisfy. The
// /evals UI uses these to gate "did my prompt change break anything?" —
// the differentiator that turns the agent from a demo into a product.
//
// Runs of an eval go through the regular WorkflowExecutor (`RunResumable`)
// so per-case execution shares the same trace persistence, cost rollup,
// and per-workflow daily cap as ad-hoc runs. The eval layer adds: (1)
// per-case input/params overrides, (2) assertion scoring against the
// agent output, (3) aggregate pass-rate / cost / p95 latency.
type Eval struct {
	ID          string     `bson:"_id"         json:"id"`            // ULID
	WorkflowID  string     `bson:"workflow_id" json:"workflow_id"`
	Name        string     `bson:"name"        json:"name"`
	Description string     `bson:"description,omitempty" json:"description,omitempty"`
	Cases       []EvalCase `bson:"cases"       json:"cases"`
	// Version bumps on every Save so historical EvalRuns can be matched
	// back to the exact config that produced them. Enables "did this
	// fail because the prompt regressed or because I changed an
	// assertion?" diagnostics. UpsertEval always increments.
	Version   int       `bson:"version"     json:"version"`
	CreatedAt time.Time `bson:"created_at"  json:"created_at"`
	UpdatedAt time.Time `bson:"updated_at"  json:"updated_at"`
}

// EvalCase is one seed input + a list of assertions that must hold
// against the resulting workflow run's output. `Input` becomes the
// trigger node's initial input; `Params` overrides workflow.Params for
// this case (string-only since downstream `applyTemplate` only handles
// strings).
type EvalCase struct {
	ID         string            `bson:"id"          json:"id"`
	Name       string            `bson:"name"        json:"name"`
	Input      any               `bson:"input,omitempty"  json:"input,omitempty"`
	Params     map[string]string `bson:"params,omitempty" json:"params,omitempty"`
	Assertions []Assertion       `bson:"assertions"  json:"assertions"`
}

// Assertion is a single boolean check applied to the resulting run's
// data. `Target` selects what to assert against:
//   - "agent_output": the AI Agent node's `output` field (default).
//   - "step": the StepResult.Output of `NodeID`.
//
// `Type` selects the predicate. For text-based predicates the target is
// stringified via fmt.Sprint. For json_path_*, the target must be a JSON
// object (or a JSON-marshalable value); the path is dotted (e.g.
// `usage.input_tokens`).
type Assertion struct {
	Target string `bson:"target,omitempty" json:"target,omitempty"`
	NodeID string `bson:"node_id,omitempty" json:"node_id,omitempty"`
	Type   string `bson:"type"   json:"type"`   // "contains" | "regex" | "json_path_eq" | "json_path_exists" | "not_contains"
	Path   string `bson:"path,omitempty" json:"path,omitempty"`   // for json_path_*
	Value  string `bson:"value,omitempty" json:"value,omitempty"` // expected value (string-encoded for non-text types)
}

// EvalRun is one execution of an Eval — N case runs in (bounded)
// parallel, scored, and persisted. Status:
//   - "running": at least one case in flight.
//   - "done":    every case completed (some may have failed assertions).
//   - "error":   eval-level failure (workflow not found, executor unavailable).
type EvalRun struct {
	ID         string           `bson:"_id"         json:"id"`
	EvalID     string           `bson:"eval_id"     json:"eval_id"`
	WorkflowID string           `bson:"workflow_id" json:"workflow_id"`
	StartedAt  time.Time        `bson:"started_at"  json:"started_at"`
	FinishedAt *time.Time       `bson:"finished_at,omitempty" json:"finished_at,omitempty"`
	Status     EvalRunStatus    `bson:"status"      json:"status"`
	Cases      []EvalCaseResult `bson:"cases"       json:"cases"`

	// EvalVersion + EvalSnapshot capture the exact Eval document that
	// produced this run. Snapshot is independent of the live `Eval`
	// record (which gets mutated by future saves), so a year-old run
	// detail page still renders the original cases/assertions verbatim.
	EvalVersion  int  `bson:"eval_version,omitempty"  json:"eval_version,omitempty"`
	EvalSnapshot Eval `bson:"eval_snapshot,omitempty" json:"eval_snapshot,omitempty"`

	// Aggregate stats — populated when Status transitions to "done".
	PassCount    int     `bson:"pass_count"     json:"pass_count"`
	FailCount    int     `bson:"fail_count"     json:"fail_count"`
	ErrorCount   int     `bson:"error_count"    json:"error_count"`
	TotalCostUSD float64 `bson:"total_cost_usd" json:"total_cost_usd"`
	P50LatencyMs int64   `bson:"p50_latency_ms" json:"p50_latency_ms"`
	P95LatencyMs int64   `bson:"p95_latency_ms" json:"p95_latency_ms"`

	// Eval-level error message for status="error". Per-case errors live
	// on EvalCaseResult.Error.
	Error string `bson:"error,omitempty" json:"error,omitempty"`
}

// EvalRunStatus is the lifecycle state of a single eval execution.
type EvalRunStatus string

const (
	EvalRunStatusRunning EvalRunStatus = "running"
	EvalRunStatusDone    EvalRunStatus = "done"
	EvalRunStatusError   EvalRunStatus = "error"
)

// EvalCaseResult is the per-case outcome inside an EvalRun.
type EvalCaseResult struct {
	CaseID         string   `bson:"case_id"   json:"case_id"`
	CaseName       string   `bson:"case_name" json:"case_name"`
	WorkflowRunID  string   `bson:"workflow_run_id" json:"workflow_run_id"`
	Pass           bool     `bson:"pass"      json:"pass"`
	AssertionFails []string `bson:"assertion_fails,omitempty" json:"assertion_fails,omitempty"`
	Error          string   `bson:"error,omitempty" json:"error,omitempty"` // executor failure (separate from assertion failure)
	DurationMs     int64    `bson:"duration_ms" json:"duration_ms"`
	CostUSD        float64  `bson:"cost_usd"   json:"cost_usd"`
}

// EvalStore persists evals + their runs.
type EvalStore interface {
	// Eval CRUD
	UpsertEval(ctx context.Context, eval Eval) (Eval, error)
	GetEval(ctx context.Context, id string) (Eval, error)
	ListEvals(ctx context.Context, workflowID string) ([]Eval, error)
	DeleteEval(ctx context.Context, id string) error

	// EvalRun persistence
	CreateEvalRun(ctx context.Context, run EvalRun) (EvalRun, error)
	UpdateEvalRun(ctx context.Context, run EvalRun) error
	GetEvalRun(ctx context.Context, id string) (EvalRun, error)
	ListEvalRuns(ctx context.Context, evalID string, limit int) ([]EvalRun, error)
}
