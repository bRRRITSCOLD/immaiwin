// Workflow executor worker — Phase 3 PR 3.2 of the durable-execution
// rollout. Replaces the in-process `RunResumable` goroutine model
// for runs flowing through Mongo: this worker claims a run's lease,
// executes it via WorkflowExecutor.RunFromCheckpoint (which
// checkpoints BFS state per node visit + yields on approval gates),
// then releases the lease. Designed to run multi-pod: any number of
// these workers can compete for the same lease pool, and the Mongo
// findAndModify in ClaimLease guarantees single-winner semantics.
//
// Wakeup signalling: the worker subscribes to the Redis pub/sub
// `burrow:wakeup` channel. The /run dispatcher (PR 3.3) and the
// approval handler (PR 3.2) publish wakeups on this channel so a
// worker that's idle between ticks claims promptly. Without the
// wakeup the periodic claim tick still picks the run up — wakeup is
// a latency optimisation, not a correctness requirement.
//
// Concurrency: cfg.Worker.Concurrency goroutines each run the
// claim → execute → release loop independently. Each ClaimLease is
// atomic so two goroutines never claim the same run.

package worker

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/config"
	"github.com/bRRRITSCOLD/burrow/internal/mongodb"
	"github.com/bRRRITSCOLD/burrow/internal/rediss"
	"github.com/bRRRITSCOLD/burrow/internal/workflow"
	"github.com/oklog/ulid/v2"
)

// Tunables — kept conservative; expose via env later if ops want to tune.
var (
	executorLeaseDur     = 30 * time.Second // matches DURABLE-EXECUTION-PLAN default
	executorHeartbeatDur = 10 * time.Second // 1/3 of lease so a stuck heartbeat costs <1 lease
	executorClaimTick    = 5 * time.Second  // backstop poll between wakeups
)

var WorkflowExecutorWorker = &workflowExecutorWorker{}

type workflowExecutorWorker struct{}

func (w *workflowExecutorWorker) Name() string { return "workflow-executor" }

func (w *workflowExecutorWorker) Run(ctx context.Context) error {
	cfg, err := config.Load(config.WithDotEnv(".env"))
	if err != nil {
		return fmt.Errorf("workflow-executor: load config: %w", err)
	}

	rc := rediss.New(cfg.Redis)
	defer func() {
		if err := rc.Close(); err != nil {
			slog.Error("workflow-executor: close redis", "err", err)
		}
	}()

	mc, err := mongodb.New(ctx, cfg.MongoDB)
	if err != nil {
		return fmt.Errorf("workflow-executor: connect mongodb: %w", err)
	}
	defer func() {
		if err := mc.Disconnect(ctx); err != nil {
			slog.Error("workflow-executor: disconnect mongodb", "err", err)
		}
	}()

	wfRepo, err := mongodb.NewWorkflowRepository(ctx, mc.DB())
	if err != nil {
		return fmt.Errorf("workflow-executor: create workflow repo: %w", err)
	}
	runRepo, err := mongodb.NewWorkflowRunRepository(ctx, mc.DB())
	if err != nil {
		return fmt.Errorf("workflow-executor: create run repo: %w", err)
	}

	var encKey []byte
	if trimmed := strings.TrimSpace(cfg.EncryptionKey); trimmed != "" {
		encKey, err = hex.DecodeString(trimmed)
		if err != nil || len(encKey) != 32 {
			return fmt.Errorf("workflow-executor: ENCRYPTION_KEY must be 64 hex chars (32 bytes): %v", err)
		}
	}
	connRepo, err := mongodb.NewConnectionRepository(ctx, mc.DB(), encKey)
	if err != nil {
		return fmt.Errorf("workflow-executor: create connection repo: %w", err)
	}
	defaultDB := mongodb.NewMongoClient(mc.DB())
	connResolver := workflow.NewConnectionResolver(connRepo, defaultDB, rc)
	defer func() {
		if err := connResolver.Close(); err != nil {
			slog.Error("workflow-executor: close connection resolver", "err", err)
		}
	}()

	exec, sandboxClose := BuildWorkerExecutor(ctx, cfg, mc.DB(), rc, connResolver)
	defer sandboxClose()

	concurrency := cfg.Worker.Concurrency
	if concurrency <= 0 {
		concurrency = 1
	}

	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "worker"
	}

	// Wakeup fan-out: one Redis subscription on `burrow:wakeup`,
	// fans into a buffered channel each loop reads. Buffer >= concurrency
	// so a burst wakeup unblocks every loop without dropping signals.
	wake := make(chan struct{}, concurrency*2)
	go subscribeWakeups(ctx, rc, wake)

	slog.Info("workflow-executor: starting",
		"concurrency", concurrency,
		"lease", executorLeaseDur,
		"tick", executorClaimTick,
	)

	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		workerID := fmt.Sprintf("%s/executor-%d-%s", hostname, i, ulid.Make().String())
		wg.Add(1)
		go func(workerID string) {
			defer wg.Done()
			runClaimLoop(ctx, workerID, exec, runRepo, wfRepo, wake)
		}(workerID)
	}
	wg.Wait()
	return nil
}

// subscribeWakeups pumps Redis pub/sub messages on WakeupChannel into
// the local wake channel. One nudge per published message — additional
// messages while the buffer is full are dropped (next claim tick still
// covers them). Exits when ctx is done; logs at warn on subscription
// errors but doesn't crash the worker (claim ticks keep working).
func subscribeWakeups(ctx context.Context, rc *rediss.Client, wake chan<- struct{}) {
	if rc == nil {
		return
	}
	sub := rc.Subscribe(ctx, workflow.WakeupChannel)
	defer func() {
		if err := sub.Close(); err != nil {
			slog.Warn("workflow-executor: wakeup unsubscribe failed", "err", err)
		}
	}()
	ch := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ch:
			if !ok {
				return
			}
			select {
			case wake <- struct{}{}:
			default:
				// buffer full: a claim attempt is already pending
			}
		}
	}
}

// runClaimLoop is the per-worker main loop: tries to claim a run on
// every wakeup or tick, executes it under a heartbeating lease,
// releases on the way out. One ErrLeaseNotHeld doesn't kill the loop
// — we just move on to the next claim.
func runClaimLoop(
	ctx context.Context,
	workerID string,
	exec *workflow.WorkflowExecutor,
	runRepo *mongodb.WorkflowRunRepository,
	wfRepo *mongodb.WorkflowRepository,
	wake <-chan struct{},
) {
	tick := time.NewTicker(executorClaimTick)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-tick.C:
		}
		// Drain extra wakeups so we don't trigger N back-to-back
		// claim attempts for one logical event.
		drain := true
		for drain {
			select {
			case <-wake:
			default:
				drain = false
			}
		}
		claimAndRun(ctx, workerID, exec, runRepo, wfRepo)
	}
}

// claimAndRun performs one claim → execute → release pass. Returns
// silently when no run is claimable. Each call is one logical lease
// acquisition; the caller's loop reattempts on the next wake or tick.
func claimAndRun(
	ctx context.Context,
	workerID string,
	exec *workflow.WorkflowExecutor,
	runRepo *mongodb.WorkflowRunRepository,
	wfRepo *mongodb.WorkflowRepository,
) {
	// Claim filter is `running` only. A run in `pending_approval` is
	// waiting on a human; the approval handler flips it back to
	// `running` (via ApplyApprovalDecision) the moment a decision
	// lands, at which point the worker picks it up. Claiming the
	// pending-approval status here would just busy-loop the gate.
	rec, claimed, err := runRepo.ClaimLease(ctx, workerID, executorLeaseDur, []workflow.RunStatus{
		workflow.RunStatusQueued,  // dispatched-but-unclaimed; ClaimLease flips → running atomically
		workflow.RunStatusRunning, // resumed / recovered runs that already have a status of running
	})
	if err != nil {
		slog.Warn("workflow-executor: claim lease failed", "worker", workerID, "err", err)
		return
	}
	if !claimed {
		return
	}
	slog.Info("workflow-executor: claimed", "worker", workerID, "run_id", rec.ID, "wf_id", rec.WorkflowID, "status", rec.Status)

	wf, gerr := wfRepo.GetByID(ctx, rec.WorkflowID)
	if gerr != nil {
		// Workflow was deleted out from under the run. Best to
		// terminate the run as error so it doesn't get re-claimed
		// forever. Foreign release is a no-op so this is safe even
		// if our lease has expired.
		now := time.Now().UTC()
		rec.Status = workflow.RunStatusError
		rec.Error = "workflow-executor: workflow not found: " + gerr.Error()
		rec.FinishedAt = &now
		_ = runRepo.Update(ctx, rec)
		_ = runRepo.ReleaseLease(ctx, rec.ID, workerID)
		slog.Warn("workflow-executor: workflow load failed; run errored", "run_id", rec.ID, "wf_id", rec.WorkflowID, "err", gerr)
		return
	}

	// Heartbeat lease while we execute. ctx-scoped cancel so the
	// goroutine exits cleanly when execute returns. Dropping a
	// heartbeat round-trip out of every loop iteration would be
	// cleaner long-term; for now a goroutine is the simplest path.
	hbCtx, hbCancel := context.WithCancel(ctx)
	hbDone := make(chan struct{})
	go heartbeatLease(hbCtx, runRepo, rec.ID, workerID, hbDone)

	// Per-run event emitter publishes RunEvents to a Redis pub/sub
	// topic so the WS handler can stream them to the browser. Without
	// this, canvas runs would have no live updates because the worker
	// runs out-of-process.
	emitter := exec.Events
	if exec.ApprovalBroker != nil {
		emitter = &workflow.RedisRunEventEmitter{
			Pub:   exec.ApprovalBroker, // shared rediss.Client; satisfies RunEventPublisher
			RunID: rec.ID,
		}
	}
	_, err = exec.RunFromCheckpoint(ctx, wf, rec, workerID, emitter)

	hbCancel()
	<-hbDone

	switch {
	case errors.Is(err, workflow.ErrLeaseNotHeld):
		slog.Warn("workflow-executor: lease lost mid-run; another worker will reclaim", "run_id", rec.ID, "worker", workerID)
		// don't release — we don't own it any more
		return
	case err != nil:
		slog.Warn("workflow-executor: run-pass returned error", "run_id", rec.ID, "worker", workerID, "err", err)
	}
	if rerr := runRepo.ReleaseLease(ctx, rec.ID, workerID); rerr != nil {
		slog.Warn("workflow-executor: release lease failed", "run_id", rec.ID, "worker", workerID, "err", rerr)
	}
}

// heartbeatLease extends the lease every executorHeartbeatDur until
// hbCtx is cancelled. Closes done before exit so the caller can wait
// without racing on the still-in-flight extend. Treats ErrLeaseNotHeld
// as fatal — we lost the lease (network partition, slow heartbeat),
// nothing further to do; the caller will see the same error from its
// own RunFromCheckpoint and abort.
func heartbeatLease(ctx context.Context, runRepo *mongodb.WorkflowRunRepository, runID, workerID string, done chan<- struct{}) {
	defer close(done)
	t := time.NewTicker(executorHeartbeatDur)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := runRepo.ExtendLease(ctx, runID, workerID, executorLeaseDur); err != nil {
				if errors.Is(err, workflow.ErrLeaseNotHeld) {
					slog.Warn("workflow-executor: lease lost during heartbeat", "run_id", runID, "worker", workerID)
					return
				}
				slog.Warn("workflow-executor: heartbeat extend failed", "run_id", runID, "err", err)
			}
		}
	}
}
