package handler

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/options"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/rediss"
	"github.com/gin-gonic/gin"
)

// OptionsWatchlistStore persists the options underlying watchlist.
type OptionsWatchlistStore interface {
	List(ctx context.Context) ([]options.WatchlistItem, error)
	Sync(ctx context.Context, symbols []string) error
}

// GetOptionsWatchlist returns the list of watched underlying symbols.
func GetOptionsWatchlist(store OptionsWatchlistStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		items, err := store.List(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if items == nil {
			items = []options.WatchlistItem{}
		}
		c.JSON(http.StatusOK, items)
	}
}

type syncOptionsRequest struct {
	Symbols []string `json:"symbols" binding:"required"`
}

// SyncOptionsWatchlist replaces the watchlist with the provided symbols.
func SyncOptionsWatchlist(store OptionsWatchlistStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req syncOptionsRequest
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

// StreamOptions streams unusual options events via SSE.
func StreamOptions(b *rediss.Broadcaster) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("Access-Control-Allow-Origin", "*")

		ch := b.Subscribe()
		defer b.Unsubscribe(ch)

		slog.Info("options sse client connected", "remote", c.Request.RemoteAddr)

		fmt.Fprintf(c.Writer, ": connected\n\n")
		c.Writer.Flush()

		keepalive := time.NewTicker(15 * time.Second)
		defer keepalive.Stop()

		ctx := c.Request.Context()
		for {
			select {
			case <-ctx.Done():
				slog.Info("options sse client disconnected", "remote", c.Request.RemoteAddr)
				return
			case <-keepalive.C:
				fmt.Fprintf(c.Writer, ": keepalive\n\n")
				c.Writer.Flush()
			case data, ok := <-ch:
				if !ok {
					return
				}
				fmt.Fprintf(c.Writer, "event: option\ndata: %s\n\n", data)
				c.Writer.Flush()
			}
		}
	}
}
