package rediss

import (
	"context"
	"log/slog"
	"sync"
)

type Broadcaster struct {
	redis   *Client
	channel string
	mu      sync.RWMutex
	clients map[chan []byte]struct{}
}

func NewBroadcaster(rc *Client, channel string) *Broadcaster {
	return &Broadcaster{
		redis:   rc,
		channel: channel,
		clients: make(map[chan []byte]struct{}),
	}
}

func (b *Broadcaster) Subscribe() chan []byte {
	ch := make(chan []byte, 16)
	b.mu.Lock()
	b.clients[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *Broadcaster) Unsubscribe(ch chan []byte) {
	b.mu.Lock()
	delete(b.clients, ch)
	b.mu.Unlock()
	close(ch)
}

func (b *Broadcaster) Run(ctx context.Context) {
	sub := b.redis.Subscribe(ctx, b.channel)
	defer func() {
		if err := sub.Close(); err != nil {
			slog.Error("broadcaster: close sub", "channel", b.channel, "err", err)
		}
	}()
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

func (b *Broadcaster) fanOut(data []byte) {
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
