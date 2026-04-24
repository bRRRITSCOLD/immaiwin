package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/api"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/config"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/mongodb"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/polymarket"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/rediss"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	rc := rediss.New(cfg.Redis)
	defer func() {
		if err := rc.Close(); err != nil {
			slog.Error("failed to close redis client", "err", err)
		}
	}()

	pm, err := polymarket.New(polymarket.ClientConfig{})
	if err != nil {
		slog.Error("failed to create polymarket client", "err", err)
		os.Exit(1)
	}
	defer func() {
		if err := pm.Close(); err != nil {
			slog.Error("failed to close polymarket client", "err", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mc, err := mongodb.New(ctx, cfg.MongoDB)
	if err != nil {
		slog.Error("failed to connect to mongodb", "err", err)
		os.Exit(1)
	}
	defer func() {
		if err := mc.Disconnect(ctx); err != nil {
			slog.Error("failed to disconnect mongodb", "err", err)
		}
	}()

	wl := mongodb.NewWatchlistRepository(mc.DB())
	tr := mongodb.NewTradeRepository(mc.DB())
	nr, err := mongodb.NewNewsRepository(ctx, mc.DB())
	if err != nil {
		slog.Error("failed to init news repository", "err", err)
		os.Exit(1)
	}

	srv := api.NewServer(cfg.API, rc, pm, wl, tr, nr)

	go func() {
		slog.Info("api server listening", "addr", srv.Addr())
		if err := srv.Start(ctx); !errors.Is(err, http.ErrServerClosed) {
			slog.Error("api server error", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down api server")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("api server shutdown error", "err", err)
		os.Exit(1)
	}
}
