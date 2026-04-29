package workflow

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// defaultEvalConcurrency caps parallel case execution. Picked low (4) so
// a 50-case eval doesn't fan out and trip provider rate limits — the
// executor's per-workflow cost cap also gates total spend, but rate
// limit / 429s come back as opaque errors and are harder to recover.
const defaultEvalConcurrency = 4

// EvalRunner executes Evals against the workflow store + executor and
// persists each EvalRun's per-case results. Wired in `cmd/api/main.go`
// the same way `WorkflowExecutor` is — one shared instance, threaded
// into the HTTP handler.
type EvalRunner struct {
	Evals       EvalStore
	Workflows   WorkflowStore // for fetching the workflow doc per run
	Executor    *WorkflowExecutor
	Concurrency int // optional override; defaults to defaultEvalConcurrency
}

// WorkflowStore is a tiny narrowing of `handler.WorkflowStore` so this
// package doesn't pull the api/handler package back into the workflow
// layer. Implemented by `mongodb.WorkflowRepository`.
type WorkflowStore interface {
	GetByID(ctx context.Context, id string) (Workflow, error)
}

// Run executes every case in `eval` (bounded parallel) and returns the
// completed EvalRun record, already persisted. Caller-provided ctx
// scopes the run; cancelling it aborts in-flight cases.
func (r *EvalRunner) Run(ctx context.Context, eval Eval) (EvalRun, error) {
	if r.Evals == nil || r.Workflows == nil || r.Executor == nil {
		return EvalRun{}, fmt.Errorf("eval runner: missing dependency (evals=%v workflows=%v executor=%v)",
			r.Evals != nil, r.Workflows != nil, r.Executor != nil)
	}
	wf, err := r.Workflows.GetByID(ctx, eval.WorkflowID)
	if err != nil {
		return EvalRun{}, fmt.Errorf("eval runner: workflow %q: %w", eval.WorkflowID, err)
	}

	run := EvalRun{
		ID:         ulid.Make().String(),
		EvalID:     eval.ID,
		WorkflowID: eval.WorkflowID,
		StartedAt:  time.Now().UTC(),
		Status:     EvalRunStatusRunning,
		Cases:      make([]EvalCaseResult, len(eval.Cases)),
		// Snapshot the eval doc as it existed at run-time so historical
		// runs survive future edits to the live Eval record.
		EvalVersion:  eval.Version,
		EvalSnapshot: eval,
	}
	if _, err := r.Evals.CreateEvalRun(ctx, run); err != nil {
		return EvalRun{}, fmt.Errorf("eval runner: persist run: %w", err)
	}

	concurrency := r.Concurrency
	if concurrency <= 0 {
		concurrency = defaultEvalConcurrency
	}

	// Bounded worker pool. sem buffer = concurrency cap; each goroutine
	// pushes once, pops once. WaitGroup tracks completion.
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	results := make([]EvalCaseResult, len(eval.Cases))

	for i := range eval.Cases {
		i := i // capture
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = r.runOneCase(ctx, wf, eval.Cases[i])
		}()
	}
	wg.Wait()

	run.Cases = results
	finalize(&run)
	finished := time.Now().UTC()
	run.FinishedAt = &finished
	run.Status = EvalRunStatusDone

	if err := r.Evals.UpdateEvalRun(ctx, run); err != nil {
		// Don't lose the in-memory results — return them even if the
		// final persist failed. UI can still render from the value the
		// handler echoes back.
		slog.Warn("eval runner: persist final run failed", "run_id", run.ID, "err", err)
	}
	return run, nil
}

// runOneCase executes a single case via RunResumable, then evaluates
// the case's assertions against the resulting run's agent output +
// step results. Errors at the executor layer (workflow not found,
// connection failure, etc.) populate `Error` on the case result and
// flag it as a non-pass; assertion failures populate `AssertionFails`.
func (r *EvalRunner) runOneCase(ctx context.Context, wf Workflow, c EvalCase) EvalCaseResult {
	res := EvalCaseResult{CaseID: c.ID, CaseName: c.Name}
	start := time.Now()

	// Param overrides — clone the workflow per case so the executor's
	// caller doesn't mutate shared state. Case params override workflow
	// params on key collision. Struct copy is sufficient because Nodes
	// + Edges are slices and we don't mutate their contents downstream.
	caseWF := wf
	if len(c.Params) > 0 {
		merged := make(map[string]string, len(wf.Params)+len(c.Params))
		for k, v := range wf.Params {
			merged[k] = v
		}
		for k, v := range c.Params {
			merged[k] = v
		}
		caseWF.Params = merged
	}

	outcome, err := r.Executor.RunResumable(ctx, caseWF, RunOpts{
		Input: c.Input,
	})
	res.DurationMs = time.Since(start).Milliseconds()
	res.WorkflowRunID = outcome.RunID

	if err != nil {
		res.Pass = false
		res.Error = err.Error()
		return res
	}

	// Stitch the agent output + step lookup the assertions evaluate
	// against. Agent output is the final ai_agent node's `output` field;
	// fall back to the last step's output when no agent is present
	// (single-shot non-agent workflows can still be evaluated this way).
	stepsByID := make(map[string]StepResult, len(outcome.Steps))
	var agentOutput any
	for _, s := range outcome.Steps {
		stepsByID[s.NodeID] = s
		if s.NodeType == NodeTypeAIAgent {
			agentOutput = s.Output
		}
	}
	if agentOutput == nil && len(outcome.Steps) > 0 {
		agentOutput = outcome.Steps[len(outcome.Steps)-1].Output
	}

	fails := evaluateAssertions(c.Assertions, agentOutput, stepsByID)
	res.AssertionFails = fails
	res.Pass = len(fails) == 0

	// Cost: pull from the persisted WorkflowRun (executor finalised it).
	if outcome.RunID != "" && r.Executor.RunRepo != nil {
		if rec, gerr := r.Executor.RunRepo.Get(ctx, outcome.RunID); gerr == nil {
			res.CostUSD = rec.Usage.CostUSD
		}
	}
	return res
}

// finalize computes pass/fail/error counts + p50/p95 latency + total
// cost on the EvalRun in-place.
func finalize(run *EvalRun) {
	durations := make([]int64, 0, len(run.Cases))
	for _, c := range run.Cases {
		switch {
		case c.Error != "":
			run.ErrorCount++
		case c.Pass:
			run.PassCount++
		default:
			run.FailCount++
		}
		run.TotalCostUSD += c.CostUSD
		if c.DurationMs > 0 {
			durations = append(durations, c.DurationMs)
		}
	}
	if len(durations) == 0 {
		return
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	run.P50LatencyMs = pct(durations, 0.50)
	run.P95LatencyMs = pct(durations, 0.95)
}

// pct returns the requested percentile from a sorted slice. Uses the
// nearest-rank method (good-enough for ops dashboards; precise enough
// for sub-100-case evals where spline interpolation is overkill).
func pct(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)) * p)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
