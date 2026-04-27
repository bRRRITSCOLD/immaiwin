package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/polymarket"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/schwab"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/workflow"
	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// PreviewWorkflowWS streams live WS trigger data via SSE so the user can
// capture messages and send them through the workflow pipeline.
func PreviewWorkflowWS(
	wfStore WorkflowStore,
	connStore ConnectionStore,
	db *mongo.Database,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if id == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
			return
		}

		wf, err := wfStore.GetByID(c.Request.Context(), id)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
			return
		}

		info, ok := workflow.WSInfoFromWorkflow(wf)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "workflow has no WS trigger"})
			return
		}

		// SSE headers
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("Access-Control-Allow-Origin", "*")

		fmt.Fprintf(c.Writer, ": connected\n\n")
		c.Writer.Flush()

		ctx, cancel := context.WithCancel(c.Request.Context())
		defer cancel()

		dataCh := make(chan []byte, 64)
		errCh := make(chan error, 1)

		switch info.TriggerType {
		case "polymarket_ws":
			go streamPolymarketPreview(ctx, info, dataCh, errCh)
		case "schwab_ws":
			go streamSchwabPreview(ctx, info, connStore, db, dataCh, errCh)
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported WS trigger type"})
			return
		}

		// Send status event
		statusJSON, _ := json.Marshal(map[string]any{
			"connected":    true,
			"trigger_type": info.TriggerType,
		})
		fmt.Fprintf(c.Writer, "event: status\ndata: %s\n\n", statusJSON)
		c.Writer.Flush()

		keepalive := time.NewTicker(15 * time.Second)
		defer keepalive.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-keepalive.C:
				fmt.Fprintf(c.Writer, ": keepalive\n\n")
				c.Writer.Flush()
			case wsErr := <-errCh:
				errJSON, _ := json.Marshal(map[string]string{"error": wsErr.Error()})
				fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", errJSON)
				c.Writer.Flush()
				return
			case data, ok := <-dataCh:
				if !ok {
					return
				}
				fmt.Fprintf(c.Writer, "event: message\ndata: %s\n\n", data)
				c.Writer.Flush()
			}
		}
	}
}

func streamPolymarketPreview(ctx context.Context, info workflow.WSTriggerInfo, dataCh chan<- []byte, errCh chan<- error) {
	defer close(dataCh)

	client, err := polymarket.New(polymarket.ClientConfig{})
	if err != nil {
		errCh <- fmt.Errorf("init polymarket client: %w", err)
		return
	}
	defer client.Close() //nolint:errcheck

	tradeCh, err := client.WatchTrades(ctx, info.AssetIDs)
	if err != nil {
		errCh <- fmt.Errorf("subscribe trades: %w", err)
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-tradeCh:
			if !ok {
				errCh <- fmt.Errorf("polymarket trade channel closed")
				return
			}
			data, err := json.Marshal(workflow.PolymarketTradeInput(event))
			if err != nil {
				slog.Warn("ws-preview: marshal polymarket event", "err", err)
				continue
			}
			select {
			case dataCh <- data:
			case <-ctx.Done():
				return
			}
		}
	}
}

func streamSchwabPreview(ctx context.Context, info workflow.WSTriggerInfo, connStore ConnectionStore, db *mongo.Database, dataCh chan<- []byte, errCh chan<- error) {
	defer close(dataCh)

	conn, err := connStore.GetByID(ctx, info.ConnectionID)
	if err != nil {
		errCh <- fmt.Errorf("load connection %s: %w", info.ConnectionID, err)
		return
	}

	tm := workflow.BuildSchwabTokenManager(conn.Config, db, conn.ID)
	if err := tm.Load(ctx); err != nil {
		errCh <- fmt.Errorf("load tokens: %w", err)
		return
	}
	if !tm.IsAuthorized() {
		errCh <- fmt.Errorf("connection %s not authorized — complete OAuth flow first", info.ConnectionID)
		return
	}

	refreshCtx, refreshCancel := context.WithCancel(ctx)
	defer refreshCancel()
	tm.RunRefresher(refreshCtx)

	client := schwab.NewClient(tm)
	defer client.Close() //nolint:errcheck

	switch info.StreamType {
	case "options":
		streamSchwabOptionsPreview(ctx, client, info, dataCh, errCh)
	case "futures":
		streamSchwabFuturesPreview(ctx, client, info, dataCh, errCh)
	default:
		errCh <- fmt.Errorf("unsupported schwab stream_type: %s", info.StreamType)
	}
}

func streamSchwabOptionsPreview(ctx context.Context, client *schwab.Client, info workflow.WSTriggerInfo, dataCh chan<- []byte, errCh chan<- error) {
	tradeCh, wsErrCh, err := client.StreamTradesEx(ctx, info.Symbols)
	if err != nil {
		errCh <- fmt.Errorf("stream options: %w", err)
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case wsErr := <-wsErrCh:
			errCh <- fmt.Errorf("options stream closed: %w", wsErr)
			return
		case trade, ok := <-tradeCh:
			if !ok {
				errCh <- fmt.Errorf("options trade channel closed")
				return
			}
			data, err := json.Marshal(workflow.SchwabOptionsTradeInput(trade))
			if err != nil {
				slog.Warn("ws-preview: marshal options trade", "err", err)
				continue
			}
			select {
			case dataCh <- data:
			case <-ctx.Done():
				return
			}
		}
	}
}

func streamSchwabFuturesPreview(ctx context.Context, client *schwab.Client, info workflow.WSTriggerInfo, dataCh chan<- []byte, errCh chan<- error) {
	tradeCh, wsErrCh, err := client.StreamFuturesEx(ctx, info.Symbols)
	if err != nil {
		errCh <- fmt.Errorf("stream futures: %w", err)
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case wsErr := <-wsErrCh:
			errCh <- fmt.Errorf("futures stream closed: %w", wsErr)
			return
		case trade, ok := <-tradeCh:
			if !ok {
				errCh <- fmt.Errorf("futures trade channel closed")
				return
			}
			data, err := json.Marshal(workflow.SchwabFuturesTradeInput(trade))
			if err != nil {
				slog.Warn("ws-preview: marshal futures trade", "err", err)
				continue
			}
			select {
			case dataCh <- data:
			case <-ctx.Done():
				return
			}
		}
	}
}
