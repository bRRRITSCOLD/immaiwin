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
	amqp "github.com/rabbitmq/amqp091-go"
)

const rmqSyncInterval = 30 * time.Second

var WorkflowRabbitMQWorker = &workflowRabbitMQWorker{}

type workflowRabbitMQWorker struct{}

func (w *workflowRabbitMQWorker) Name() string { return "workflow-rabbitmq" }

// rmqTriggerInfo holds the RabbitMQ trigger config extracted from a workflow.
type rmqTriggerInfo struct {
	connectionID string
	queue        string
	prefetch     int
	autoAck      bool
}

// trackedConsumer holds the state of a running consumer goroutine.
type trackedConsumer struct {
	cancel    context.CancelFunc
	updatedAt time.Time
	info      rmqTriggerInfo
}

func rmqInfoFromWorkflow(wf workflow.Workflow) (rmqTriggerInfo, bool) {
	for _, n := range wf.Nodes {
		if n.Type != workflow.NodeTypeTrigger {
			continue
		}
		tt, _ := n.Data["trigger_type"].(string)
		if tt != "rabbitmq" {
			continue
		}
		queue, _ := n.Data["queue"].(string)
		if queue == "" {
			continue
		}
		connID, _ := n.Data["connection_id"].(string)
		if connID == "" {
			continue
		}

		prefetch := 1
		if v, ok := n.Data["prefetch"].(float64); ok && v > 0 {
			prefetch = int(v)
		}
		autoAck := false
		if v, ok := n.Data["auto_ack"].(bool); ok {
			autoAck = v
		}

		return rmqTriggerInfo{
			connectionID: connID,
			queue:        queue,
			prefetch:     prefetch,
			autoAck:      autoAck,
		}, true
	}
	return rmqTriggerInfo{}, false
}

func (w *workflowRabbitMQWorker) Run(ctx context.Context) error {
	cfg, err := config.Load(config.WithDotEnv(".env"))
	if err != nil {
		return fmt.Errorf("workflow-rabbitmq: load config: %w", err)
	}

	rc := rediss.New(cfg.Redis)
	defer func() {
		if err := rc.Close(); err != nil {
			slog.Error("workflow-rabbitmq: close redis", "err", err)
		}
	}()

	mc, err := mongodb.New(ctx, cfg.MongoDB)
	if err != nil {
		return fmt.Errorf("workflow-rabbitmq: connect mongodb: %w", err)
	}
	defer func() {
		if err := mc.Disconnect(ctx); err != nil {
			slog.Error("workflow-rabbitmq: disconnect mongodb", "err", err)
		}
	}()

	runRepo, err := mongodb.NewWorkflowRunRepository(ctx, mc.DB())
	if err != nil {
		return fmt.Errorf("workflow-rabbitmq: create run repo: %w", err)
	}

	repo, err := mongodb.NewWorkflowRepository(ctx, mc.DB())
	if err != nil {
		return fmt.Errorf("workflow-rabbitmq: create workflow repo: %w", err)
	}

	var encKey []byte
	if trimmed := strings.TrimSpace(cfg.EncryptionKey); trimmed != "" {
		encKey, err = hex.DecodeString(trimmed)
		if err != nil || len(encKey) != 32 {
			return fmt.Errorf("workflow-rabbitmq: ENCRYPTION_KEY must be 64 hex chars (32 bytes): %v", err)
		}
	}

	connRepo, err := mongodb.NewConnectionRepository(ctx, mc.DB(), encKey)
	if err != nil {
		return fmt.Errorf("workflow-rabbitmq: create connection repo: %w", err)
	}

	defaultDB := mongodb.NewMongoClient(mc.DB())
	connResolver := workflow.NewConnectionResolver(connRepo, defaultDB, rc)
	defer func() {
		if err := connResolver.Close(); err != nil {
			slog.Error("workflow-rabbitmq: close connection resolver", "err", err)
		}
	}()

	exec, sandboxClose := BuildWorkerExecutor(ctx, cfg, mc.DB(), rc, connResolver)
	defer sandboxClose()

	tracked := make(map[string]*trackedConsumer)

	syncRMQConsumers(ctx, repo, runRepo, rc, connRepo, exec, tracked)

	ticker := time.NewTicker(rmqSyncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Cancel all consumer goroutines
			for wfID, tc := range tracked {
				tc.cancel()
				delete(tracked, wfID)
			}
			return nil
		case <-ticker.C:
			syncRMQConsumers(ctx, repo, runRepo, rc, connRepo, exec, tracked)
		}
	}
}

func syncRMQConsumers(
	ctx context.Context,
	repo *mongodb.WorkflowRepository,
	runRepo *mongodb.WorkflowRunRepository,
	rc *rediss.Client,
	connRepo *mongodb.ConnectionRepository,
	exec *workflow.WorkflowExecutor,
	tracked map[string]*trackedConsumer,
) {
	wfs, err := repo.List(ctx)
	if err != nil {
		slog.Error("workflow-rabbitmq: list workflows", "err", err)
		return
	}

	type activeEntry struct {
		info      rmqTriggerInfo
		updatedAt time.Time
		name      string
	}
	active := make(map[string]activeEntry)
	for _, wf := range wfs {
		// Skip disabled workflows — the existing diff-and-cancel
		// logic tears down any tracked consumer on the next pass
		// (messages stay in the queue / hit the queue's TTL+DLQ
		// per RMQ config).
		if !wf.Enabled {
			continue
		}
		info, ok := rmqInfoFromWorkflow(wf)
		if !ok {
			continue
		}
		active[wf.ID] = activeEntry{info: info, updatedAt: wf.UpdatedAt, name: wf.Name}
	}

	// Add new or update changed
	for wfID, entry := range active {
		existing, ok := tracked[wfID]
		if ok && existing.updatedAt.Equal(entry.updatedAt) {
			continue // no change
		}

		// Cancel old consumer if exists
		if ok {
			existing.cancel()
			slog.Info("workflow-rabbitmq: stopped stale consumer", "workflow", wfID, "name", entry.name)
		}

		consumerCtx, cancel := context.WithCancel(ctx)
		tracked[wfID] = &trackedConsumer{
			cancel:    cancel,
			updatedAt: entry.updatedAt,
			info:      entry.info,
		}

		go consumeLoop(consumerCtx, repo, runRepo, rc, connRepo, exec, wfID, entry.name, entry.info)
		slog.Info("workflow-rabbitmq: started consumer", "workflow", wfID, "name", entry.name, "queue", entry.info.queue)
	}

	// Remove deleted workflows
	for wfID, tc := range tracked {
		if _, ok := active[wfID]; !ok {
			tc.cancel()
			delete(tracked, wfID)
			slog.Info("workflow-rabbitmq: removed deleted consumer", "workflow", wfID)
		}
	}
}

func consumeLoop(
	ctx context.Context,
	repo *mongodb.WorkflowRepository,
	runRepo *mongodb.WorkflowRunRepository,
	rc *rediss.Client,
	connRepo *mongodb.ConnectionRepository,
	exec *workflow.WorkflowExecutor,
	wfID, wfName string,
	info rmqTriggerInfo,
) {
	attempt := 0
	for {
		if ctx.Err() != nil {
			return
		}

		err := consumeOnce(ctx, repo, runRepo, rc, connRepo, exec, wfID, wfName, info)
		if ctx.Err() != nil {
			return // clean shutdown
		}

		// Reconnect with exponential backoff
		attempt++
		delay := time.Duration(math.Min(float64(time.Second)*math.Pow(2, float64(attempt)), float64(60*time.Second)))
		slog.Warn("workflow-rabbitmq: consumer disconnected, reconnecting",
			"workflow", wfID, "name", wfName, "err", err, "attempt", attempt, "backoff", delay)

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

func consumeOnce(
	ctx context.Context,
	repo *mongodb.WorkflowRepository,
	runRepo *mongodb.WorkflowRunRepository,
	rc *rediss.Client,
	connRepo *mongodb.ConnectionRepository,
	exec *workflow.WorkflowExecutor,
	wfID, wfName string,
	info rmqTriggerInfo,
) error {
	// Resolve connection
	conn, err := connRepo.GetByID(ctx, info.connectionID)
	if err != nil {
		return fmt.Errorf("resolve connection: %w", err)
	}

	url := workflow.BuildRabbitMQURL(conn.Config)
	amqpConn, err := amqp.Dial(url)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer amqpConn.Close() //nolint:errcheck

	ch, err := amqpConn.Channel()
	if err != nil {
		return fmt.Errorf("channel: %w", err)
	}
	defer ch.Close() //nolint:errcheck

	if err := ch.Qos(info.prefetch, 0, false); err != nil {
		return fmt.Errorf("qos: %w", err)
	}

	consumerTag := fmt.Sprintf("burrow-%s", wfID)
	msgs, err := ch.Consume(
		info.queue,
		consumerTag,
		info.autoAck,
		false, // exclusive
		false, // noLocal
		false, // noWait
		nil,
	)
	if err != nil {
		return fmt.Errorf("consume: %w", err)
	}

	slog.Info("workflow-rabbitmq: consuming", "workflow", wfID, "name", wfName, "queue", info.queue)

	// Watch for AMQP connection errors
	connErr := amqpConn.NotifyClose(make(chan *amqp.Error, 1))

	for {
		select {
		case <-ctx.Done():
			return nil
		case amqpErr := <-connErr:
			if amqpErr != nil {
				return fmt.Errorf("amqp: %s", amqpErr.Error())
			}
			return fmt.Errorf("amqp: connection closed")
		case delivery, ok := <-msgs:
			if !ok {
				return fmt.Errorf("delivery channel closed")
			}
			processDelivery(ctx, repo, runRepo, rc, wfID, wfName, info, delivery)
		}
	}
}

// processDelivery dispatches the run via the lease worker (PR 3.4a)
// instead of executing it inline. Each delivery:
//   1. Fetches the workflow doc (still required to know the trigger
//      shape exists and the workflow hasn't been deleted).
//   2. Persists a queued WorkflowRun rec with the message body as
//      trigger_input.
//   3. Publishes a Redis wakeup so an idle workflow-executor picks
//      it up immediately.
//   4. Acks the AMQP delivery — the message is durable in Mongo
//      from this point forward, so AMQP's redelivery is no longer
//      needed for run-survival. Manual-ack mode preserves so a
//      Mongo write failure still NACKs (requeue) before this point.
func processDelivery(
	ctx context.Context,
	repo *mongodb.WorkflowRepository,
	runRepo *mongodb.WorkflowRunRepository,
	rc *rediss.Client,
	wfID, wfName string,
	info rmqTriggerInfo,
	delivery amqp.Delivery,
) {
	// Parse body: try JSON, fallback to string.
	var body any
	if err := json.Unmarshal(delivery.Body, &body); err != nil {
		body = string(delivery.Body)
	}

	wf, err := repo.GetByID(ctx, wfID)
	if err != nil {
		slog.Error("workflow-rabbitmq: fetch workflow", "workflow", wfID, "name", wfName, "err", err)
		if !info.autoAck {
			_ = delivery.Nack(false, true) // requeue
		}
		return
	}

	now := time.Now().UTC()
	rec := workflow.WorkflowRun{
		ID:           ulid.Make().String(),
		WorkflowID:   wf.ID,
		TenantID:     wf.TenantID,
		QueuedAt:     now,
		Status:       workflow.RunStatusQueued,
		Config: wf.Config,
		TriggerInput: body,
	}
	if _, cerr := runRepo.Create(ctx, rec); cerr != nil {
		slog.Error("workflow-rabbitmq: persist run rec failed",
			"workflow", wfID, "name", wfName, "err", cerr)
		if !info.autoAck {
			_ = delivery.Nack(false, true) // requeue — message survives, retry on next delivery
		}
		return
	}
	workflow.PublishWakeup(ctx, rc)

	slog.Info("workflow-rabbitmq: dispatched",
		"workflow", wfID, "name", wfName,
		"run_id", rec.ID)
	if !info.autoAck {
		_ = delivery.Ack(false)
	}
}
