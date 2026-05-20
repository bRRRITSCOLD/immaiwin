package worker

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/config"
	"github.com/bRRRITSCOLD/burrow/internal/mongodb"
	"github.com/bRRRITSCOLD/burrow/internal/rediss"
	"github.com/bRRRITSCOLD/burrow/internal/workflow"
	"github.com/oklog/ulid/v2"
	"github.com/redis/go-redis/v9"
)

const redisSubSyncInterval = 30 * time.Second

var WorkflowRedisSubscribeWorker = &workflowRedisSubscribeWorker{}

type workflowRedisSubscribeWorker struct{}

func (w *workflowRedisSubscribeWorker) Name() string { return "workflow-redis-subscribe" }

// redisSubTriggerInfo holds the Redis subscribe trigger config extracted from
// a workflow's trigger node.
type redisSubTriggerInfo struct {
	connectionID string
	channels     []string
	patterns     []string
}

type trackedRedisSubscriber struct {
	cancel    context.CancelFunc
	updatedAt time.Time
	info      redisSubTriggerInfo
}

func redisSubInfoFromWorkflow(wf workflow.Workflow) (redisSubTriggerInfo, bool) {
	for _, n := range wf.Nodes {
		if n.Type != workflow.NodeTypeTrigger {
			continue
		}
		tt, _ := n.Data["trigger_type"].(string)
		if tt != "redis_subscribe" {
			continue
		}
		connID, _ := n.Data["connection_id"].(string)
		if connID == "" {
			continue
		}
		channels := splitNonEmptyLines(getString(n.Data, "channels"))
		patterns := splitNonEmptyLines(getString(n.Data, "patterns"))
		if len(channels) == 0 && len(patterns) == 0 {
			continue
		}
		return redisSubTriggerInfo{
			connectionID: connID,
			channels:     channels,
			patterns:     patterns,
		}, true
	}
	return redisSubTriggerInfo{}, false
}

func getString(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func splitNonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func (w *workflowRedisSubscribeWorker) Run(ctx context.Context) error {
	cfg, err := config.Load(config.WithDotEnv(".env"))
	if err != nil {
		return fmt.Errorf("workflow-redis-subscribe: load config: %w", err)
	}

	rc := rediss.New(cfg.Redis)
	defer func() {
		if err := rc.Close(); err != nil {
			slog.Error("workflow-redis-subscribe: close redis", "err", err)
		}
	}()

	mc, err := mongodb.New(ctx, cfg.MongoDB)
	if err != nil {
		return fmt.Errorf("workflow-redis-subscribe: connect mongodb: %w", err)
	}
	defer func() {
		if err := mc.Disconnect(ctx); err != nil {
			slog.Error("workflow-redis-subscribe: disconnect mongodb", "err", err)
		}
	}()

	runRepo, err := mongodb.NewWorkflowRunRepository(ctx, mc.DB())
	if err != nil {
		return fmt.Errorf("workflow-redis-subscribe: create run repo: %w", err)
	}

	repo, err := mongodb.NewWorkflowRepository(ctx, mc.DB())
	if err != nil {
		return fmt.Errorf("workflow-redis-subscribe: create workflow repo: %w", err)
	}

	var encKey []byte
	if trimmed := strings.TrimSpace(cfg.EncryptionKey); trimmed != "" {
		encKey, err = hex.DecodeString(trimmed)
		if err != nil || len(encKey) != 32 {
			return fmt.Errorf("workflow-redis-subscribe: ENCRYPTION_KEY must be 64 hex chars (32 bytes): %v", err)
		}
	}

	connRepo, err := mongodb.NewConnectionRepository(ctx, mc.DB(), encKey)
	if err != nil {
		return fmt.Errorf("workflow-redis-subscribe: create connection repo: %w", err)
	}

	defaultDB := mongodb.NewMongoClient(mc.DB())
	connResolver := workflow.NewConnectionResolver(connRepo, defaultDB, rc)
	defer func() {
		if err := connResolver.Close(); err != nil {
			slog.Error("workflow-redis-subscribe: close connection resolver", "err", err)
		}
	}()

	exec, sandboxClose := BuildWorkerExecutor(ctx, cfg, mc.DB(), rc, connResolver)
	defer sandboxClose()

	tracked := make(map[string]*trackedRedisSubscriber)

	syncRedisSubscribers(ctx, repo, runRepo, rc, connRepo, exec, tracked)

	ticker := time.NewTicker(redisSubSyncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			for wfID, ts := range tracked {
				ts.cancel()
				delete(tracked, wfID)
			}
			return nil
		case <-ticker.C:
			syncRedisSubscribers(ctx, repo, runRepo, rc, connRepo, exec, tracked)
		}
	}
}

func syncRedisSubscribers(
	ctx context.Context,
	repo *mongodb.WorkflowRepository,
	runRepo *mongodb.WorkflowRunRepository,
	rc *rediss.Client,
	connRepo *mongodb.ConnectionRepository,
	exec *workflow.WorkflowExecutor,
	tracked map[string]*trackedRedisSubscriber,
) {
	wfs, err := repo.List(ctx)
	if err != nil {
		slog.Error("workflow-redis-subscribe: list workflows", "err", err)
		return
	}

	type activeEntry struct {
		info      redisSubTriggerInfo
		updatedAt time.Time
		name      string
	}
	active := make(map[string]activeEntry)
	for _, wf := range wfs {
		// Skip disabled workflows — the existing diff-and-cancel
		// logic drops the subscription on the next pass. Note:
		// messages published while a Redis-subscribe workflow is
		// disabled are LOST (Redis pub/sub has no replay). The UI
		// flags this when the user disables a redis-sub trigger.
		if !wf.Enabled {
			continue
		}
		info, ok := redisSubInfoFromWorkflow(wf)
		if !ok {
			continue
		}
		active[wf.ID] = activeEntry{info: info, updatedAt: wf.UpdatedAt, name: wf.Name}
	}

	for wfID, entry := range active {
		existing, ok := tracked[wfID]
		if ok && existing.updatedAt.Equal(entry.updatedAt) {
			continue
		}
		if ok {
			existing.cancel()
			slog.Info("workflow-redis-subscribe: stopped stale subscriber", "workflow", wfID, "name", entry.name)
		}

		subCtx, cancel := context.WithCancel(ctx)
		tracked[wfID] = &trackedRedisSubscriber{
			cancel:    cancel,
			updatedAt: entry.updatedAt,
			info:      entry.info,
		}

		go subscribeLoop(subCtx, repo, runRepo, rc, connRepo, exec, wfID, entry.name, entry.info)
		slog.Info("workflow-redis-subscribe: started subscriber",
			"workflow", wfID, "name", entry.name,
			"channels", entry.info.channels, "patterns", entry.info.patterns)
	}

	for wfID, ts := range tracked {
		if _, ok := active[wfID]; !ok {
			ts.cancel()
			delete(tracked, wfID)
			slog.Info("workflow-redis-subscribe: removed deleted subscriber", "workflow", wfID)
		}
	}
}

func subscribeLoop(
	ctx context.Context,
	repo *mongodb.WorkflowRepository,
	runRepo *mongodb.WorkflowRunRepository,
	rc *rediss.Client,
	connRepo *mongodb.ConnectionRepository,
	exec *workflow.WorkflowExecutor,
	wfID, wfName string,
	info redisSubTriggerInfo,
) {
	attempt := 0
	for {
		if ctx.Err() != nil {
			return
		}
		err := subscribeOnce(ctx, repo, runRepo, rc, connRepo, exec, wfID, wfName, info)
		if ctx.Err() != nil {
			return
		}
		attempt++
		delay := time.Duration(math.Min(float64(time.Second)*math.Pow(2, float64(attempt)), float64(60*time.Second)))
		slog.Warn("workflow-redis-subscribe: subscriber disconnected, reconnecting",
			"workflow", wfID, "name", wfName, "err", err, "attempt", attempt, "backoff", delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

func subscribeOnce(
	ctx context.Context,
	repo *mongodb.WorkflowRepository,
	runRepo *mongodb.WorkflowRunRepository,
	rc *rediss.Client,
	connRepo *mongodb.ConnectionRepository,
	exec *workflow.WorkflowExecutor,
	wfID, wfName string,
	info redisSubTriggerInfo,
) error {
	conn, err := connRepo.GetByID(ctx, info.connectionID)
	if err != nil {
		return fmt.Errorf("resolve connection: %w", err)
	}
	if conn.Type != workflow.ConnectionTypeRedis {
		return fmt.Errorf("connection %q is %s, expected redis", info.connectionID, conn.Type)
	}

	rdb := redis.NewClient(workflow.BuildRedisOpts(conn.Config))
	defer rdb.Close() //nolint:errcheck

	if err := rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("ping redis: %w", err)
	}

	var pubsub *redis.PubSub
	switch {
	case len(info.channels) > 0 && len(info.patterns) > 0:
		pubsub = rdb.Subscribe(ctx, info.channels...)
		if err := pubsub.PSubscribe(ctx, info.patterns...); err != nil {
			_ = pubsub.Close()
			return fmt.Errorf("psubscribe: %w", err)
		}
	case len(info.channels) > 0:
		pubsub = rdb.Subscribe(ctx, info.channels...)
	case len(info.patterns) > 0:
		pubsub = rdb.PSubscribe(ctx, info.patterns...)
	default:
		return fmt.Errorf("no channels or patterns configured")
	}
	defer pubsub.Close() //nolint:errcheck

	// Drain initial subscription confirmations to surface any subscribe error
	// before entering the receive loop.
	if _, err := pubsub.Receive(ctx); err != nil {
		return fmt.Errorf("receive subscribe confirmation: %w", err)
	}

	ch := pubsub.Channel()
	slog.Info("workflow-redis-subscribe: subscribed",
		"workflow", wfID, "name", wfName,
		"channels", info.channels, "patterns", info.patterns)

	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-ch:
			if !ok {
				return fmt.Errorf("pubsub channel closed")
			}
			processRedisMessage(ctx, repo, runRepo, rc, wfID, wfName, msg)
		}
	}
}

// processRedisMessage dispatches the run via the lease worker
// (PR 3.4a). Redis pub/sub has no ack — once we persist the run
// record + publish wakeup, the message is durable. Worker SIGKILL
// before persistence loses the message (matches pub/sub semantics:
// fire-and-forget).
func processRedisMessage(
	ctx context.Context,
	repo *mongodb.WorkflowRepository,
	runRepo *mongodb.WorkflowRunRepository,
	rc *rediss.Client,
	wfID, wfName string,
	msg *redis.Message,
) {
	// Parse payload: try JSON, fall back to string.
	var payload any
	if err := json.Unmarshal([]byte(msg.Payload), &payload); err != nil {
		payload = msg.Payload
	}

	body := map[string]any{
		"channel": msg.Channel,
		"pattern": msg.Pattern,
		"payload": payload,
	}

	wf, err := repo.GetByID(ctx, wfID)
	if err != nil {
		slog.Error("workflow-redis-subscribe: fetch workflow", "workflow", wfID, "name", wfName, "err", err)
		return
	}

	now := time.Now().UTC()
	rec := workflow.WorkflowRun{
		ID:           ulid.Make().String(),
		WorkflowID:   wf.ID,
		TenantID:     wf.TenantID,
		QueuedAt:     now,
		Status:       workflow.RunStatusQueued,
		Params:       wf.Params,
		TriggerInput: body,
	}
	if _, cerr := runRepo.Create(ctx, rec); cerr != nil {
		slog.Error("workflow-redis-subscribe: persist run rec failed",
			"workflow", wfID, "name", wfName, "channel", msg.Channel, "err", cerr)
		return
	}
	workflow.PublishWakeup(ctx, rc)

	slog.Info("workflow-redis-subscribe: dispatched",
		"workflow", wfID, "name", wfName, "channel", msg.Channel, "run_id", rec.ID)
}
