//go:build integration

// Boot-sweep integration tests — verifies SweepAbandonedNonTerminal
// honors the lease + execution_state gates added in PR 3.5. The boot
// sweep used to flip every non-terminal run on api restart, which
// killed durable lease-held runs alongside the legacy orphans it was
// meant to clean up. After 3.5, the sweep only touches runs that
// have neither a live lease NOR a persisted ExecutionState.

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

type WorkflowRunSweepIntegrationSuite struct {
	suite.Suite

	mongoClient *mongo.Client
	db          *mongo.Database
	dbName      string
	repo        *mongodb.WorkflowRunRepository
}

func TestWorkflowRunSweepIntegrationSuite(t *testing.T) {
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
	suite.Run(t, new(WorkflowRunSweepIntegrationSuite))
}

func (s *WorkflowRunSweepIntegrationSuite) SetupSuite() {
	mongoURI := envOr("MONGO_URI", "mongodb://localhost:27017")
	c, err := mongo.Connect(driveroptions.Client().ApplyURI(mongoURI))
	s.Require().NoError(err)
	s.mongoClient = c
	s.dbName = fmt.Sprintf("burrow_test_sweep_%d", time.Now().UnixNano())
	s.db = c.Database(s.dbName)

	repo, err := mongodb.NewWorkflowRunRepository(context.Background(), s.db)
	s.Require().NoError(err)
	s.repo = repo
}

func (s *WorkflowRunSweepIntegrationSuite) TearDownSuite() {
	if s.db != nil {
		_ = s.db.Drop(context.Background())
	}
	if s.mongoClient != nil {
		_ = s.mongoClient.Disconnect(context.Background())
	}
}

func (s *WorkflowRunSweepIntegrationSuite) SetupTest() {
	_, _ = s.db.Collection("workflow_runs").DeleteMany(context.Background(), map[string]any{})
}

func (s *WorkflowRunSweepIntegrationSuite) TearDownTest() {}

// statusOf re-reads the run by ID and returns its current status —
// the sweep mutates in place, so each assertion fetches fresh.
func (s *WorkflowRunSweepIntegrationSuite) statusOf(id string) workflow.RunStatus {
	r, err := s.repo.Get(context.Background(), id)
	s.Require().NoError(err)
	return r.Status
}

// TestSweepAbandonedNonTerminal_LegacyInProcessOrphan_FlipsToError
// verifies the canonical "what the sweep is for" case: a non-terminal
// run with no lease and no execution_state, older than the safety
// window. Pre-3.5 this was already flipped; post-3.5 it must still
// flip — the gates only protect lease/durable runs, not legacy
// orphans.
func (s *WorkflowRunSweepIntegrationSuite) TestSweepAbandonedNonTerminal_LegacyInProcessOrphan_FlipsToError() {
	ctx := context.Background()
	old := time.Now().UTC().Add(-2 * time.Minute)
	_, err := s.repo.Create(ctx, workflow.WorkflowRun{
		ID:         "legacy-orphan",
		WorkflowID: "wf-1",
		TenantID:   "default",
		Status:     workflow.RunStatusRunning,
		QueuedAt:   old,
	})
	s.Require().NoError(err)

	count, err := s.repo.SweepAbandonedNonTerminal(ctx, "boot test", 30*time.Second)
	s.Require().NoError(err)
	s.Equal(int64(1), count)
	s.Equal(workflow.RunStatusError, s.statusOf("legacy-orphan"))
}

// TestSweepAbandonedNonTerminal_LiveLease_Survives is the core PR-3.5
// regression: a run currently held by a heartbeating worker
// (lease_owner set, lease_expires_at in the future) must survive an
// api boot. Without this gate, restarting the api process kills every
// in-flight lease run.
func (s *WorkflowRunSweepIntegrationSuite) TestSweepAbandonedNonTerminal_LiveLease_Survives() {
	ctx := context.Background()
	old := time.Now().UTC().Add(-2 * time.Minute)
	future := time.Now().UTC().Add(30 * time.Second)
	_, err := s.repo.Create(ctx, workflow.WorkflowRun{
		ID:             "leased-alive",
		WorkflowID:     "wf-1",
		TenantID:       "default",
		Status:         workflow.RunStatusRunning,
		QueuedAt:       old,
		LeaseOwner:     "worker-a",
		LeaseExpiresAt: &future,
	})
	s.Require().NoError(err)

	count, err := s.repo.SweepAbandonedNonTerminal(ctx, "boot test", 30*time.Second)
	s.Require().NoError(err)
	s.Equal(int64(0), count, "live-leased run must be skipped — flipping it would race with the heartbeating worker")
	s.Equal(workflow.RunStatusRunning, s.statusOf("leased-alive"))
}

// TestSweepAbandonedNonTerminal_ExpiredLeaseButDurable_Survives
// covers the "both api and worker died" case: lease has expired
// (worker is gone) but execution_state is non-nil (BFS progress is
// committed). The next ClaimLease tick rehydrates and resumes; the
// boot sweep must NOT flip it to error — that would discard real
// work that was already checkpointed.
func (s *WorkflowRunSweepIntegrationSuite) TestSweepAbandonedNonTerminal_ExpiredLeaseButDurable_Survives() {
	ctx := context.Background()
	old := time.Now().UTC().Add(-2 * time.Minute)
	past := time.Now().UTC().Add(-10 * time.Second)
	_, err := s.repo.Create(ctx, workflow.WorkflowRun{
		ID:             "leased-expired-durable",
		WorkflowID:     "wf-1",
		TenantID:       "default",
		Status:         workflow.RunStatusRunning,
		QueuedAt:       old,
		LeaseOwner:     "worker-dead",
		LeaseExpiresAt: &past,
		ExecutionState: &workflow.ExecutionState{
			Visited: []string{"node-1"},
			Queue: []workflow.QueuedNode{
				{NodeID: "node-2", Input: "carryover"},
			},
		},
	})
	s.Require().NoError(err)

	count, err := s.repo.SweepAbandonedNonTerminal(ctx, "boot test", 30*time.Second)
	s.Require().NoError(err)
	s.Equal(int64(0), count, "durable run (execution_state set) must be skipped; ClaimLease will rehydrate it")
	s.Equal(workflow.RunStatusRunning, s.statusOf("leased-expired-durable"))
}

// TestSweepAbandonedNonTerminal_NoLeaseButDurable_Survives is the
// less-common variant of the durable case: a yield-path run that
// released its lease at the gate (lease_owner cleared by
// ApplyApprovalDecision) but has execution_state.pending awaiting
// the next claim. The sweep used to take this out because the lease
// was gone; post-3.5 the execution_state gate keeps it alive.
func (s *WorkflowRunSweepIntegrationSuite) TestSweepAbandonedNonTerminal_NoLeaseButDurable_Survives() {
	ctx := context.Background()
	old := time.Now().UTC().Add(-2 * time.Minute)
	_, err := s.repo.Create(ctx, workflow.WorkflowRun{
		ID:         "yielded-pending",
		WorkflowID: "wf-1",
		TenantID:   "default",
		Status:     workflow.RunStatusPendingApproval,
		QueuedAt:   old,
		// No lease — yield path released it.
		ExecutionState: &workflow.ExecutionState{
			Visited: []string{"trigger", "agent"},
			Pending: &workflow.PendingExecutionGate{
				Kind:   "node",
				NodeID: "next-node",
			},
		},
	})
	s.Require().NoError(err)

	count, err := s.repo.SweepAbandonedNonTerminal(ctx, "boot test", 30*time.Second)
	s.Require().NoError(err)
	s.Equal(int64(0), count, "yielded-and-waiting durable run must be skipped — the next claim post-approval will resume it")
	s.Equal(workflow.RunStatusPendingApproval, s.statusOf("yielded-pending"))
}

// TestSweepAbandonedNonTerminal_RecentNonTerminal_Survives verifies
// the safety window still works post-3.5: a fresh non-terminal run
// (no lease, no execution_state, but queued_at within the window) is
// preserved as defense in depth against stomping a parallel api
// pid's brand-new dispatch.
func (s *WorkflowRunSweepIntegrationSuite) TestSweepAbandonedNonTerminal_RecentNonTerminal_Survives() {
	ctx := context.Background()
	recent := time.Now().UTC().Add(-1 * time.Second)
	_, err := s.repo.Create(ctx, workflow.WorkflowRun{
		ID:         "fresh-orphan",
		WorkflowID: "wf-1",
		TenantID:   "default",
		Status:     workflow.RunStatusRunning,
		QueuedAt:   recent,
	})
	s.Require().NoError(err)

	count, err := s.repo.SweepAbandonedNonTerminal(ctx, "boot test", 30*time.Second)
	s.Require().NoError(err)
	s.Equal(int64(0), count, "queued_at within safety window — preserved by the cutoff predicate")
	s.Equal(workflow.RunStatusRunning, s.statusOf("fresh-orphan"))
}

// TestSweepAbandonedNonTerminal_TerminalRuns_Untouched verifies the
// status filter — terminal records must never be flipped, regardless
// of lease / execution_state shape.
func (s *WorkflowRunSweepIntegrationSuite) TestSweepAbandonedNonTerminal_TerminalRuns_Untouched() {
	ctx := context.Background()
	old := time.Now().UTC().Add(-5 * time.Minute)
	for _, st := range []workflow.RunStatus{
		workflow.RunStatusSuccess,
		workflow.RunStatusError,
		workflow.RunStatusCancelled,
	} {
		id := "terminal-" + string(st)
		_, err := s.repo.Create(ctx, workflow.WorkflowRun{
			ID:         id,
			WorkflowID: "wf-1",
			TenantID:   "default",
			Status:     st,
			QueuedAt:   old,
		})
		s.Require().NoError(err)
	}

	count, err := s.repo.SweepAbandonedNonTerminal(ctx, "boot test", 30*time.Second)
	s.Require().NoError(err)
	s.Equal(int64(0), count)

	s.Equal(workflow.RunStatusSuccess, s.statusOf("terminal-success"))
	s.Equal(workflow.RunStatusError, s.statusOf("terminal-error"))
	s.Equal(workflow.RunStatusCancelled, s.statusOf("terminal-cancelled"))
}

// TestSweepAbandonedNonTerminal_MixedFleet_FlipsOnlyOrphans is the
// realistic post-restart scenario: a mix of lease-held, durable, and
// legacy in-process runs together. Only the legacy orphans should
// flip; the rest must survive intact.
func (s *WorkflowRunSweepIntegrationSuite) TestSweepAbandonedNonTerminal_MixedFleet_FlipsOnlyOrphans() {
	ctx := context.Background()
	old := time.Now().UTC().Add(-2 * time.Minute)
	future := time.Now().UTC().Add(30 * time.Second)
	past := time.Now().UTC().Add(-10 * time.Second)

	mustCreate := func(run workflow.WorkflowRun) {
		_, err := s.repo.Create(ctx, run)
		s.Require().NoError(err)
	}

	// Two legacy orphans (should flip).
	mustCreate(workflow.WorkflowRun{
		ID: "orphan-1", WorkflowID: "wf-1", TenantID: "default",
		Status: workflow.RunStatusRunning, QueuedAt: old,
	})
	mustCreate(workflow.WorkflowRun{
		ID: "orphan-2", WorkflowID: "wf-1", TenantID: "default",
		Status: workflow.RunStatusQueued, QueuedAt: old,
	})

	// Live lease (should survive).
	mustCreate(workflow.WorkflowRun{
		ID: "live-1", WorkflowID: "wf-1", TenantID: "default",
		Status: workflow.RunStatusRunning, QueuedAt: old,
		LeaseOwner: "worker-x", LeaseExpiresAt: &future,
	})

	// Expired lease + execution_state (should survive).
	mustCreate(workflow.WorkflowRun{
		ID: "durable-1", WorkflowID: "wf-1", TenantID: "default",
		Status: workflow.RunStatusRunning, QueuedAt: old,
		LeaseOwner: "worker-dead", LeaseExpiresAt: &past,
		ExecutionState: &workflow.ExecutionState{
			Visited: []string{"trigger"},
			Queue:   []workflow.QueuedNode{{NodeID: "next"}},
		},
	})

	count, err := s.repo.SweepAbandonedNonTerminal(ctx, "boot test", 30*time.Second)
	s.Require().NoError(err)
	s.Equal(int64(2), count, "exactly the two legacy orphans flip")

	s.Equal(workflow.RunStatusError, s.statusOf("orphan-1"))
	s.Equal(workflow.RunStatusError, s.statusOf("orphan-2"))
	s.Equal(workflow.RunStatusRunning, s.statusOf("live-1"))
	s.Equal(workflow.RunStatusRunning, s.statusOf("durable-1"))
}
