package handler

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/news"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/rediss"
	"github.com/gin-gonic/gin"
)

// NewsLister fetches recent news articles from MongoDB.
type NewsLister interface {
	List(ctx context.Context, since time.Time, limit int) ([]news.Article, error)
}

// GetNews returns articles scraped in the last N days (default 3).
func GetNews(store NewsLister) gin.HandlerFunc {
	return func(c *gin.Context) {
		days := 3
		since := time.Now().UTC().AddDate(0, 0, -days)
		articles, err := store.List(c.Request.Context(), since, 500)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if articles == nil {
			articles = []news.Article{}
		}
		c.JSON(http.StatusOK, articles)
	}
}

func StreamNews(b *rediss.Broadcaster) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("Access-Control-Allow-Origin", "*")

		ch := b.Subscribe()
		defer b.Unsubscribe(ch)

		slog.Info("sse news client connected", "remote", c.Request.RemoteAddr)

		fmt.Fprintf(c.Writer, ": connected\n\n")
		c.Writer.Flush()

		keepalive := time.NewTicker(15 * time.Second)
		defer keepalive.Stop()

		ctx := c.Request.Context()
		for {
			select {
			case <-ctx.Done():
				slog.Info("sse news client disconnected", "remote", c.Request.RemoteAddr)
				return
			case <-keepalive.C:
				fmt.Fprintf(c.Writer, ": keepalive\n\n")
				c.Writer.Flush()
			case data, ok := <-ch:
				if !ok {
					return
				}
				fmt.Fprintf(c.Writer, "event: article\ndata: %s\n\n", data)
				c.Writer.Flush()
			}
		}
	}
}
