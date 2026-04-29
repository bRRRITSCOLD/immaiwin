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

	"github.com/bRRRITSCOLD/immaiwin-go/internal/config"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/mongodb"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/rediss"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/skills"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/workflow"
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
		syncSchedules(ctx, repo, exec, scheduler, tracked, running)
	}

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
	exec *workflow.WorkflowExecutor,
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
			runCronWorkflow(ctx, repo, exec, wfID, wfName, skip, running)
		})
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

func runCronWorkflow(
	ctx context.Context,
	repo *mongodb.WorkflowRepository,
	exec *workflow.WorkflowExecutor,
	wfID, wfName string,
	skipIfRunning bool,
	running *sync.Map,
) {
	if skipIfRunning {
		if _, loaded := running.LoadOrStore(wfID, true); loaded {
			slog.Info("workflow-cron: skipped (still running)", "workflow", wfID, "name", wfName)
			return
		}
		defer running.Delete(wfID)
	}

	start := time.Now()

	wf, err := repo.GetByID(ctx, wfID)
	if err != nil {
		slog.Error("workflow-cron: fetch workflow", "workflow", wfID, "name", wfName, "err", err)
		return
	}

	// Use RunResumable so the run gets persisted into WorkflowRunStore
	// (visible in /runs UI) and the agent's OOB approval gate has a
	// runID to register against.
	outcome, err := exec.RunResumable(ctx, wf, workflow.RunOpts{})
	steps := outcome.Steps
	elapsed := time.Since(start).Round(time.Millisecond)

	var errCount int
	for _, s := range steps {
		if s.Error != "" {
			errCount++
		}
	}

	if err != nil {
		slog.Error("workflow-cron: run failed", "workflow", wfID, "name", wfName, "err", err, "elapsed", elapsed)
	} else {
		slog.Info("workflow-cron: run complete", "workflow", wfID, "name", wfName, "steps", len(steps), "errors", errCount, "elapsed", elapsed)
	}
}
