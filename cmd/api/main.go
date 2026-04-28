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
	_ "github.com/bRRRITSCOLD/immaiwin-go/internal/llm/anthropic" // register Anthropic provider in llm.Default
	"github.com/bRRRITSCOLD/immaiwin-go/internal/sandbox"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/sandbox/docker"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/sandbox/k3s"
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

	// Sandbox runtime (optional — controlled by SANDBOX_ENABLED env)
	// Backend selection: SANDBOX_BACKEND in {docker, k3s}; default = docker.
	var sandboxRT sandbox.Runtime
	if cfg.Sandbox.Enabled {
		backend := cfg.Sandbox.Backend
		if backend == "" || backend == "auto" {
			backend = "docker"
		}
		switch backend {
		case "docker":
			rt, err := docker.New(docker.Options{
				Runtime:        cfg.Sandbox.Runtime,
				DebugPortStart: 19000,
				DebugPortEnd:   19100,
				Registry:       cfg.Sandbox.ImageRegistry,
			})
			if err != nil {
				slog.Error("failed to create docker sandbox runtime", "err", err)
				os.Exit(1)
			}
			rt.CleanupOrphans(ctx)

			pool := docker.NewPool(rt.DockerClient(), cfg.Sandbox.PoolSize, cfg.Sandbox.Runtime)
			pool.Warm(ctx, sandbox.LangJavaScript, sandbox.LangPython)
			rt.SetPool(pool)

			defer pool.Close(context.Background())
			defer func() { _ = rt.Close() }()
			sandboxRT = rt
			slog.Info("sandbox enabled", "backend", "docker", "runtime", cfg.Sandbox.Runtime, "pool_size", cfg.Sandbox.PoolSize)
		case "k3s":
			rt, err := k3s.New(k3s.Options{
				Kubeconfig:     cfg.Sandbox.Kubeconfig,
				Namespace:      cfg.Sandbox.Namespace,
				RuntimeClass:   cfg.Sandbox.RuntimeClass,
				Registry:       cfg.Sandbox.ImageRegistry,
				DebugPortStart: 19000,
				DebugPortEnd:   19100,
				CIDRs: k3s.CIDRs{
					Pod:       cfg.Sandbox.PodCIDR,
					Service:   cfg.Sandbox.ServiceCIDR,
					LinkLocal: cfg.Sandbox.LinkLocalCIDR,
				},
			})
			if err != nil {
				slog.Error("failed to create k3s sandbox runtime", "err", err)
				os.Exit(1)
			}
			rt.CleanupOrphans(ctx)
			defer func() { _ = rt.Close() }()
			sandboxRT = rt
			slog.Info("sandbox enabled", "backend", "k3s", "namespace", cfg.Sandbox.Namespace, "registry", cfg.Sandbox.ImageRegistry)
		default:
			slog.Error("unknown sandbox backend", "backend", backend)
			os.Exit(1)
		}
	}

	// Agent dependencies — chat memory + run persistence. Best-effort init;
	// failures degrade the agent feature but don't break the rest of the API.
	chatMem, err := mongodb.NewChatMemory(ctx, mc.DB())
	if err != nil {
		slog.Warn("agent chat memory init failed (agents will run without history)", "err", err)
		chatMem = nil
	}
	runRepo, err := mongodb.NewWorkflowRunRepository(ctx, mc.DB())
	if err != nil {
		slog.Warn("workflow run repo init failed (agent traces will not persist)", "err", err)
		runRepo = nil
	}

	wfExec := &workflow.WorkflowExecutor{
		HTTPClient:   &http.Client{Timeout: 30 * time.Second},
		DB:           defaultDB,
		Pub:          rc,
		ConnResolver: connResolver,
		SandboxRT:    sandboxRT,
		Memory:       chatMem,
		RunRepo:      runRepo,
	}

	srv := api.NewServer(cfg.API, rc, pm, wl, tr, nr, tokens, owl, fwl, sc, wfRepo, wfExec, connRepo, mc.DB(), sandboxRT)

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
