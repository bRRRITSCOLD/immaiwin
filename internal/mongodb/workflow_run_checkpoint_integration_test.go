//go:build integration

// Workflow-run checkpoint + approval-decision integration tests —
// verifies the Phase 3 PR 3.2 persistence primitives:
//
//   - CheckpointExecutionState writes execution_state + steps under a
//     held lease, refuses writes from a foreign worker, and bumps
//     last_checkpoint_at on every successful pass.
//   - ApplyApprovalDecision writes the user's verdict into
//     execution_state.pending.decision, clears the held lease, flips
//     the run back to running, and is idempotent on a second call.
//
// Companion to workflow_run_lease_integration_test.go (PR 3.1) — both
// suites are required for the durable-execution worker loop to be safe
// to ship.

package mongodb_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/mongodb"
	"github.com/bRRRITSCOLD/burrow/internal/workflow"
	"github.com/stretchr/testify/suite"
	"go.mongodb.org/mongo-driver/v2/mongo"
	driveroptions "go.mongodb.org/mongo-driver/v2/mongo/options"
)

type WorkflowRunCheckpointIntegrationSuite struct {
	suite.Suite

	mongoClient *mongo.Client
	db          *mongo.Database
	dbName      string
	repo        *mongodb.WorkflowRunRepository
}

func TestWorkflowRunCheckpointIntegrationSuite(t *testing.T) {
	mongoURI := envOr("MONGO_URI", "mongodb://localhost:27017")

	probeCtx, probeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer probeCancel()

	mc, err := mongo.Connect(driveroptions.Client().ApplyURI(mongoURI))
	if err != nil {
		t.Fatalf("mongo connect failed (compose stack required): %v", err)
		return
	}
	if err := mc.Ping(probeCtx, nil); err != nil {
		_ = mc.Disconnect(context.Background())
		t.Fatalf("mongo unreachable at %s (compose stack required): %v", mongoURI, err)
		return
	}
	_ = mc.Disconnect(context.Background())
	suite.Run(t, new(WorkflowRunCheckpointIntegrationSuite))
}

func (s *WorkflowRunCheckpointIntegrationSuite) SetupSuite() {
	mongoURI := envOr("MONGO_URI", "mongodb://localhost:27017")
	c, err := mongo.Connect(driveroptions.Client().ApplyURI(mongoURI))
	s.Require().NoError(err)
	s.mongoClient = c
	s.dbName = fmt.Sprintf("burrow_test_checkpoint_%d", time.Now().UnixNano())
	s.db = c.Database(s.dbName)

	repo, err := mongodb.NewWorkflowRunRepository(context.Background(), s.db)
	s.Require().NoError(err)
	s.repo = repo
}

func (s *WorkflowRunCheckpointIntegrationSuite) TearDownSuite() {
	if s.db != nil {
		_ = s.db.Drop(context.Background())
	}
	if s.mongoClient != nil {
		_ = s.mongoClient.Disconnect(context.Background())
	}
}

func (s *WorkflowRunCheckpointIntegrationSuite) SetupTest() {
	_, _ = s.db.Collection("workflow_runs").DeleteMany(context.Background(), map[string]any{})
}

func (s *WorkflowRunCheckpointIntegrationSuite) TearDownTest() {}

// seedClaimedRun seeds a running workflow run + claims its lease for
// the given worker. Returns the loaded post-claim record.
func (s *WorkflowRunCheckpointIntegrationSuite) seedClaimedRun(id, workerID string) workflow.WorkflowRun {
	_, err := s.repo.Create(context.Background(), workflow.WorkflowRun{
		ID:         id,
		WorkflowID: "wf-1",
		TenantID:   "default",
		Status:     workflow.RunStatusRunning,
		QueuedAt:   time.Now().UTC(),
	})
	s.Require().NoError(err)
	rec, ok, err := s.repo.ClaimLease(context.Background(), workerID, 30*time.Second,
		[]workflow.RunStatus{workflow.RunStatusQueued, workflow.RunStatusRunning})
	s.Require().NoError(err)
	s.Require().True(ok, "claim must succeed on a freshly seeded run")
	return rec
}

// TestCheckpointExecutionState_LeaseHeld_WritesState verifies the happy
// path: a worker holding the lease writes ExecutionState + Steps and
// the document persists exactly what was written.
func (s *WorkflowRunCheckpointIntegrationSuite) TestCheckpointExecutionState_LeaseHeld_WritesState() {
	s.seedClaimedRun("run-cp-1", "worker-a")

	state := workflow.ExecutionState{
		Visited: []string{"trigger-1", "http-1"},
		Queue: []workflow.QueuedNode{
			{NodeID: "transform-1", Input: map[string]any{"foo": "bar"}},
		},
	}
	steps := []workflow.StepResult{
		{NodeID: "trigger-1", NodeType: workflow.NodeTypeTrigger, Output: nil},
		{NodeID: "http-1", NodeType: workflow.NodeTypeHTTPRequest, Output: "ok"},
	}

	err := s.repo.CheckpointExecutionState(context.Background(), "run-cp-1", "worker-a", state, steps, "")
	s.Require().NoError(err)

	got, err := s.repo.Get(context.Background(), "run-cp-1")
	s.Require().NoError(err)
	s.Require().NotNil(got.ExecutionState)
	s.Equal(state.Visited, got.ExecutionState.Visited)
	s.Require().Len(got.ExecutionState.Queue, 1)
	s.Equal("transform-1", got.ExecutionState.Queue[0].NodeID)
	s.Len(got.Steps, 2)
	s.NotNil(got.LastCheckpointAt, "last_checkpoint_at must be stamped on every checkpoint")
}

// TestCheckpointExecutionState_ForeignWorker_ReturnsErrLeaseNotHeld is
// the durability invariant — a worker without the lease cannot stomp
// the run state. Without this, two pods racing on a re-claimed run
// would clobber each other's progress.
func (s *WorkflowRunCheckpointIntegrationSuite) TestCheckpointExecutionState_ForeignWorker_ReturnsErrLeaseNotHeld() {
	s.seedClaimedRun("run-cp-2", "worker-a")

	state := workflow.ExecutionState{Visited: []string{"trigger-1"}}
	err := s.repo.CheckpointExecutionState(context.Background(), "run-cp-2", "worker-b", state, nil, "")
	s.Require().Error(err)
	s.ErrorIs(err, workflow.ErrLeaseNotHeld)
}

// TestCheckpointExecutionState_StatusUpdate_WritesNewStatus verifies
// the optional status passthrough — the BFS gate-yield path writes
// `pending_approval` alongside the state in the same call.
func (s *WorkflowRunCheckpointIntegrationSuite) TestCheckpointExecutionState_StatusUpdate_WritesNewStatus() {
	s.seedClaimedRun("run-cp-3", "worker-a")

	state := workflow.ExecutionState{
		Visited: []string{"trigger-1"},
		Pending: &workflow.PendingExecutionGate{NodeID: "approve-1", Input: nil},
	}
	err := s.repo.CheckpointExecutionState(context.Background(), "run-cp-3", "worker-a", state, nil, workflow.RunStatusPendingApproval)
	s.Require().NoError(err)

	got, err := s.repo.Get(context.Background(), "run-cp-3")
	s.Require().NoError(err)
	s.Equal(workflow.RunStatusPendingApproval, got.Status)
	s.Require().NotNil(got.ExecutionState)
	s.Require().NotNil(got.ExecutionState.Pending)
	s.Equal("approve-1", got.ExecutionState.Pending.NodeID)
}

// TestApplyApprovalDecision_PendingPresent_WritesDecisionAndClearsLease
// verifies the full approval-handler side-effects: decision lands in
// state, status flips back to running, lease cleared so a worker can
// claim. This is the bridge between the API process (handler) and the
// worker process (resume).
func (s *WorkflowRunCheckpointIntegrationSuite) TestApplyApprovalDecision_PendingPresent_WritesDecisionAndClearsLease() {
	rec := s.seedClaimedRun("run-cp-4", "worker-a")

	// First, get the run into a yielded shape: status=pending_approval,
	// execution_state.pending populated.
	state := workflow.ExecutionState{
		Visited: []string{"trigger-1"},
		Queue:   []workflow.QueuedNode{{NodeID: "approve-1"}},
		Pending: &workflow.PendingExecutionGate{NodeID: "approve-1", Input: "input-payload"},
	}
	err := s.repo.CheckpointExecutionState(context.Background(), rec.ID, "worker-a", state, nil, workflow.RunStatusPendingApproval)
	s.Require().NoError(err)

	// Apply user's approval.
	err = s.repo.ApplyApprovalDecision(context.Background(), rec.ID, workflow.ApprovalDecision{
		Approved: true,
		Reason:   "looks good",
	})
	s.Require().NoError(err)

	got, err := s.repo.Get(context.Background(), rec.ID)
	s.Require().NoError(err)
	s.Equal(workflow.RunStatusRunning, got.Status, "approved gate flips back to running so the worker can claim")
	s.Equal("", got.LeaseOwner, "lease must be cleared so a worker can claim")
	s.Nil(got.LeaseExpiresAt)
	s.Require().NotNil(got.ExecutionState)
	s.Require().NotNil(got.ExecutionState.Pending)
	s.Require().NotNil(got.ExecutionState.Pending.Decision)
	s.True(got.ExecutionState.Pending.Decision.Approved)
	s.Equal("looks good", got.ExecutionState.Pending.Decision.Reason)
}

// TestApplyApprovalDecision_NoPending_NoOp protects against duplicate
// clicks: the second click lands on a run whose pending was already
// cleared by the worker. We want a quiet no-op, not an error.
func (s *WorkflowRunCheckpointIntegrationSuite) TestApplyApprovalDecision_NoPending_NoOp() {
	_, err := s.repo.Create(context.Background(), workflow.WorkflowRun{
		ID:         "run-cp-5",
		WorkflowID: "wf-1",
		TenantID:   "default",
		Status:     workflow.RunStatusRunning,
		QueuedAt:   time.Now().UTC(),
	})
	s.Require().NoError(err)

	err = s.repo.ApplyApprovalDecision(context.Background(), "run-cp-5", workflow.ApprovalDecision{Approved: true})
	s.Require().NoError(err, "no-pending apply must not error so duplicate clicks stay silent")

	got, err := s.repo.Get(context.Background(), "run-cp-5")
	s.Require().NoError(err)
	s.Equal(workflow.RunStatusRunning, got.Status, "status unchanged when no pending gate exists")
	s.Nil(got.ExecutionState, "execution_state stays nil when nothing to apply")
}

// TestApplyApprovalDecision_RejectsCarriesReason verifies the reject
// path: decision lands with approved=false + the reason, available to
// the resuming worker for routing the rejection error edge.
func (s *WorkflowRunCheckpointIntegrationSuite) TestApplyApprovalDecision_RejectsCarriesReason() {
	rec := s.seedClaimedRun("run-cp-6", "worker-a")

	state := workflow.ExecutionState{
		Pending: &workflow.PendingExecutionGate{NodeID: "approve-1"},
	}
	err := s.repo.CheckpointExecutionState(context.Background(), rec.ID, "worker-a", state, nil, workflow.RunStatusPendingApproval)
	s.Require().NoError(err)

	err = s.repo.ApplyApprovalDecision(context.Background(), rec.ID, workflow.ApprovalDecision{
		Approved: false,
		Reason:   "missing budget signoff",
	})
	s.Require().NoError(err)

	got, err := s.repo.Get(context.Background(), rec.ID)
	s.Require().NoError(err)
	s.Require().NotNil(got.ExecutionState)
	s.Require().NotNil(got.ExecutionState.Pending)
	s.Require().NotNil(got.ExecutionState.Pending.Decision)
	s.False(got.ExecutionState.Pending.Decision.Approved)
	s.Equal("missing budget signoff", got.ExecutionState.Pending.Decision.Reason)
}
