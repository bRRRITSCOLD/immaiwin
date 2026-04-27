package main

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"os/signal"
	"syscall"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/api"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/config"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/mongodb"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/polymarket"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/rediss"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/sandbox"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/schwab"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/workflow"
)

func main() {
	cfg, err := config.Load(config.WithDotEnv(".env"))
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

	tokens := schwab.NewTokenManager(cfg.Schwab, mc.DB())
	if err := tokens.Load(ctx); err != nil {
		slog.Warn("schwab tokens not loaded (visit /auth/schwab to authorize)", "err", err)
	}
	tokens.RunRefresher(ctx)

	owl := mongodb.NewOptionsWatchlistRepository(mc.DB())
	fwl := mongodb.NewFuturesWatchlistRepository(mc.DB())
	sc, err := mongodb.NewScraperConfigRepository(ctx, mc.DB())
	if err != nil {
		slog.Error("failed to init scraper config repository", "err", err)
		os.Exit(1)
	}

	wfRepo, err := mongodb.NewWorkflowRepository(ctx, mc.DB())
	if err != nil {
		slog.Error("failed to init workflow repository", "err", err)
		os.Exit(1)
	}

	var encKey []byte
	if trimmed := strings.TrimSpace(cfg.EncryptionKey); trimmed != "" {
		encKey, err = hex.DecodeString(trimmed)
		if err != nil || len(encKey) != 32 {
			slog.Error("ENCRYPTION_KEY must be 64 hex chars (32 bytes)", "len", len(trimmed), "err", err)
			os.Exit(1)
		}
	}

	connRepo, err := mongodb.NewConnectionRepository(ctx, mc.DB(), encKey)
	if err != nil {
		slog.Error("failed to init connection repository", "err", err)
		os.Exit(1)
	}

	defaultDB := mongodb.NewRawDB(mc.DB())
	connResolver := workflow.NewConnectionResolver(connRepo, defaultDB, rc)
	defer func() {
		if err := connResolver.Close(); err != nil {
			slog.Error("failed to close connection resolver", "err", err)
		}
	}()

	// Sandbox manager (optional — controlled by SANDBOX_ENABLED env)
	var sandboxMgr *sandbox.Manager
	if cfg.Sandbox.Enabled {
		opts := []sandbox.ManagerOption{
			sandbox.WithDebugPorts(19000, 19100),
		}
		if cfg.Sandbox.Runtime != "" {
			opts = append(opts, sandbox.WithRuntime(cfg.Sandbox.Runtime))
		}
		mgr, err := sandbox.NewManager(opts...)
		if err != nil {
			slog.Error("failed to create sandbox manager", "err", err)
			os.Exit(1)
		}

		// Remove orphan containers from previous crashed sessions
		mgr.CleanupOrphans(ctx)

		// Warm container pool for interpreted languages
		pool := sandbox.NewPool(mgr.DockerClient(), cfg.Sandbox.PoolSize, cfg.Sandbox.Runtime)
		pool.Warm(ctx, sandbox.LangJavaScript, sandbox.LangPython)
		mgr.SetPool(pool)

		defer pool.Close(context.Background())
		defer func() { _ = mgr.Close() }()
		sandboxMgr = mgr
		slog.Info("sandbox manager enabled", "runtime", cfg.Sandbox.Runtime, "pool_size", cfg.Sandbox.PoolSize)
	}

	wfExec := &workflow.WorkflowExecutor{
		HTTPClient:   &http.Client{Timeout: 30 * time.Second},
		DB:           defaultDB,
		Pub:          rc,
		ConnResolver: connResolver,
		SandboxMgr:   sandboxMgr,
	}

	srv := api.NewServer(cfg.API, rc, pm, wl, tr, nr, tokens, owl, fwl, sc, wfRepo, wfExec, connRepo, mc.DB(), sandboxMgr)

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
