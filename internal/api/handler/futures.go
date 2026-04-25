package handler

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/futures"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/rediss"
	"github.com/gin-gonic/gin"
)

// FuturesWatchlistStore persists the futures root watchlist.
type FuturesWatchlistStore interface {
	List(ctx context.Context) ([]futures.WatchlistItem, error)
	Sync(ctx context.Context, symbols []string) error
}

// GetFuturesWatchlist returns the list of watched futures root symbols.
func GetFuturesWatchlist(store FuturesWatchlistStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		items, err := store.List(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if items == nil {
			items = []futures.WatchlistItem{}
		}
		c.JSON(http.StatusOK, items)
	}
}

type syncFuturesRequest struct {
	Symbols []string `json:"symbols" binding:"required"`
}

// SyncFuturesWatchlist replaces the watchlist with the provided root symbols.
func SyncFuturesWatchlist(store FuturesWatchlistStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req syncFuturesRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := store.Sync(c.Request.Context(), req.Symbols); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}

// StreamFutures streams futures trade events via SSE.
func StreamFutures(b *rediss.Broadcaster) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("Access-Control-Allow-Origin", "*")

		ch := b.Subscribe()
		defer b.Unsubscribe(ch)

		slog.Info("futures sse client connected", "remote", c.Request.RemoteAddr)

		fmt.Fprintf(c.Writer, ": connected\n\n")
		c.Writer.Flush()

		keepalive := time.NewTicker(15 * time.Second)
		defer keepalive.Stop()

		ctx := c.Request.Context()
		for {
			select {
			case <-ctx.Done():
				slog.Info("futures sse client disconnected", "remote", c.Request.RemoteAddr)
				return
			case <-keepalive.C:
				fmt.Fprintf(c.Writer, ": keepalive\n\n")
				c.Writer.Flush()
			case data, ok := <-ch:
				if !ok {
					return
				}
				fmt.Fprintf(c.Writer, "event: future\ndata: %s\n\n", data)
				c.Writer.Flush()
			}
		}
	}
}
