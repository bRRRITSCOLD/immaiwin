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

	syncRMQConsumers(ctx, repo, connRepo, exec, tracked)

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
			syncRMQConsumers(ctx, repo, connRepo, exec, tracked)
		}
	}
}

func syncRMQConsumers(
	ctx context.Context,
	repo *mongodb.WorkflowRepository,
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

		go consumeLoop(consumerCtx, repo, connRepo, exec, wfID, entry.name, entry.info)
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

		err := consumeOnce(ctx, repo, connRepo, exec, wfID, wfName, info)
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
			processDelivery(ctx, repo, exec, wfID, wfName, info, delivery)
		}
	}
}

func processDelivery(
	ctx context.Context,
	repo *mongodb.WorkflowRepository,
	exec *workflow.WorkflowExecutor,
	wfID, wfName string,
	info rmqTriggerInfo,
	delivery amqp.Delivery,
) {
	start := time.Now()

	// Parse body: try JSON, fallback to string
	var body any
	if err := json.Unmarshal(delivery.Body, &body); err != nil {
		body = string(delivery.Body)
	}

	// Fetch fresh workflow (picks up node changes without consumer restart)
	wf, err := repo.GetByID(ctx, wfID)
	if err != nil {
		slog.Error("workflow-rabbitmq: fetch workflow", "workflow", wfID, "name", wfName, "err", err)
		if !info.autoAck {
			_ = delivery.Nack(false, true) // requeue
		}
		return
	}

	steps, err := exec.Run(ctx, wf, "", body)
	elapsed := time.Since(start).Round(time.Millisecond)

	var errCount int
	for _, s := range steps {
		if s.Error != "" {
			errCount++
		}
	}

	if err != nil {
		slog.Error("workflow-rabbitmq: run failed", "workflow", wfID, "name", wfName, "err", err, "elapsed", elapsed)
		if !info.autoAck {
			_ = delivery.Nack(false, true) // requeue
		}
		return
	}

	slog.Info("workflow-rabbitmq: run complete", "workflow", wfID, "name", wfName, "steps", len(steps), "errors", errCount, "elapsed", elapsed)
	if !info.autoAck {
		_ = delivery.Ack(false)
	}
}
