package workflow

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/suite"
)

// stubWorkflowStore is a minimal in-memory WorkflowStore for guard
// tests — no Mongo dependency. Returns sentinel "not found" when
// the ID is missing so the dispatcher's lookup error path is
// exercisable.
type stubWorkflowStore struct {
	byID map[string]Workflow
}

func (s *stubWorkflowStore) GetByID(_ context.Context, id string) (Workflow, error) {
	if wf, ok := s.byID[id]; ok {
		return wf, nil
	}
	return Workflow{}, fmt.Errorf("not found: %s", id)
}

type SubWorkflowGuardSuite struct {
	suite.Suite
}

func (s *SubWorkflowGuardSuite) SetupSuite()    {}
func (s *SubWorkflowGuardSuite) TearDownSuite() {}
func (s *SubWorkflowGuardSuite) SetupTest()     {}
func (s *SubWorkflowGuardSuite) TearDownTest()  {}

// TestSubWorkflowGuardSuite is the test entrypoint for sub_workflow
// dispatch guards (tenant scoping, cycle detection, depth cap).
func TestSubWorkflowGuardSuite(t *testing.T) {
	suite.Run(t, new(SubWorkflowGuardSuite))
}

// TestDispatch_NoWorkflowStore_ReturnsError verifies that calling
// dispatchSubWorkflow on an executor without a WorkflowStore wired
// returns a clear error rather than nil-panicking. Without it the
// agent loop would crash the worker on the first sub_workflow tool
// call when deployments forget to wire the store.
func (s *SubWorkflowGuardSuite) TestDispatch_NoWorkflowStore_ReturnsError() {
	e := &WorkflowExecutor{}
	caller := &Workflow{ID: "wf_a", TenantID: "tenant_x"}
	_, err := e.dispatchSubWorkflow(context.Background(), caller, "wf_b", nil)
	s.Require().Error(err)
	s.Contains(err.Error(), "WorkflowStore not configured")
}

// TestDispatch_EmptyTargetID_ReturnsError verifies an empty target
// workflow_id is rejected before any lookup. Without this the call
// would Mongo-query for "" which depending on driver could match
// the wrong document.
func (s *SubWorkflowGuardSuite) TestDispatch_EmptyTargetID_ReturnsError() {
	e := &WorkflowExecutor{Workflows: &stubWorkflowStore{}}
	caller := &Workflow{ID: "wf_a", TenantID: "tenant_x"}
	_, err := e.dispatchSubWorkflow(context.Background(), caller, "", nil)
	s.Require().Error(err)
	s.Contains(err.Error(), "target workflow_id is empty")
}

// TestDispatch_NilCaller_ReturnsError verifies a nil caller workflow
// is rejected — without it we'd panic trying to read TenantID.
func (s *SubWorkflowGuardSuite) TestDispatch_NilCaller_ReturnsError() {
	e := &WorkflowExecutor{Workflows: &stubWorkflowStore{byID: map[string]Workflow{
		"wf_b": {ID: "wf_b", TenantID: "tenant_x"},
	}}}
	_, err := e.dispatchSubWorkflow(context.Background(), nil, "wf_b", nil)
	s.Require().Error(err)
	s.Contains(err.Error(), "caller workflow context missing")
}

// TestDispatch_CrossTenant_Refused verifies the tenant scoping
// invariant: a workflow in tenant X cannot dispatch a workflow in
// tenant Y even if it knows the ID. Multi-tenant isolation.
func (s *SubWorkflowGuardSuite) TestDispatch_CrossTenant_Refused() {
	e := &WorkflowExecutor{Workflows: &stubWorkflowStore{byID: map[string]Workflow{
		"wf_b": {ID: "wf_b", TenantID: "tenant_y"},
	}}}
	caller := &Workflow{ID: "wf_a", TenantID: "tenant_x"}
	_, err := e.dispatchSubWorkflow(context.Background(), caller, "wf_b", nil)
	s.Require().Error(err)
	s.Contains(err.Error(), "cross-tenant dispatch refused")
}

// TestDispatch_TargetInAncestors_DetectsCycle verifies a workflow
// already on the call chain can't be dispatched again — prevents
// A → B → A infinite recursion.
func (s *SubWorkflowGuardSuite) TestDispatch_TargetInAncestors_DetectsCycle() {
	e := &WorkflowExecutor{Workflows: &stubWorkflowStore{byID: map[string]Workflow{
		"wf_a": {ID: "wf_a", TenantID: "tenant_x"},
	}}}
	caller := &Workflow{ID: "wf_b", TenantID: "tenant_x"}
	ctx := withSubWorkflowAncestor(context.Background(), "wf_a")
	_, err := e.dispatchSubWorkflow(ctx, caller, "wf_a", nil)
	s.Require().Error(err)
	s.Contains(err.Error(), "cycle detected")
}

// TestDispatch_DepthCap_Enforced verifies that once the call chain
// reaches maxSubWorkflowDepth, further sub-calls are refused even
// when no cycle exists. Stops runaway nesting (and runaway LLM
// spend) from a misbehaving orchestrator.
func (s *SubWorkflowGuardSuite) TestDispatch_DepthCap_Enforced() {
	e := &WorkflowExecutor{Workflows: &stubWorkflowStore{byID: map[string]Workflow{
		"wf_target": {ID: "wf_target", TenantID: "tenant_x"},
	}}}
	caller := &Workflow{ID: "wf_caller", TenantID: "tenant_x"}
	ctx := context.Background()
	for i := 0; i < maxSubWorkflowDepth; i++ {
		ctx = withSubWorkflowAncestor(ctx, fmt.Sprintf("wf_anc_%d", i))
	}
	_, err := e.dispatchSubWorkflow(ctx, caller, "wf_target", nil)
	s.Require().Error(err)
	s.True(strings.Contains(err.Error(), "recursion depth"))
}

// TestSubWorkflowAncestor_PreservesOrder verifies the ancestor
// helper builds a defensive copy on each push (mutating one
// returned chain doesn't bleed back into the underlying ctx
// value), which a future deeper nesting would otherwise corrupt.
func (s *SubWorkflowGuardSuite) TestSubWorkflowAncestor_PreservesOrder() {
	ctx := context.Background()
	ctx = withSubWorkflowAncestor(ctx, "a")
	ctx = withSubWorkflowAncestor(ctx, "b")
	ctx = withSubWorkflowAncestor(ctx, "c")
	got := subWorkflowAncestors(ctx)
	s.Equal([]string{"a", "b", "c"}, got)

	// Mutating the returned slice must not corrupt the underlying.
	got[0] = "MUT"
	s.Equal([]string{"a", "b", "c"}, subWorkflowAncestors(ctx))
}
