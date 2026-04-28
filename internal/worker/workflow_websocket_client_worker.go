package worker

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/config"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/mongodb"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/polymarket"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/rediss"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/schwab"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/workflow"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

const wsSyncInterval = 30 * time.Second

var WorkflowWebSocketClientWorker = &workflowWSClientWorker{}

type workflowWSClientWorker struct{}

func (w *workflowWSClientWorker) Name() string { return "workflow-ws-client" }

// wsTriggerInfo is an alias for the shared type.
type wsTriggerInfo = workflow.WSTriggerInfo

// trackedWSConsumer holds the state of a running WS consumer goroutine.
type trackedWSConsumer struct {
	cancel    context.CancelFunc
	updatedAt time.Time
	info      wsTriggerInfo
}

// wsInfoFromWorkflow delegates to the shared workflow package.
var wsInfoFromWorkflow = workflow.WSInfoFromWorkflow

func (w *workflowWSClientWorker) Run(ctx context.Context) error {
	cfg, err := config.Load(config.WithDotEnv(".env"))
	if err != nil {
		return fmt.Errorf("workflow-ws-client: load config: %w", err)
	}

	rc := rediss.New(cfg.Redis)
	defer func() {
		if err := rc.Close(); err != nil {
			slog.Error("workflow-ws-client: close redis", "err", err)
		}
	}()

	mc, err := mongodb.New(ctx, cfg.MongoDB)
	if err != nil {
		return fmt.Errorf("workflow-ws-client: connect mongodb: %w", err)
	}
	defer func() {
		if err := mc.Disconnect(ctx); err != nil {
			slog.Error("workflow-ws-client: disconnect mongodb", "err", err)
		}
	}()

	repo, err := mongodb.NewWorkflowRepository(ctx, mc.DB())
	if err != nil {
		return fmt.Errorf("workflow-ws-client: create workflow repo: %w", err)
	}

	var encKey []byte
	if trimmed := strings.TrimSpace(cfg.EncryptionKey); trimmed != "" {
		encKey, err = hex.DecodeString(trimmed)
		if err != nil || len(encKey) != 32 {
			return fmt.Errorf("workflow-ws-client: ENCRYPTION_KEY must be 64 hex chars (32 bytes): %v", err)
		}
	}

	connRepo, err := mongodb.NewConnectionRepository(ctx, mc.DB(), encKey)
	if err != nil {
		return fmt.Errorf("workflow-ws-client: create connection repo: %w", err)
	}

	defaultDB := mongodb.NewMongoClient(mc.DB())
	connResolver := workflow.NewConnectionResolver(connRepo, defaultDB, rc)
	defer func() {
		if err := connResolver.Close(); err != nil {
			slog.Error("workflow-ws-client: close connection resolver", "err", err)
		}
	}()

	exec := &workflow.WorkflowExecutor{
		HTTPClient:   &http.Client{Timeout: 30 * time.Second},
		DB:           defaultDB,
		Redis:        rc,
		ConnResolver: connResolver,
	}

	tracked := make(map[string]*trackedWSConsumer)

	syncWSConsumers(ctx, repo, connRepo, mc.DB(), exec, tracked)

	ticker := time.NewTicker(wsSyncInterval)
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
			syncWSConsumers(ctx, repo, connRepo, mc.DB(), exec, tracked)
		}
	}
}

func syncWSConsumers(
	ctx context.Context,
	repo *mongodb.WorkflowRepository,
	connRepo *mongodb.ConnectionRepository,
	db *mongo.Database,
	exec *workflow.WorkflowExecutor,
	tracked map[string]*trackedWSConsumer,
) {
	wfs, err := repo.List(ctx)
	if err != nil {
		slog.Error("workflow-ws-client: list workflows", "err", err)
		return
	}

	type activeEntry struct {
		info      wsTriggerInfo
		updatedAt time.Time
		name      string
	}
	active := make(map[string]activeEntry)
	for _, wf := range wfs {
		info, ok := wsInfoFromWorkflow(wf)
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

		// Cancel old consumer before creating new one
		if ok {
			existing.cancel()
			slog.Info("workflow-ws-client: stopped stale consumer", "workflow", wfID, "name", entry.name)
		}

		consumerCtx, cancel := context.WithCancel(ctx)
		tracked[wfID] = &trackedWSConsumer{
			cancel:    cancel,
			updatedAt: entry.updatedAt,
			info:      entry.info,
		}

		switch entry.info.TriggerType {
		case "polymarket_ws":
			go runPolymarketWSConsumer(consumerCtx, repo, exec, wfID, entry.name, entry.info)
		case "schwab_ws":
			go runSchwabWSConsumer(consumerCtx, repo, connRepo, db, exec, wfID, entry.name, entry.info)
		default:
			slog.Error("workflow-ws-client: unsupported trigger type", "type", entry.info.TriggerType, "workflow", wfID)
			cancel()
			delete(tracked, wfID)
			continue
		}

		slog.Info("workflow-ws-client: started consumer",
			"workflow", wfID, "name", entry.name,
			"trigger_type", entry.info.TriggerType,
			"asset_ids", len(entry.info.AssetIDs),
			"symbols", len(entry.info.Symbols),
		)
	}

	// Remove deleted workflows
	for wfID, tc := range tracked {
		if _, ok := active[wfID]; !ok {
			tc.cancel()
			delete(tracked, wfID)
			slog.Info("workflow-ws-client: removed deleted consumer", "workflow", wfID)
		}
	}
}

// runPolymarketWSConsumer manages the Polymarket WS lifecycle with reconnect backoff.
func runPolymarketWSConsumer(
	ctx context.Context,
	repo *mongodb.WorkflowRepository,
	exec *workflow.WorkflowExecutor,
	wfID, wfName string,
	info wsTriggerInfo,
) {
	backoff := &StreamBackoff{}

	for {
		if ctx.Err() != nil {
			return
		}

		err := runPolymarketWSOnce(ctx, repo, exec, wfID, wfName, info, backoff)
		if ctx.Err() != nil {
			return // clean shutdown
		}

		delay := backoff.Next(err)
		slog.Warn("workflow-ws-client: polymarket WS disconnected, reconnecting",
			"workflow", wfID, "name", wfName, "err", err, "backoff", delay)

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// runPolymarketWSOnce connects to Polymarket WS, subscribes to asset IDs, and
// triggers workflow execution for each trade event.
func runPolymarketWSOnce(
	ctx context.Context,
	repo *mongodb.WorkflowRepository,
	exec *workflow.WorkflowExecutor,
	wfID, wfName string,
	info wsTriggerInfo,
	backoff *StreamBackoff,
) error {
	client, err := polymarket.New(polymarket.ClientConfig{})
	if err != nil {
		return fmt.Errorf("init polymarket client: %w", err)
	}
	defer client.Close() //nolint:errcheck

	tradeCh, err := client.WatchTrades(ctx, info.AssetIDs)
	if err != nil {
		return fmt.Errorf("subscribe trades: %w", err)
	}

	slog.Info("workflow-ws-client: polymarket WS connected",
		"workflow", wfID, "name", wfName, "asset_ids", len(info.AssetIDs))

	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-tradeCh:
			if !ok {
				return fmt.Errorf("trade channel closed")
			}

			// Reset backoff on live data
			backoff.Reset()

			tradeInput := workflow.PolymarketTradeInput(event)

			// Fetch fresh workflow (picks up node changes without consumer restart)
			wf, err := repo.GetByID(ctx, wfID)
			if err != nil {
				slog.Error("workflow-ws-client: fetch workflow", "workflow", wfID, "name", wfName, "err", err)
				continue
			}

			start := time.Now()
			steps, err := exec.Run(ctx, wf, "", tradeInput)
			elapsed := time.Since(start).Round(time.Millisecond)

			var errCount int
			for _, s := range steps {
				if s.Error != "" {
					errCount++
				}
			}

			if err != nil {
				slog.Error("workflow-ws-client: run failed",
					"workflow", wfID, "name", wfName, "err", err, "elapsed", elapsed)
			} else if errCount > 0 {
				slog.Warn("workflow-ws-client: run partial errors",
					"workflow", wfID, "name", wfName, "steps", len(steps), "errors", errCount, "elapsed", elapsed)
			}
		}
	}
}

// runSchwabWSConsumer manages the Schwab WS lifecycle with reconnect backoff.
func runSchwabWSConsumer(
	ctx context.Context,
	repo *mongodb.WorkflowRepository,
	connRepo *mongodb.ConnectionRepository,
	db *mongo.Database,
	exec *workflow.WorkflowExecutor,
	wfID, wfName string,
	info wsTriggerInfo,
) {
	backoff := &StreamBackoff{}

	for {
		if ctx.Err() != nil {
			return
		}

		err := runSchwabWSOnce(ctx, repo, connRepo, db, exec, wfID, wfName, info, backoff)
		if ctx.Err() != nil {
			return
		}

		delay := backoff.Next(err)
		slog.Warn("workflow-ws-client: schwab WS disconnected, reconnecting",
			"workflow", wfID, "name", wfName, "err", err, "backoff", delay)

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// runSchwabWSOnce connects to Schwab streamer WS, subscribes to symbols, and
// triggers workflow execution for each trade event.
func runSchwabWSOnce(
	ctx context.Context,
	repo *mongodb.WorkflowRepository,
	connRepo *mongodb.ConnectionRepository,
	db *mongo.Database,
	exec *workflow.WorkflowExecutor,
	wfID, wfName string,
	info wsTriggerInfo,
	backoff *StreamBackoff,
) error {
	// Load connection to get client credentials
	conn, err := connRepo.GetByID(ctx, info.ConnectionID)
	if err != nil {
		return fmt.Errorf("load connection %s: %w", info.ConnectionID, err)
	}

	// Build per-connection token manager
	tm := workflow.BuildSchwabTokenManager(conn.Config, db, conn.ID)
	if err := tm.Load(ctx); err != nil {
		return fmt.Errorf("load tokens: %w", err)
	}
	if !tm.IsAuthorized() {
		return fmt.Errorf("connection %s not authorized — complete OAuth flow first", info.ConnectionID)
	}

	// Start background token refresher
	refreshCtx, refreshCancel := context.WithCancel(ctx)
	defer refreshCancel()
	tm.RunRefresher(refreshCtx)

	client := schwab.NewClient(tm)
	defer client.Close() //nolint:errcheck

	switch info.StreamType {
	case "options":
		return schwabStreamOptions(ctx, client, repo, exec, wfID, wfName, info, backoff)
	case "futures":
		return schwabStreamFutures(ctx, client, repo, exec, wfID, wfName, info, backoff)
	default:
		return fmt.Errorf("unsupported schwab stream_type: %s", info.StreamType)
	}
}

func schwabStreamOptions(
	ctx context.Context,
	client *schwab.Client,
	repo *mongodb.WorkflowRepository,
	exec *workflow.WorkflowExecutor,
	wfID, wfName string,
	info wsTriggerInfo,
	backoff *StreamBackoff,
) error {
	tradeCh, errCh, err := client.StreamTradesEx(ctx, info.Symbols)
	if err != nil {
		return fmt.Errorf("stream options: %w", err)
	}

	slog.Info("workflow-ws-client: schwab options WS connected",
		"workflow", wfID, "name", wfName, "symbols", len(info.Symbols))

	for {
		select {
		case <-ctx.Done():
			return nil
		case wsErr := <-errCh:
			return fmt.Errorf("options stream closed: %w", wsErr)
		case trade, ok := <-tradeCh:
			if !ok {
				return fmt.Errorf("options trade channel closed")
			}
			backoff.Reset()

			tradeInput := workflow.SchwabOptionsTradeInput(trade)

			wf, err := repo.GetByID(ctx, wfID)
			if err != nil {
				slog.Error("workflow-ws-client: fetch workflow", "workflow", wfID, "name", wfName, "err", err)
				continue
			}

			start := time.Now()
			steps, err := exec.Run(ctx, wf, "", tradeInput)
			elapsed := time.Since(start).Round(time.Millisecond)

			var errCount int
			for _, s := range steps {
				if s.Error != "" {
					errCount++
				}
			}

			if err != nil {
				slog.Error("workflow-ws-client: schwab run failed",
					"workflow", wfID, "name", wfName, "err", err, "elapsed", elapsed)
			} else if errCount > 0 {
				slog.Warn("workflow-ws-client: schwab run partial errors",
					"workflow", wfID, "name", wfName, "steps", len(steps), "errors", errCount, "elapsed", elapsed)
			}
		}
	}
}

func schwabStreamFutures(
	ctx context.Context,
	client *schwab.Client,
	repo *mongodb.WorkflowRepository,
	exec *workflow.WorkflowExecutor,
	wfID, wfName string,
	info wsTriggerInfo,
	backoff *StreamBackoff,
) error {
	tradeCh, errCh, err := client.StreamFuturesEx(ctx, info.Symbols)
	if err != nil {
		return fmt.Errorf("stream futures: %w", err)
	}

	slog.Info("workflow-ws-client: schwab futures WS connected",
		"workflow", wfID, "name", wfName, "symbols", len(info.Symbols))

	for {
		select {
		case <-ctx.Done():
			return nil
		case wsErr := <-errCh:
			return fmt.Errorf("futures stream closed: %w", wsErr)
		case trade, ok := <-tradeCh:
			if !ok {
				return fmt.Errorf("futures trade channel closed")
			}
			backoff.Reset()

			tradeInput := workflow.SchwabFuturesTradeInput(trade)

			wf, err := repo.GetByID(ctx, wfID)
			if err != nil {
				slog.Error("workflow-ws-client: fetch workflow", "workflow", wfID, "name", wfName, "err", err)
				continue
			}

			start := time.Now()
			steps, err := exec.Run(ctx, wf, "", tradeInput)
			elapsed := time.Since(start).Round(time.Millisecond)

			var errCount int
			for _, s := range steps {
				if s.Error != "" {
					errCount++
				}
			}

			if err != nil {
				slog.Error("workflow-ws-client: schwab run failed",
					"workflow", wfID, "name", wfName, "err", err, "elapsed", elapsed)
			} else if errCount > 0 {
				slog.Warn("workflow-ws-client: schwab run partial errors",
					"workflow", wfID, "name", wfName, "steps", len(steps), "errors", errCount, "elapsed", elapsed)
			}
		}
	}
}
