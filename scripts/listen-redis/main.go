//go:build ignore

// Subscribe to a Redis pub/sub channel and print every message.
//
// Default channel = "weather.alerts" (matches the AI Weather workflow's
// publish_weather tool). Override with -channel or REDIS_CHANNEL.
//
// Usage:
//
//	go run ./scripts/listen-redis -channel weather.alerts
//	REDIS_CHANNEL=weather.alerts go run ./scripts/listen-redis
//
// Stops cleanly on Ctrl-C.

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/config"
	"github.com/bRRRITSCOLD/burrow/internal/rediss"
)

func main() {
	defaultChannel := os.Getenv("REDIS_CHANNEL")
	if defaultChannel == "" {
		defaultChannel = "weather.alerts"
	}
	channel := flag.String("channel", defaultChannel, "Redis pub/sub channel to subscribe to")
	flag.Parse()

	cfg, err := config.Load(config.WithDotEnv(".env"))
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}

	rc := rediss.New(cfg.Redis)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	sub := rc.Subscribe(ctx, *channel)
	defer sub.Close() //nolint:errcheck

	// Wait for the subscription to be ready before announcing.
	pingCtx, pingCancel := context.WithTimeout(ctx, 5*time.Second)
	defer pingCancel()
	if _, err := sub.Receive(pingCtx); err != nil {
		slog.Error("subscribe", "channel", *channel, "addr", fmt.Sprintf("%s:%d", cfg.Redis.Host, cfg.Redis.Port), "err", err)
		os.Exit(1)
	}

	fmt.Printf("listening on %s:%d → channel %q (Ctrl-C to stop)\n",
		cfg.Redis.Host, cfg.Redis.Port, *channel)

	ch := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			fmt.Println("\nshutting down")
			return
		case msg, ok := <-ch:
			if !ok {
				slog.Warn("channel closed by server")
				return
			}
			fmt.Printf("[%s] %s\n", time.Now().Format(time.RFC3339), msg.Payload)
		}
	}
}
