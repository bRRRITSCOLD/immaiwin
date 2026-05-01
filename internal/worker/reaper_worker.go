package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/config"
	"github.com/bRRRITSCOLD/burrow/internal/mongodb"
	"github.com/bRRRITSCOLD/burrow/internal/rediss"
	"github.com/bRRRITSCOLD/burrow/internal/workflow"
)

// ReaperWorker sweeps workflow runs stuck in non-terminal states past
// a configured per-status TTL and force-cancels them.
//
// Why per-status TTLs:
//   - `running`         — covers crashed workers + runaway agent loops
//   - `paused`          — covers UI breakpoints abandoned by the operator
//   - `pending_approval` — covers OOB approval gates waiting on a human
//                         (Slack/email round-trip; usually wakes within
//                         minutes, but legit waits of hours happen)
//
// A single global timeout would either cancel humans-in-the-loop too
// aggressively or leave dead workers' runs visibly "running" for days.
// Each TTL can be set to 0 to disable that status's sweep.
//
// Coordination: reaper runs in the worker pid; the api pid hosts the
// manual /workflow_runs/:id/cancel handler. They share semantics —
// flip status=cancelled, publish "rejected" on the OOB approval
// channel, write timestamps. A live worker actually executing the run
// will eventually fail to write its terminal status (race), but the
// reaper's win is intentional: we trust the stuck-detection.
//
// Cross-pid safety: two reaper instances would just race on Update
// — last writer wins. Idempotent. No leader election needed.

var ReaperWorker = &reaperWorker{}

type reaperWorker struct{}

func (w *reaperWorker) Name() string { return "reaper" }

// reaperTask captures one configured sweep — a status + its max age
// before cancellation. Centralised so adding a new "stuck" status (e.g.
// future `pending_input`) is one entry, not three switch statements.
type reaperTask struct {
	status workflow.RunStatus
	ttl    time.Duration
}

func (w *reaperWorker) Run(ctx context.Context) error {
	cfg, err := config.Load(config.WithDotEnv(".env"))
	if err != nil {
		return fmt.Errorf("reaper: load config: %w", err)
	}

	tickInterval := parseDurationOrDefault(cfg.Worker.Reaper.TickInterval, 60*time.Second)

	tasks := []reaperTask{}
	if d := parseDurationOrDefault(cfg.Worker.Reaper.RunningTTL, 0); d > 0 {
		tasks = append(tasks, reaperTask{status: workflow.RunStatusRunning, ttl: d})
	}
	if d := parseDurationOrDefault(cfg.Worker.Reaper.PausedTTL, 0); d > 0 {
		tasks = append(tasks, reaperTask{status: workflow.RunStatusPaused, ttl: d})
	}
	if d := parseDurationOrDefault(cfg.Worker.Reaper.PendingApprovalTTL, 0); d > 0 {
		tasks = append(tasks, reaperTask{status: workflow.RunStatusPendingApproval, ttl: d})
	}
	if len(tasks) == 0 {
		slog.Warn("reaper: all TTLs are 0 — nothing to sweep, worker idle")
	}

	mc, err := mongodb.New(ctx, cfg.MongoDB)
	if err != nil {
		return fmt.Errorf("reaper: connect mongodb: %w", err)
	}
	defer func() {
		if err := mc.Disconnect(ctx); err != nil {
			slog.Error("reaper: disconnect mongodb", "err", err)
		}
	}()

	runStore, err := mongodb.NewWorkflowRunRepository(ctx, mc.DB())
	if err != nil {
		return fmt.Errorf("reaper: create run repo: %w", err)
	}

	// Approval broker is best-effort — only used to unblock pending_
	// approval runs that still have a live agent waiting (the cancel
	// handler does the same). When no live worker is listening the
	// publish drops on the floor and the run still gets force-marked
	// cancelled below. *rediss.Client satisfies workflow.ApprovalSubscriber.
	rc := rediss.New(cfg.Redis)
	defer func() {
		if err := rc.Close(); err != nil {
			slog.Error("reaper: close redis", "err", err)
		}
	}()
	var broker workflow.ApprovalSubscriber = rc

	slog.Info("reaper: starting",
		"tick", tickInterval,
		"tasks", taskSummary(tasks),
	)

	w.sweep(ctx, runStore, broker, tasks)

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			w.sweep(ctx, runStore, broker, tasks)
		}
	}
}

// sweep runs one pass: for each task, query stuck runs and force-cancel.
// Logs counts at info-level so the operator can see "0 stuck" as a
// healthy heartbeat without DEBUG noise.
func (w *reaperWorker) sweep(
	ctx context.Context,
	runStore *mongodb.WorkflowRunRepository,
	broker workflow.ApprovalSubscriber,
	tasks []reaperTask,
) {
	now := time.Now().UTC()
	totalReaped := 0

	for _, t := range tasks {
		cutoff := now.Add(-t.ttl)
		stuck, err := runStore.ListWithFilter(ctx, workflow.RunFilter{
			Status:        t.status,
			StartedBefore: cutoff,
			Limit:         200, // page size; next tick gets the rest if any
		})
		if err != nil {
			slog.Error("reaper: list stuck runs", "status", t.status, "err", err)
			continue
		}
		if len(stuck) == 0 {
			continue
		}

		reaped := 0
		for _, run := range stuck {
			if err := w.reap(ctx, runStore, broker, run, t.ttl); err != nil {
				slog.Error("reaper: failed to reap run",
					"run_id", run.ID,
					"workflow_id", run.WorkflowID,
					"status", run.Status,
					"err", err,
				)
				continue
			}
			reaped++
		}
		totalReaped += reaped
		slog.Info("reaper: swept",
			"status", t.status,
			"ttl", t.ttl,
			"found", len(stuck),
			"reaped", reaped,
			"cutoff", cutoff.Format(time.RFC3339),
		)
	}

	if totalReaped > 0 {
		slog.Info("reaper: sweep complete", "total_reaped", totalReaped)
	}
}

// reap force-cancels one stuck run. Mirrors the manual cancel handler
// in api/handler/run_cancel.go: publish "rejected" decision (best-
// effort unblock for live agents), then write status=cancelled with a
// human-readable error message that surfaces in the runs UI.
func (w *reaperWorker) reap(
	ctx context.Context,
	runStore *mongodb.WorkflowRunRepository,
	broker workflow.ApprovalSubscriber,
	run workflow.WorkflowRun,
	ttl time.Duration,
) error {
	// Pending-approval needs the broker poke; without it any live
	// agent waiting on the gate keeps blocking until the next request
	// ctx times out (could be hours).
	if run.Status == workflow.RunStatusPendingApproval && broker != nil {
		reg := workflow.NewApprovalRegistry(broker)
		// Reaper always writes the terminal `error` state below; the
		// publish is a best-effort nudge for any live waiter, count
		// is irrelevant here.
		_, _ = reg.Submit(ctx, run.ID, workflow.ApprovalDecision{
			Approved: false,
			Reason:   fmt.Sprintf("reaper: pending_approval TTL %s exceeded", ttl),
		})
	}

	now := time.Now().UTC()
	run.Status = workflow.RunStatusCancelled
	run.FinishedAt = &now
	run.PendingApproval = nil
	if run.Error == "" {
		run.Error = fmt.Sprintf("reaper: %s TTL %s exceeded", string(run.Status), ttl)
	}
	return runStore.Update(ctx, run)
}

func parseDurationOrDefault(raw string, fallback time.Duration) time.Duration {
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		slog.Warn("reaper: invalid duration, using default", "raw", raw, "default", fallback)
		return fallback
	}
	return d
}

func taskSummary(tasks []reaperTask) string {
	if len(tasks) == 0 {
		return "none"
	}
	out := ""
	for i, t := range tasks {
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("%s=%s", t.status, t.ttl)
	}
	return out
}
