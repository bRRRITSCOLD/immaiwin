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
	"github.com/bRRRITSCOLD/immaiwin-go/internal/api/handler"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/config"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/mongodb"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/polymarket"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/rediss"
	_ "github.com/bRRRITSCOLD/immaiwin-go/internal/llm/anthropic" // register Anthropic provider in llm.Default
	_ "github.com/bRRRITSCOLD/immaiwin-go/internal/llm/ollama"    // register Ollama provider
	_ "github.com/bRRRITSCOLD/immaiwin-go/internal/llm/openai"    // register OpenAI provider
	"github.com/bRRRITSCOLD/immaiwin-go/internal/sandbox"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/sandbox/docker"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/sandbox/k3s"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/schwab"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/skills"
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

	defaultDB := mongodb.NewMongoClient(mc.DB())
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
	// runRepo doubles as workflow.WorkflowRunStore for the executor + the
	// /runs history handlers. On init failure we keep the runStore as a
	// nil interface (NOT typed-nil) so downstream `store == nil` guards
	// actually fire.
	var runStore workflow.WorkflowRunStore
	runRepo, err := mongodb.NewWorkflowRunRepository(ctx, mc.DB())
	if err != nil {
		slog.Warn("workflow run repo init failed (agent traces will not persist)", "err", err)
		runRepo = nil
	} else {
		runStore = runRepo
	}

	// Skill resolver. Off by default; opt-in via SKILLS_ENABLED. When enabled
	// we wire the Mongo-backed registry as the index and a LocalFS source
	// (default `/var/lib/immaiwin/skills`) as the bundle storage. Failures
	// degrade the skill feature without taking down the rest of the API.
	var (
		skillRes     *skills.Resolver
		skillBackend *handler.SkillBackend
	)
	if cfg.Skills.Enabled {
		registry, regErr := mongodb.NewSkillRegistry(ctx, mc.DB())
		if regErr != nil {
			slog.Warn("skill registry init failed (skills disabled)", "err", regErr)
		} else {
			fsSrc := skills.NewLocalFSSource(cfg.Skills.Dir, "local-fs")
			skillRes = skills.NewResolver(registry, fsSrc)
			skillBackend = &handler.SkillBackend{
				Registry: registry,
				Sources:  []skills.Source{fsSrc},
			}
			slog.Info("skills system enabled", "dir", cfg.Skills.Dir)
		}
	}

	wfExec := &workflow.WorkflowExecutor{
		HTTPClient:     &http.Client{Timeout: 30 * time.Second},
		DB:             defaultDB,
		Redis:          rc,
		ConnResolver:   connResolver,
		SandboxRT:      sandboxRT,
		ApprovalBroker: rc,
		Memory:         chatMem,
		RunRepo:      runStore,
		SkillRes:     skillRes,
	}

	// Eval harness (Tier C). Best-effort init — the rest of the API stays
	// up if Mongo can't create the eval indexes for any reason.
	var evalDeps handler.EvalDeps
	if evalRepo, eerr := mongodb.NewEvalRepository(ctx, mc.DB()); eerr != nil {
		slog.Warn("eval repo init failed (evals disabled)", "err", eerr)
	} else {
		evalDeps = handler.EvalDeps{
			Store: evalRepo,
			Runner: &workflow.EvalRunner{
				Evals:     evalRepo,
				Workflows: wfRepo,
				Executor:  wfExec,
			},
		}
	}

	// User + tenant repos for auth + multi-tenancy. Best-effort init
	// — failures degrade auth without taking down the rest of the API.
	userRepo, uerr := mongodb.NewUserRepository(ctx, mc.DB())
	if uerr != nil {
		slog.Warn("user repo init failed (auth disabled)", "err", uerr)
		userRepo = nil
	}
	tenantRepo, terr := mongodb.NewTenantRepository(ctx, mc.DB())
	if terr != nil {
		slog.Warn("tenant repo init failed (multi-tenancy disabled)", "err", terr)
		tenantRepo = nil
	}
	apiKeyRepo, kerr := mongodb.NewAPIKeyRepository(ctx, mc.DB())
	if kerr != nil {
		slog.Warn("api key repo init failed (programmatic auth disabled)", "err", kerr)
		apiKeyRepo = nil
	}
	workerHealthRepo, herr := mongodb.NewWorkerHealthRepository(ctx, mc.DB())
	if herr != nil {
		slog.Warn("worker health repo init failed (worker observability disabled)", "err", herr)
		workerHealthRepo = nil
	}
	if cfg.Auth.JWTSecret == "" {
		slog.Warn("AUTH_JWT_SECRET not configured — auth endpoints will refuse requests; set a 32+ byte hex value in .env to enable")
	}

	srv := api.NewServer(cfg.API, cfg.Auth, rc, pm, wl, tr, nr, tokens, owl, fwl, sc, wfRepo, runStore, wfExec, connRepo, connResolver, skillBackend, evalDeps, userRepo, tenantRepo, apiKeyRepo, workerHealthRepo, mc.DB(), sandboxRT)

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
