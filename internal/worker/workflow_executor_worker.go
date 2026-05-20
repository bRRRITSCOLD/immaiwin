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
	"encoding/json"
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

	// Periodic orphan sweep — every non-terminal run with an
	// expired lease and no checkpoint state gets flipped to error.
	// Catches debug-paused runs whose worker died mid-pause (the
	// pre-exec breakpoint wait doesn't checkpoint execution_state,
	// so a killed worker leaves the run record stuck at status=
	// running indefinitely until the reaper TTL — by default 1h —
	// catches up).
	//
	// Runs on a ticker rather than once at boot because the lease
	// only lapses ~30s AFTER the worker dies; if the new worker
	// boots immediately the lease is still live + the sweep
	// would miss the orphan. Periodic check catches it on the
	// next tick after lease expiry.
	//
	// Publishes synthetic run_done events per swept run so live
	// canvas WS subscribers flip terminal immediately.
	//
	// Safety: the filter skips live-lease runs + runs with
	// execution_state set, so a parallel worker's in-flight run
	// can never be stomped.
	go runOrphanSweepLoop(ctx, runRepo, rc)

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

	// Heartbeat lease while we execute. runCtx is what the run-pass
	// uses; the heartbeat CANCELS it the moment the lease is lost
	// (force-cancel drops lease_owner, or a partition). Without this
	// cancellation a long internal loop (for_each / agent ReAct)
	// keeps running until the NEXT BFS-level checkpoint — a cancel
	// mid-for_each couldn't stop the loop. ctx-cancel makes every
	// ctx-aware op (http dial, loop iteration guards) abort promptly.
	runCtx, runCancel := context.WithCancel(ctx)
	hbDone := make(chan struct{})
	go heartbeatLease(runCtx, runRepo, rec.ID, workerID, runCancel, hbDone)

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

	// Canvas Continue + set_breakpoints control bridge. Subscribe to
	// `burrow:run_control:<runID>` and route inbound messages into
	// in-process chans the executor's runEnv listens on. Without this
	// the engine has continueCh/SetBreakpoints primitives but the
	// API-side REST endpoints have no path to reach the worker that
	// holds the run. Bridge survives the run-pass; chans close on
	// runCtx cancel (heartbeat + outer cancel both feed runCtx).
	bridgeCtx := runCtx
	if exec.ApprovalBroker != nil {
		controlCh := make(chan struct{}, 4)
		bpCh := make(chan []string, 4)
		startRunControlBridge(runCtx, exec.ApprovalBroker, rec.ID, controlCh, bpCh)
		bridgeCtx = workflow.WithControlChannels(runCtx, &workflow.ControlChannels{
			Continue:    controlCh,
			Breakpoints: bpCh,
		})
	}

	_, err = exec.RunFromCheckpoint(bridgeCtx, wf, rec, workerID, emitter)

	runCancel() // stop the heartbeat once the run-pass returns
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
func heartbeatLease(ctx context.Context, runRepo *mongodb.WorkflowRunRepository, runID, workerID string, onLeaseLost context.CancelFunc, done chan<- struct{}) {
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
					slog.Warn("workflow-executor: lease lost during heartbeat — cancelling run ctx", "run_id", runID, "worker", workerID)
					// Cancel the run-pass ctx so a long internal loop
					// (for_each / agent) aborts NOW instead of running
					// to the next BFS checkpoint. The BFS still surfaces
					// ErrLeaseNotHeld so the run-pass is abandoned and
					// the cancelled status isn't overwritten.
					onLeaseLost()
					return
				}
				slog.Warn("workflow-executor: heartbeat extend failed", "run_id", runID, "err", err)
			}
		}
	}
}

// orphanSweepTick is how often the executor worker scans for
// abandoned non-terminal runs. Quick tick + a longer safety window
// inside the predicate (see orphanSweepStaleAfter) so the canvas
// flip is snappy without false-positiving on fresh runs whose
// worker is just busy on another claim.
var orphanSweepTick = 10 * time.Second

// orphanSweepStaleAfter is the safety window applied to queued_at
// in the sweep filter. Must comfortably exceed executorLeaseDur
// (30s) — only AFTER a lease has had time to lapse does an
// unrecovered run count as orphaned. 60s = 30s lease + 30s grace.
var orphanSweepStaleAfter = 60 * time.Second

// runOrphanSweepLoop scans for runs the worker was driving but
// lost — process crash mid-run, debug-paused run whose worker
// died at the breakpoint wait. Flips them to error + publishes a
// synthetic run_done so any live canvas WS subscriber clears
// stale Continue/Cancel affordances. Best-effort: failures log
// at warn but don't take down the worker.
func runOrphanSweepLoop(ctx context.Context, runRepo *mongodb.WorkflowRunRepository, rc *rediss.Client) {
	t := time.NewTicker(orphanSweepTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweepCtx, sweepCancel := context.WithTimeout(ctx, 10*time.Second)
			swept, err := runRepo.SweepWorkerOrphansCollect(sweepCtx,
				"abandoned: worker died mid-run with no checkpoint state (debug-paused breakpoint or in-process tier)",
				orphanSweepStaleAfter)
			sweepCancel()
			if err != nil {
				slog.Warn("workflow-executor: orphan sweep failed", "err", err)
				continue
			}
			if len(swept) == 0 {
				continue
			}
			slog.Warn("workflow-executor: orphan sweep flipped abandoned runs to error", "count", len(swept))
			for _, runID := range swept {
				payload, _ := json.Marshal(workflow.RunEvent{
					Type:   workflow.EventRunDone,
					RunID:  runID,
					Status: string(workflow.RunStatusError),
					Error:  "abandoned: worker died mid-run with no checkpoint state",
				})
				if _, perr := rc.PublishWithCount(ctx, workflow.RunEventChannel(runID), payload); perr != nil {
					slog.Warn("workflow-executor: run_done publish failed", "run_id", runID, "err", perr)
				}
			}
		}
	}
}

// startRunControlBridge subscribes to the per-run control channel on
// Redis and routes decoded RunControlMessages into the worker-owned
// in-process chans the executor's runEnv listens on. Goroutine exits
// on ctx cancel (lease lost / run-pass returned) and unsubscribes so
// the next claim of the same run gets a fresh bridge.
//
// Buffered chans (4 slots) keep a fast double-click from blocking
// the subscription pump; the executor's continueCh drain loop in
// waitAtBreakpoint handles legit "click Continue twice" UX.
func startRunControlBridge(ctx context.Context, sub workflow.ApprovalSubscriber, runID string, continueCh chan struct{}, bpCh chan []string) {
	pubsub := sub.Subscribe(ctx, workflow.RunControlChannel(runID))
	go func() {
		defer func() {
			_ = pubsub.Close()
		}()
		ch := pubsub.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				var rcm workflow.RunControlMessage
				if err := json.Unmarshal([]byte(msg.Payload), &rcm); err != nil {
					slog.Warn("workflow-executor: bad run_control payload", "run_id", runID, "err", err)
					continue
				}
				switch rcm.Type {
				case "continue":
					select {
					case continueCh <- struct{}{}:
					default:
						// Buffer full — a click already pending; drop.
					}
				case "set_breakpoints":
					select {
					case bpCh <- rcm.NodeIDs:
					case <-ctx.Done():
						return
					}
				default:
					slog.Debug("workflow-executor: unknown run_control type", "run_id", runID, "type", rcm.Type)
				}
			}
		}
	}()
}
