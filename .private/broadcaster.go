package api

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	redisclient "github.com/bRRRITSCOLD/immaiwin-go/internal/redis"
	"github.com/gin-gonic/gin"
)

type broadcaster struct {
	redis   *redisclient.Client
	mu      sync.RWMutex
	clients map[chan []byte]struct{}
}

func newBroadcaster(rc *redisclient.Client) *broadcaster {
	return &broadcaster{
		redis:   rc,
		clients: make(map[chan []byte]struct{}),
	}
}

func (b *broadcaster) subscribe() chan []byte {
	ch := make(chan []byte, 16)
	b.mu.Lock()
	b.clients[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *broadcaster) unsubscribe(ch chan []byte) {
	b.mu.Lock()
	delete(b.clients, ch)
	b.mu.Unlock()
	close(ch)
}

func (b *broadcaster) run(ctx context.Context) {
	sub := b.redis.Subscribe(ctx, redisclient.TradesChannel)
	defer sub.Close()
	msgs := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-msgs:
			if !ok {
				return
			}
			b.fanOut([]byte(msg.Payload))
		}
	}
}

func (b *broadcaster) fanOut(data []byte) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.clients {
		select {
		case ch <- data:
		default:
			// slow client — drop rather than block
		}
	}
}

func tradesStreamHandler(b *broadcaster) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("Access-Control-Allow-Origin", "*")

		ch := b.subscribe()
		defer b.unsubscribe(ch)

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
