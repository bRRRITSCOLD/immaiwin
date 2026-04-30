//go:build integration

// Heartbeat ticker integration test. Spins up a real Mongo, registers
// a fake worker that blocks until ctx cancel, runs the registry with
// a 100ms heartbeat interval, asserts tick_count advances + status
// flips on clean shutdown.
//
// Real Mongo (per .claude/rules/TESTING.md — no mocks). Uses a uniquely-
// named test database so concurrent test runs don't collide; dropped
// in TearDownSuite. Compiled only under `-tags=integration`.

package worker

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/mongodb"
	"github.com/stretchr/testify/suite"
	"go.mongodb.org/mongo-driver/v2/mongo"
	driveroptions "go.mongodb.org/mongo-driver/v2/mongo/options"
)

type HeartbeatIntegrationSuite struct {
	suite.Suite
	client *mongo.Client
	db     *mongo.Database
	dbName string
	health *mongodb.WorkerHealthRepository
}

func TestHeartbeatIntegrationSuite(t *testing.T) {
	uri := os.Getenv("MONGO_URI")
	if uri == "" {
		uri = "mongodb://localhost:27017"
	}
	// Reachability probe — fail loud (no skip) so a missing service
	// is never silently green. Compose stack must be up for the
	// `-tags=integration` test build to pass.
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer probeCancel()
	c, err := mongo.Connect(driveroptions.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect failed (compose stack required): %v", err)
	}
	if err := c.Ping(probeCtx, nil); err != nil {
		_ = c.Disconnect(context.Background())
		t.Fatalf("mongo unreachable at %s (compose stack required): %v", uri, err)
	}
	_ = c.Disconnect(context.Background())
	suite.Run(t, new(HeartbeatIntegrationSuite))
}

func (s *HeartbeatIntegrationSuite) SetupSuite() {
	uri := os.Getenv("MONGO_URI")
	if uri == "" {
		uri = "mongodb://localhost:27017"
	}
	c, err := mongo.Connect(driveroptions.Client().ApplyURI(uri))
	s.Require().NoError(err)
	s.client = c
	s.dbName = fmt.Sprintf("burrow_test_heartbeat_%d", time.Now().UnixNano())
	s.db = c.Database(s.dbName)
	repo, err := mongodb.NewWorkerHealthRepository(context.Background(), s.db)
	s.Require().NoError(err)
	s.health = repo
}

func (s *HeartbeatIntegrationSuite) TearDownSuite() {
	if s.db != nil {
		_ = s.db.Drop(context.Background())
	}
	if s.client != nil {
		_ = s.client.Disconnect(context.Background())
	}
}

func (s *HeartbeatIntegrationSuite) SetupTest() {
	// Wipe worker_health between tests so tick_count starts at 0 on
	// each run.
	_, _ = s.db.Collection("worker_health").DeleteMany(context.Background(), map[string]any{})
}

func (s *HeartbeatIntegrationSuite) TearDownTest() {}

// fetchHealth pulls a single worker_health doc by name. Repo only
// exposes List + write methods; this test needs a single-doc lookup
// for assertions, so we go straight to the collection rather than
// inflate the repo API for one caller.
func (s *HeartbeatIntegrationSuite) fetchHealth(name string) (mongodb.WorkerHealth, error) {
	var row mongodb.WorkerHealth
	err := s.db.Collection("worker_health").
		FindOne(context.Background(), map[string]any{"_id": name}).
		Decode(&row)
	return row, err
}

// fakeWorker blocks in Run until ctx is cancelled. It owns no internal
// ticker — heartbeats are entirely the registry's responsibility, which
// is exactly what we want to test.
type fakeWorker struct{ name string }

func (f *fakeWorker) Name() string { return f.name }
func (f *fakeWorker) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// TestHeartbeat_TickerOver1s_AdvancesAndCleanlyStops runs the registry for ~1s with a
// 100ms heartbeat interval, then cancels ctx and waits for clean exit.
// Asserts:
//
//   - tick_count is at least 5 (1000ms / 100ms = 10, give margin for
//     scheduler jitter + the start-of-loop where the first tick fires
//     after ~100ms not at t=0)
//   - status is "stopped" (clean exit went through MarkStopped)
//   - started_at is populated and recent
//   - last_heartbeat advanced beyond started_at
func (s *HeartbeatIntegrationSuite) TestHeartbeat_TickerOver1s_AdvancesAndCleanlyStops() {
	wr := NewWorkerRegistry().WithHealth(s.health).WithHeartbeatInterval(100 * time.Millisecond)
	wr.RegisterWorker(&fakeWorker{name: "test-fake"})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- wr.StartWorker(ctx, "test-fake", 1)
	}()

	// Let the heartbeat goroutine run long enough to log multiple
	// ticks. 1s / 100ms = 10 expected ticks; assert >= 5 to absorb
	// scheduling jitter without flaking on slow CI runners.
	time.Sleep(1 * time.Second)
	cancel()

	select {
	case err := <-done:
		s.Require().NoError(err)
	case <-time.After(3 * time.Second):
		s.FailNow("worker StartWorker did not return within 3s of ctx cancel")
	}

	row, err := s.fetchHealth("test-fake")
	s.Require().NoError(err)
	s.GreaterOrEqual(row.TickCount, int64(5),
		"tick_count should advance ≥5 times in 1s @ 100ms cadence (got %d)", row.TickCount)
	s.Equal("stopped", row.Status, "status should be 'stopped' after clean exit")
	s.False(row.StartedAt.IsZero(), "started_at should be populated")
	s.True(row.LastHeartbeat.After(row.StartedAt),
		"last_heartbeat (%s) should be after started_at (%s)", row.LastHeartbeat, row.StartedAt)
	s.NotNil(row.StoppedAt, "stopped_at should be populated after clean exit")
}

// TestHeartbeat_NilHealthRepo_RegistryStillUsable asserts the registry
// stays usable with a nil health repo — important because cmd-line
// tooling sometimes constructs the registry without Mongo wired.
func (s *HeartbeatIntegrationSuite) TestHeartbeat_NilHealthRepo_RegistryStillUsable() {
	wr := NewWorkerRegistry().WithHeartbeatInterval(50 * time.Millisecond)
	wr.RegisterWorker(&fakeWorker{name: "test-no-health"})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- wr.StartWorker(ctx, "test-no-health", 1)
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		s.Require().NoError(err)
	case <-time.After(2 * time.Second):
		s.FailNow("worker StartWorker did not return within 2s of ctx cancel")
	}
}
