package worker

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/config"
	"github.com/bRRRITSCOLD/burrow/internal/mongodb"
	"github.com/bRRRITSCOLD/burrow/internal/rediss"
	"github.com/bRRRITSCOLD/burrow/internal/skills"
	"github.com/bRRRITSCOLD/burrow/internal/workflow"
	"github.com/oklog/ulid/v2"
	"github.com/robfig/cron/v3"
	mongoDriver "go.mongodb.org/mongo-driver/v2/mongo"
)

// buildSkillResolver constructs a skill resolver if SKILLS_ENABLED is
// set in config. Mirrors cmd/api init so cron-driven agent runs see the
// same skill catalog as canvas-driven runs. Best-effort — returns nil
// on any failure so a misconfigured skills dir doesn't break workers.
func buildSkillResolver(ctx context.Context, cfg *config.Config, db *mongoDriver.Database) *skills.Resolver {
	if !cfg.Skills.Enabled {
		return nil
	}
	registry, err := mongodb.NewSkillRegistry(ctx, db)
	if err != nil {
		slog.Warn("workflow-cron: skill registry init failed (skills disabled)", "err", err)
		return nil
	}
	fsSrc := skills.NewLocalFSSource(cfg.Skills.Dir, "local-fs")
	return skills.NewResolver(registry, fsSrc)
}

const cronSyncInterval = 5 * time.Second

var WorkflowCronWorker = &workflowCronWorker{}

type workflowCronWorker struct{}

func (w *workflowCronWorker) Name() string { return "workflow-cron" }

type trackedEntry struct {
	entryID   cron.EntryID
	updatedAt time.Time
	cronExpr  string
}

type cronTriggerInfo struct {
	expr          string
	skipIfRunning bool
}

func cronInfoFromWorkflow(wf workflow.Workflow) (cronTriggerInfo, bool) {
	for _, n := range wf.Nodes {
		if n.Type != workflow.NodeTypeTrigger {
			continue
		}
		tt, _ := n.Data["trigger_type"].(string)
		if tt != "cron" {
			continue
		}
		expr, _ := n.Data["cron"].(string)
		if expr == "" {
			continue
		}
		// default true if not explicitly set to false
		skip := true
		if v, ok := n.Data["skip_if_running"].(bool); ok {
			skip = v
		}
		return cronTriggerInfo{expr: expr, skipIfRunning: skip}, true
	}
	return cronTriggerInfo{}, false
}

func (w *workflowCronWorker) Run(ctx context.Context) error {
	cfg, err := config.Load(config.WithDotEnv(".env"))
	if err != nil {
		return fmt.Errorf("workflow-cron: load config: %w", err)
	}

	rc := rediss.New(cfg.Redis)
	defer func() {
		if err := rc.Close(); err != nil {
			slog.Error("workflow-cron: close redis", "err", err)
		}
	}()

	mc, err := mongodb.New(ctx, cfg.MongoDB)
	if err != nil {
		return fmt.Errorf("workflow-cron: connect mongodb: %w", err)
	}
	defer func() {
		if err := mc.Disconnect(ctx); err != nil {
			slog.Error("workflow-cron: disconnect mongodb", "err", err)
		}
	}()

	repo, err := mongodb.NewWorkflowRepository(ctx, mc.DB())
	if err != nil {
		return fmt.Errorf("workflow-cron: create workflow repo: %w", err)
	}

	runRepo, err := mongodb.NewWorkflowRunRepository(ctx, mc.DB())
	if err != nil {
		return fmt.Errorf("workflow-cron: create run repo: %w", err)
	}

	var encKey []byte
	if trimmed := strings.TrimSpace(cfg.EncryptionKey); trimmed != "" {
		encKey, err = hex.DecodeString(trimmed)
		if err != nil || len(encKey) != 32 {
			return fmt.Errorf("workflow-cron: ENCRYPTION_KEY must be 64 hex chars (32 bytes): %v", err)
		}
	}

	connRepo, err := mongodb.NewConnectionRepository(ctx, mc.DB(), encKey)
	if err != nil {
		return fmt.Errorf("workflow-cron: create connection repo: %w", err)
	}

	defaultDB := mongodb.NewMongoClient(mc.DB())
	connResolver := workflow.NewConnectionResolver(connRepo, defaultDB, rc)
	defer func() {
		if err := connResolver.Close(); err != nil {
			slog.Error("workflow-cron: close connection resolver", "err", err)
		}
	}()

	// All worker exec wiring goes through BuildWorkerExecutor so cron,
	// webhook, ws-client, redis-subscribe, and rabbitmq workers share
	// identical dep sets. Drift between worker types caused the
	// cron-worker bug; this helper is the single source of truth.
	exec, sandboxClose := BuildWorkerExecutor(ctx, cfg, mc.DB(), rc, connResolver)
	defer sandboxClose()

	// 6-field parser w/ optional seconds. Standard 5-field cron
	// (`*/5 * * * *`) still parses since cron.Second is optional.
	// Adds sub-minute scheduling like `*/5 * * * * *` = every 5 seconds.
	parser := cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	scheduler := cron.New(cron.WithParser(parser))
	scheduler.Start()
	defer scheduler.Stop()

	tracked := make(map[string]trackedEntry)
	running := &sync.Map{} // wfID → true while in-flight
	// Re-entrancy guard: skip a sync tick when a previous one is still
	// in flight. Mongo lookups + cron entry diffing can take >tick on
	// large workflow sets; without this guard the ticker stacks
	// goroutines and produces duplicate "scheduled" log spam.
	var syncing atomic.Bool

	runSync := func() {
		if !syncing.CompareAndSwap(false, true) {
			slog.Debug("workflow-cron: skip sync — previous still in flight")
			return
		}
		defer syncing.Store(false)
		syncSchedules(ctx, repo, runRepo, rc, scheduler, tracked, running)
	}
	_ = exec // exec retained for future migrations (eval / ad-hoc dispatch). Cron itself no longer executes runs in-process — it dispatches via the lease worker.

	runSync()

	ticker := time.NewTicker(cronSyncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			runSync()
		}
	}
}

func syncSchedules(
	ctx context.Context,
	repo *mongodb.WorkflowRepository,
	runRepo *mongodb.WorkflowRunRepository,
	rc *rediss.Client,
	scheduler *cron.Cron,
	tracked map[string]trackedEntry,
	running *sync.Map,
) {
	wfs, err := repo.List(ctx)
	if err != nil {
		slog.Error("workflow-cron: list workflows", "err", err)
		return
	}

	// Build map of workflows with cron triggers
	type activeInfo struct {
		expr          string
		updatedAt     time.Time
		name          string
		skipIfRunning bool
	}
	active := make(map[string]activeInfo)
	for _, wf := range wfs {
		// Skip disabled workflows — the existing diff-and-cancel
		// logic tears down any tracked schedule on the next pass.
		if !wf.Enabled {
			continue
		}
		info, ok := cronInfoFromWorkflow(wf)
		if !ok {
			continue
		}
		active[wf.ID] = activeInfo{expr: info.expr, updatedAt: wf.UpdatedAt, name: wf.Name, skipIfRunning: info.skipIfRunning}
	}

	// Add new or update changed
	for wfID, info := range active {
		existing, ok := tracked[wfID]
		if ok && existing.cronExpr == info.expr && existing.updatedAt.Equal(info.updatedAt) {
			continue // no change
		}

		// Remove old entry if exists
		if ok {
			scheduler.Remove(existing.entryID)
			slog.Info("workflow-cron: removed stale", "workflow", wfID, "name", info.name, "old_expr", existing.cronExpr)
		}

		wfID, wfName, skip := wfID, info.name, info.skipIfRunning
		entryID, err := scheduler.AddFunc(info.expr, func() {
			dispatchCronTick(ctx, repo, runRepo, rc, wfID, wfName, skip)
		})
		_ = running // legacy in-memory dedupe; lease path uses Mongo CountInFlightForWorkflow
		if err != nil {
			slog.Error("workflow-cron: schedule failed", "workflow", wfID, "name", wfName, "expr", info.expr, "err", err)
			delete(tracked, wfID)
			continue
		}

		tracked[wfID] = trackedEntry{
			entryID:   entryID,
			updatedAt: info.updatedAt,
			cronExpr:  info.expr,
		}
		slog.Info("workflow-cron: scheduled", "workflow", wfID, "name", wfName, "expr", info.expr, "skip_if_running", skip)
	}

	// Remove deleted workflows
	for wfID, entry := range tracked {
		if _, ok := active[wfID]; !ok {
			scheduler.Remove(entry.entryID)
			delete(tracked, wfID)
			slog.Info("workflow-cron: removed deleted", "workflow", wfID)
		}
	}
}

// dispatchCronTick is the lease-path replacement for the legacy
// runCronWorkflow inline executor. Each cron tick:
//   1. (skip_if_running) consults Mongo for any in-flight run of this
//      workflow. If found, log + skip — the previous run finishes
//      naturally, the next tick gets its own dispatch.
//   2. Loads the workflow doc (validation only — the worker re-loads
//      its own copy on claim, so this fetch just confirms the
//      workflow still exists and the trigger config hasn't been
//      rewritten in the last few seconds).
//   3. Persists a queued WorkflowRun record + publishes a Redis
//      wakeup. The lease worker picks it up on next claim tick.
//
// Replaces the in-process RunResumable call so cron-triggered runs
// are durable, restart-safe, and multi-pod-coordinated alongside
// every other dispatch path. Per the durable-execution plan PR 3.4a.
func dispatchCronTick(
	ctx context.Context,
	wfRepo *mongodb.WorkflowRepository,
	runRepo *mongodb.WorkflowRunRepository,
	rc *rediss.Client,
	wfID, wfName string,
	skipIfRunning bool,
) {
	if skipIfRunning {
		// Mongo is the source of truth — survives this worker's
		// restart, unlike the prior sync.Map. Any non-terminal run
		// for this workflow blocks the next tick.
		count, err := runRepo.CountInFlightForWorkflow(ctx, wfID)
		if err != nil {
			slog.Warn("workflow-cron: count-in-flight failed; dispatching anyway", "workflow", wfID, "err", err)
		} else if count > 0 {
			slog.Info("workflow-cron: skipped (still running)", "workflow", wfID, "name", wfName, "in_flight", count)
			return
		}
	}

	wf, err := wfRepo.GetByID(ctx, wfID)
	if err != nil {
		slog.Error("workflow-cron: fetch workflow", "workflow", wfID, "name", wfName, "err", err)
		return
	}

	now := time.Now().UTC()
	rec := workflow.WorkflowRun{
		ID:         ulid.Make().String(),
		WorkflowID: wf.ID,
		TenantID:   wf.TenantID,
		QueuedAt:   now,
		Status:     workflow.RunStatusQueued,
		Config: wf.Config,
	}
	if _, cerr := runRepo.Create(ctx, rec); cerr != nil {
		slog.Error("workflow-cron: persist run rec failed", "workflow", wfID, "name", wfName, "err", cerr)
		return
	}
	// Best-effort wakeup so a worker idling between claim ticks
	// picks the run up immediately. ClaimLease's tick covers worker
	// pools that missed the publish.
	workflow.PublishWakeup(ctx, rc)

	slog.Info("workflow-cron: dispatched", "workflow", wfID, "name", wfName, "run_id", rec.ID)
}
