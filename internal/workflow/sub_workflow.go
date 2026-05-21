// Package workflow — sub-workflow dispatch (agent calls another
// workflow as a tool).
//
// A `sub_workflow` node is an `as_tool` target on an agent. Its
// handler:
//
//  1. Reads target workflow ID from `data.workflow_id`.
//  2. Looks the workflow up via WorkflowStore.
//  3. Tenant-scopes: refuses cross-tenant dispatch.
//  4. Recursion + cycle guards: tracks the call chain on ctx,
//     refuses if the target is already an ancestor or the chain
//     would exceed maxSubWorkflowDepth.
//  5. Dispatches the sub-run via the SAME executor (e.RunWithEvents)
//     with the tool args as initial input.
//  6. Returns the final StepResult's output as the tool result.
//
// The sub-run shares ctx + the same EventEmitter as the parent —
// canvas viewers see sub-run nodes light up alongside the parent
// agent. Lease, checkpoint, breakpoints all flow through normally
// (the parent run already owns the lease; the sub-run runs inside
// the parent's run-pass goroutine).

package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
)

// maxSubWorkflowDepth caps how deep nested sub-workflow calls
// can go. 5 is generous for legitimate orchestrator-of-
// orchestrators patterns and tight enough that a misbehaving
// agent loop can't blow the call stack or the LLM budget.
const maxSubWorkflowDepth = 5

// subWorkflowAncestorsKey threads the call chain through ctx.
// Each entry is the workflow ID of a caller currently on the
// stack. The active sub-run's own workflow ID is NOT in the list
// — only its ancestors are.
type subWorkflowAncestorsKey struct{}

// subWorkflowAncestors returns the current call chain (oldest
// caller first). Returns a defensive copy so callers can use
// `append` / mutate without corrupting the ctx value (the
// underlying slice would otherwise be shared across every ctx
// derived from the same parent).
func subWorkflowAncestors(ctx context.Context) []string {
	v, ok := ctx.Value(subWorkflowAncestorsKey{}).([]string)
	if !ok || len(v) == 0 {
		return nil
	}
	out := make([]string, len(v))
	copy(out, v)
	return out
}

// withSubWorkflowAncestor appends the caller's workflow ID to the
// chain. The returned ctx is what the sub-run sees.
func withSubWorkflowAncestor(ctx context.Context, callerWfID string) context.Context {
	ancestors := subWorkflowAncestors(ctx)
	next := make([]string, 0, len(ancestors)+1)
	next = append(next, ancestors...)
	next = append(next, callerWfID)
	return context.WithValue(ctx, subWorkflowAncestorsKey{}, next)
}

// dispatchSubWorkflow runs the workflow named by `targetWfID` as
// the calling agent's tool. Returns a JSON-stringified output
// suitable for feeding back into the LLM as a tool_result.
//
// Guards (in order):
//   - WorkflowStore wired (else: "sub_workflow tool unavailable")
//   - targetWfID non-empty
//   - workflow exists
//   - same tenant as caller
//   - target NOT already on the call chain (cycle guard)
//   - depth < maxSubWorkflowDepth
func (e *WorkflowExecutor) dispatchSubWorkflow(
	ctx context.Context,
	callerWf *Workflow,
	targetWfID string,
	toolArgs any,
) (string, error) {
	if e.Workflows == nil {
		return "", fmt.Errorf("sub_workflow: WorkflowStore not configured")
	}
	if targetWfID == "" {
		return "", fmt.Errorf("sub_workflow: target workflow_id is empty")
	}
	if callerWf == nil {
		return "", fmt.Errorf("sub_workflow: caller workflow context missing")
	}

	ancestors := subWorkflowAncestors(ctx)
	if slices.Contains(ancestors, targetWfID) {
		return "", fmt.Errorf("sub_workflow: cycle detected — target %s is already in the call chain %v", targetWfID, ancestors)
	}
	if len(ancestors) >= maxSubWorkflowDepth {
		return "", fmt.Errorf("sub_workflow: recursion depth %d exceeded (max %d)", len(ancestors), maxSubWorkflowDepth)
	}

	subWf, err := e.Workflows.GetByID(ctx, targetWfID)
	if err != nil {
		return "", fmt.Errorf("sub_workflow: lookup %s: %w", targetWfID, err)
	}
	if subWf.TenantID != callerWf.TenantID {
		// Cross-tenant calls are refused even if the caller knew the
		// target ID. Multi-tenant isolation invariant.
		return "", fmt.Errorf("sub_workflow: cross-tenant dispatch refused (caller=%s target=%s)", callerWf.TenantID, subWf.TenantID)
	}

	// Sub-run shares the parent's emitter so canvas viewers see the
	// nested nodes light up. Passing nil falls back to e.Events,
	// which is what the parent had. The ctx wrap propagates the
	// call chain so deeper sub-calls can detect cycles back to us.
	subCtx := withSubWorkflowAncestor(ctx, callerWf.ID)

	// nil stopAtIDs — sub-runs don't honor parent breakpoints; the
	// agent gets a single tool call out, sub completes inline.
	results, err := e.RunWithEvents(subCtx, subWf, nil, e.Events, toolArgs)
	if err != nil {
		return "", fmt.Errorf("sub_workflow %s: %w", targetWfID, err)
	}
	if len(results) == 0 {
		return "{}", nil
	}
	final := results[len(results)-1].Output
	encoded, err := json.Marshal(final)
	if err != nil {
		return fmt.Sprintf("%v", final), nil
	}
	return string(encoded), nil
}
