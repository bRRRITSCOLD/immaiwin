package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	_ "github.com/bRRRITSCOLD/burrow/internal/llm/anthropic" // register Anthropic provider in llm.Default
	_ "github.com/bRRRITSCOLD/burrow/internal/llm/ollama"    // register Ollama provider
	_ "github.com/bRRRITSCOLD/burrow/internal/llm/openai"    // register OpenAI provider

	"github.com/bRRRITSCOLD/burrow/internal/config"
	"github.com/bRRRITSCOLD/burrow/internal/mongodb"
	"github.com/bRRRITSCOLD/burrow/internal/worker"
)

func main() {
	name := flag.String("name", "", "worker name to run (see -list)")
	list := flag.Bool("list", false, "print available worker names and exit")
	flag.Parse()

	wr := worker.NewWorkerRegistry()

	wr.RegisterWorker(worker.WorkflowCronWorker)
	wr.RegisterWorker(worker.WorkflowRabbitMQWorker)
	wr.RegisterWorker(worker.WorkflowRedisSubscribeWorker)
	wr.RegisterWorker(worker.ReaperWorker)

	if *list {
		slog.Info("available workers", "names", strings.Join(wr.RegisteredWorkerNames(), ", "))
		return
	}

	if *name == "" {
		slog.Error("flag -name is required")
		flag.Usage()
		os.Exit(1)
	}

	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Wire heartbeat repo if Mongo is reachable. Best-effort — if the
	// connection fails, the worker still runs but won't surface in
	// /api/v1/workers/health. Surfaces a warn log so the operator
	// knows observability is degraded.
	if mc, err := mongodb.New(ctx, cfg.MongoDB); err == nil {
		defer func() {
			if err := mc.Disconnect(context.Background()); err != nil {
				slog.Error("disconnect mongodb (health)", "err", err)
			}
		}()
		if hr, err := mongodb.NewWorkerHealthRepository(ctx, mc.DB()); err == nil {
			wr = wr.WithHealth(hr)
		} else {
			slog.Warn("worker: health repo init failed (continuing without observability)", "err", err)
		}
	} else {
		slog.Warn("worker: mongo connect failed for health repo (continuing without observability)", "err", err)
	}

	slog.Info("running worker", "name", *name, "concurrency", cfg.Worker.Concurrency)

	if err := wr.StartWorker(ctx, *name, cfg.Worker.Concurrency); err != nil {
		slog.Error("worker exited with error", "name", *name, "err", err)
		os.Exit(1)
	}
}
