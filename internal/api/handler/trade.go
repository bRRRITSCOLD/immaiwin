package handler

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/rediss"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/trade"
	"github.com/gin-gonic/gin"
)

// TradesLister fetches recent persisted trades from MongoDB.
type TradesLister interface {
	List(ctx context.Context, limit int) ([]trade.Trade, error)
}

type tradesQuery struct {
	Limit int `form:"limit"`
}

// GetTrades returns recent persisted (unusual) trades sorted newest-first.
func GetTrades(store TradesLister) gin.HandlerFunc {
	return func(c *gin.Context) {
		var q tradesQuery
		if err := c.ShouldBindQuery(&q); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		limit := q.Limit
		if limit <= 0 || limit > 500 {
			limit = 200
		}
		trades, err := store.List(c.Request.Context(), limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if trades == nil {
			trades = []trade.Trade{}
		}
		c.JSON(http.StatusOK, trades)
	}
}

func StreamTrades(store TradesLister, b *rediss.Broadcaster) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("Access-Control-Allow-Origin", "*")

		ch := b.Subscribe()
		defer b.Unsubscribe(ch)

		slog.Info("sse client connected", "remote", c.Request.RemoteAddr)

		// Flush headers immediately so EventSource.onopen fires in the browser.
		fmt.Fprintf(c.Writer, ": connected\n\n")
		c.Writer.Flush()

		keepalive := time.NewTicker(15 * time.Second)
		defer keepalive.Stop()

		ctx := c.Request.Context()
		for {
			select {
			case <-ctx.Done():
				slog.Info("sse client disconnected", "remote", c.Request.RemoteAddr)
				return
			case <-keepalive.C:
				fmt.Fprintf(c.Writer, ": keepalive\n\n")
				c.Writer.Flush()
			case data, ok := <-ch:
				if !ok {
					return
				}
				fmt.Fprintf(c.Writer, "event: trade\ndata: %s\n\n", data)
				c.Writer.Flush()
			}
		}
	}
}
