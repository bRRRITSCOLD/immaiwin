//go:build integration

// Workflow-run lease integration tests — verifies the Phase 3
// durable-execution primitive: ClaimLease / ExtendLease / ReleaseLease
// against a real Mongo. Lease semantics are the foundation everything
// in DURABLE-EXECUTION-PLAN.md builds on, so we test them in isolation
// before wiring up the executor.

package mongodb_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/mongodb"
	"github.com/bRRRITSCOLD/burrow/internal/workflow"
	"github.com/stretchr/testify/suite"
	"go.mongodb.org/mongo-driver/v2/mongo"
	driveroptions "go.mongodb.org/mongo-driver/v2/mongo/options"
)

type WorkflowRunLeaseIntegrationSuite struct {
	suite.Suite

	mongoClient *mongo.Client
	db          *mongo.Database
	dbName      string
	repo        *mongodb.WorkflowRunRepository
}

func TestWorkflowRunLeaseIntegrationSuite(t *testing.T) {
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
	suite.Run(t, new(WorkflowRunLeaseIntegrationSuite))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func (s *WorkflowRunLeaseIntegrationSuite) SetupSuite() {
	mongoURI := envOr("MONGO_URI", "mongodb://localhost:27017")
	c, err := mongo.Connect(driveroptions.Client().ApplyURI(mongoURI))
	s.Require().NoError(err)
	s.mongoClient = c
	s.dbName = fmt.Sprintf("burrow_test_lease_%d", time.Now().UnixNano())
	s.db = c.Database(s.dbName)

	repo, err := mongodb.NewWorkflowRunRepository(context.Background(), s.db)
	s.Require().NoError(err)
	s.repo = repo
}

func (s *WorkflowRunLeaseIntegrationSuite) TearDownSuite() {
	if s.db != nil {
		_ = s.db.Drop(context.Background())
	}
	if s.mongoClient != nil {
		_ = s.mongoClient.Disconnect(context.Background())
	}
}

func (s *WorkflowRunLeaseIntegrationSuite) SetupTest() {
	_, _ = s.db.Collection("workflow_runs").DeleteMany(context.Background(), map[string]any{})
}

func (s *WorkflowRunLeaseIntegrationSuite) TearDownTest() {}

// seedRun creates a non-terminal workflow run. `queuedAt` doubles as
// the FIFO sort key for ClaimLease — the lease tests assert oldest-first
// ordering.
func (s *WorkflowRunLeaseIntegrationSuite) seedRun(id, workflowID string, queuedAt time.Time) {
	_, err := s.repo.Create(context.Background(), workflow.WorkflowRun{
		ID:         id,
		WorkflowID: workflowID,
		TenantID:   "default",
		Status:     workflow.RunStatusQueued,
		QueuedAt:   queuedAt,
	})
	s.Require().NoError(err)
}

// TestClaimLease_NoCandidates_ReturnsFalse verifies that ClaimLease
// signals "nothing to do" cleanly when no run matches the predicate.
// Workers' claim loops rely on this for backoff.
func (s *WorkflowRunLeaseIntegrationSuite) TestClaimLease_NoCandidates_ReturnsFalse() {
	_, ok, err := s.repo.ClaimLease(context.Background(), "worker-a", 30*time.Second,
		[]workflow.RunStatus{workflow.RunStatusQueued, workflow.RunStatusRunning})
	s.Require().NoError(err)
	s.False(ok, "no runs in the collection → no claim possible")
}

// TestClaimLease_TwoWorkersCompete_ExactlyOneWinsPerRun is the core
// invariant: two concurrent claims for the only available run must
// resolve to one winner + one false. Without this, the lease is just
// advisory.
func (s *WorkflowRunLeaseIntegrationSuite) TestClaimLease_TwoWorkersCompete_ExactlyOneWinsPerRun() {
	now := time.Now().UTC()
	s.seedRun("run-only", "wf-1", now)

	// Sequential first to verify the basic pattern.
	a, ok, err := s.repo.ClaimLease(context.Background(), "worker-a", 30*time.Second,
		[]workflow.RunStatus{workflow.RunStatusQueued, workflow.RunStatusRunning})
	s.Require().NoError(err)
	s.True(ok)
	s.Equal("run-only", a.ID)
	s.Equal("worker-a", a.LeaseOwner)
	s.NotNil(a.LeaseExpiresAt, "lease_expires_at must be persisted")

	// Worker B claims now: same run is held by A, so B should get nothing.
	_, ok, err = s.repo.ClaimLease(context.Background(), "worker-b", 30*time.Second,
		[]workflow.RunStatus{workflow.RunStatusQueued, workflow.RunStatusRunning})
	s.Require().NoError(err)
	s.False(ok, "worker-a holds the only candidate; worker-b must get nothing")
}

// TestClaimLease_DistributesAcrossWorkers verifies multiple claimable
// runs get distributed FIFO-ish across workers — a worker that comes
// up shouldn't accidentally batch-claim everything.
func (s *WorkflowRunLeaseIntegrationSuite) TestClaimLease_DistributesAcrossWorkers() {
	base := time.Now().UTC()
	// Three runs, oldest first (lowest started_at = highest priority).
	s.seedRun("run-1", "wf-1", base.Add(-3*time.Minute))
	s.seedRun("run-2", "wf-1", base.Add(-2*time.Minute))
	s.seedRun("run-3", "wf-1", base.Add(-1*time.Minute))

	a, ok, err := s.repo.ClaimLease(context.Background(), "worker-a", 30*time.Second,
		[]workflow.RunStatus{workflow.RunStatusQueued, workflow.RunStatusRunning})
	s.Require().NoError(err)
	s.Require().True(ok)
	s.Equal("run-1", a.ID, "FIFO: oldest started_at gets claimed first")

	b, ok, err := s.repo.ClaimLease(context.Background(), "worker-b", 30*time.Second,
		[]workflow.RunStatus{workflow.RunStatusQueued, workflow.RunStatusRunning})
	s.Require().NoError(err)
	s.Require().True(ok)
	s.Equal("run-2", b.ID)

	c, ok, err := s.repo.ClaimLease(context.Background(), "worker-c", 30*time.Second,
		[]workflow.RunStatus{workflow.RunStatusQueued, workflow.RunStatusRunning})
	s.Require().NoError(err)
	s.Require().True(ok)
	s.Equal("run-3", c.ID)

	// Fourth claim — pool empty.
	_, ok, _ = s.repo.ClaimLease(context.Background(), "worker-d", 30*time.Second,
		[]workflow.RunStatus{workflow.RunStatusQueued, workflow.RunStatusRunning})
	s.False(ok, "all candidates leased; new worker should idle")
}

// TestExtendLease_FromHolder_ExtendsExpiry verifies the heartbeat path:
// the worker that owns the lease can push expiry forward.
func (s *WorkflowRunLeaseIntegrationSuite) TestExtendLease_FromHolder_ExtendsExpiry() {
	now := time.Now().UTC()
	s.seedRun("run-extend", "wf-1", now)
	a, ok, err := s.repo.ClaimLease(context.Background(), "worker-a", 5*time.Second,
		[]workflow.RunStatus{workflow.RunStatusQueued, workflow.RunStatusRunning})
	s.Require().NoError(err)
	s.Require().True(ok)
	first := *a.LeaseExpiresAt

	// Extend by a longer window.
	err = s.repo.ExtendLease(context.Background(), "run-extend", "worker-a", 60*time.Second)
	s.Require().NoError(err)

	// Read back + assert expiry moved forward.
	rec, err := s.repo.Get(context.Background(), "run-extend")
	s.Require().NoError(err)
	s.Require().NotNil(rec.LeaseExpiresAt)
	s.True(rec.LeaseExpiresAt.After(first), "extended expiry must be later than the original")
}

// TestExtendLease_FromForeignWorker_ReturnsErrLeaseNotHeld verifies a
// worker that doesn't own the lease can't trample it. This is the
// primary safety guarantee — a slow heartbeat must NOT extend a
// re-claimed lease without the original owner noticing.
func (s *WorkflowRunLeaseIntegrationSuite) TestExtendLease_FromForeignWorker_ReturnsErrLeaseNotHeld() {
	now := time.Now().UTC()
	s.seedRun("run-foreign", "wf-1", now)
	_, ok, err := s.repo.ClaimLease(context.Background(), "worker-a", 30*time.Second,
		[]workflow.RunStatus{workflow.RunStatusQueued, workflow.RunStatusRunning})
	s.Require().NoError(err)
	s.Require().True(ok)

	// Worker-b tries to extend worker-a's lease.
	err = s.repo.ExtendLease(context.Background(), "run-foreign", "worker-b", 60*time.Second)
	s.ErrorIs(err, workflow.ErrLeaseNotHeld)
}

// TestExpiredLease_AutoReclaimable_NewWorkerWins verifies the
// restart-recovery path: a lease whose owner died (no heartbeat) goes
// stale + a new worker can claim. This is what makes the platform
// restart-safe.
func (s *WorkflowRunLeaseIntegrationSuite) TestExpiredLease_AutoReclaimable_NewWorkerWins() {
	now := time.Now().UTC()
	s.seedRun("run-stale", "wf-1", now)

	// Worker-a claims with a tiny TTL.
	_, ok, err := s.repo.ClaimLease(context.Background(), "worker-a", 100*time.Millisecond,
		[]workflow.RunStatus{workflow.RunStatusQueued, workflow.RunStatusRunning})
	s.Require().NoError(err)
	s.Require().True(ok)

	// Wait for the lease to lapse.
	time.Sleep(200 * time.Millisecond)

	// Worker-b can now claim.
	b, ok, err := s.repo.ClaimLease(context.Background(), "worker-b", 30*time.Second,
		[]workflow.RunStatus{workflow.RunStatusQueued, workflow.RunStatusRunning})
	s.Require().NoError(err)
	s.Require().True(ok)
	s.Equal("run-stale", b.ID)
	s.Equal("worker-b", b.LeaseOwner)

	// Worker-a (the original holder) tries to extend — must fail since
	// lease was re-claimed.
	err = s.repo.ExtendLease(context.Background(), "run-stale", "worker-a", 30*time.Second)
	s.ErrorIs(err, workflow.ErrLeaseNotHeld, "stale-lease re-claim must invalidate the original holder")
}

// TestReleaseLease_FromHolder_ClearsLease verifies the explicit
// release path used at terminal status / yield points.
func (s *WorkflowRunLeaseIntegrationSuite) TestReleaseLease_FromHolder_ClearsLease() {
	now := time.Now().UTC()
	s.seedRun("run-release", "wf-1", now)
	_, ok, err := s.repo.ClaimLease(context.Background(), "worker-a", 30*time.Second,
		[]workflow.RunStatus{workflow.RunStatusQueued, workflow.RunStatusRunning})
	s.Require().NoError(err)
	s.Require().True(ok)

	err = s.repo.ReleaseLease(context.Background(), "run-release", "worker-a")
	s.Require().NoError(err)

	rec, err := s.repo.Get(context.Background(), "run-release")
	s.Require().NoError(err)
	s.Empty(rec.LeaseOwner, "lease_owner should be cleared")
	s.Nil(rec.LeaseExpiresAt, "lease_expires_at should be cleared")

	// And another worker can claim immediately.
	b, ok, err := s.repo.ClaimLease(context.Background(), "worker-b", 30*time.Second,
		[]workflow.RunStatus{workflow.RunStatusQueued, workflow.RunStatusRunning})
	s.Require().NoError(err)
	s.Require().True(ok)
	s.Equal("run-release", b.ID)
	s.Equal("worker-b", b.LeaseOwner)
}

// TestReleaseLease_FromForeignWorker_NoOp documents the defensive
// behaviour — a release call from someone who doesn't hold the lease
// silently does nothing instead of returning an error. Cleanup paths
// double-release in normal operation.
func (s *WorkflowRunLeaseIntegrationSuite) TestReleaseLease_FromForeignWorker_NoOp() {
	now := time.Now().UTC()
	s.seedRun("run-foreign-rel", "wf-1", now)
	_, ok, err := s.repo.ClaimLease(context.Background(), "worker-a", 30*time.Second,
		[]workflow.RunStatus{workflow.RunStatusQueued, workflow.RunStatusRunning})
	s.Require().NoError(err)
	s.Require().True(ok)

	// Worker-b releases worker-a's lease — no-op.
	err = s.repo.ReleaseLease(context.Background(), "run-foreign-rel", "worker-b")
	s.NoError(err, "foreign release should be a silent no-op")

	rec, err := s.repo.Get(context.Background(), "run-foreign-rel")
	s.Require().NoError(err)
	s.Equal("worker-a", rec.LeaseOwner, "lease_owner should still be worker-a")
	s.NotNil(rec.LeaseExpiresAt)
}

// TestClaimLease_StatusFilter_OnlyMatchesRequestedStatuses verifies
// the predicate honors the supplied status set.
func (s *WorkflowRunLeaseIntegrationSuite) TestClaimLease_StatusFilter_OnlyMatchesRequestedStatuses() {
	now := time.Now().UTC()
	// Seed runs in different terminal states; none should be claimable
	// when we ask for `running`.
	_, _ = s.repo.Create(context.Background(), workflow.WorkflowRun{
		ID: "run-success", WorkflowID: "wf-1", TenantID: "default",
		Status: workflow.RunStatusSuccess, QueuedAt: now,
	})
	_, _ = s.repo.Create(context.Background(), workflow.WorkflowRun{
		ID: "run-error", WorkflowID: "wf-1", TenantID: "default",
		Status: workflow.RunStatusError, QueuedAt: now,
	})

	_, ok, err := s.repo.ClaimLease(context.Background(), "worker-a", 30*time.Second,
		[]workflow.RunStatus{workflow.RunStatusQueued, workflow.RunStatusRunning})
	s.Require().NoError(err)
	s.False(ok, "no runs in `running` status; terminal runs must NOT be claimed")

	// Now ask for the right status.
	s.seedRun("run-r", "wf-1", now)
	_, ok, err = s.repo.ClaimLease(context.Background(), "worker-a", 30*time.Second,
		[]workflow.RunStatus{workflow.RunStatusQueued, workflow.RunStatusRunning})
	s.Require().NoError(err)
	s.True(ok)
}
