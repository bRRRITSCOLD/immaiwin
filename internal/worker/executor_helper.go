// Shared executor wiring for worker processes. Mirrors the API's
// WorkflowExecutor construction so cron-, webhook-, and event-driven
// runs all share the same dep set: chat memory, run persistence,
// skill resolver, sandbox runtime, OOB approval broker. Without this
// helper each worker reproduced a partial wiring + drifted from the
// API + canvas behaviour — the cron-worker bug that took multiple
// iterations to find.
//
// All workers in this package MUST construct their executor via
// BuildWorkerExecutor. New worker? Use this helper. New executor
// dependency? Add it here once.

package worker

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/config"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/mongodb"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/rediss"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/workflow"
	mongoDriver "go.mongodb.org/mongo-driver/v2/mongo"
)

// BuildWorkerExecutor wires a WorkflowExecutor with the full dep set
// every worker needs. Returns the executor + a cleanup function that
// closes the sandbox runtime + ConnectionResolver. Callers should
// `defer cleanup()` so process shutdown releases held resources.
//
// Failures on optional deps (chat memory, run repo, skill resolver,
// sandbox runtime) degrade the feature locally but don't fail the
// builder — workers should keep running with whatever subset is
// available, matching the API's best-effort init pattern.
func BuildWorkerExecutor(
	ctx context.Context,
	cfg *config.Config,
	db *mongoDriver.Database,
	rc *rediss.Client,
	connResolver *workflow.ConnectionResolver,
) (*workflow.WorkflowExecutor, func()) {
	defaultDB := mongodb.NewMongoClient(db)

	chatMem, err := mongodb.NewChatMemory(ctx, db)
	if err != nil {
		slog.Warn("worker: chat memory init failed (agents will run without history)", "err", err)
		chatMem = nil
	}
	var runStore workflow.WorkflowRunStore
	if runRepo, rerr := mongodb.NewWorkflowRunRepository(ctx, db); rerr == nil {
		runStore = runRepo
	} else {
		slog.Warn("worker: run repo init failed (runs will not persist)", "err", rerr)
	}
	skillRes := buildSkillResolver(ctx, cfg, db)
	sandboxRT := buildSandboxRT(ctx, cfg)

	exec := &workflow.WorkflowExecutor{
		HTTPClient:     &http.Client{Timeout: 30 * time.Second},
		DB:             defaultDB,
		Redis:          rc,
		ConnResolver:   connResolver,
		SandboxRT:      sandboxRT,
		ApprovalBroker: rc,
		Memory:         chatMem,
		RunRepo:        runStore,
		SkillRes:       skillRes,
	}

	cleanup := func() {
		if sandboxRT != nil {
			_ = sandboxRT.Close()
		}
	}
	return exec, cleanup
}
