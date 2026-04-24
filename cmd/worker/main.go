package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/config"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/worker"
)

func main() {
	name := flag.String("name", "", "worker name to run (see -list)")
	list := flag.Bool("list", false, "print available worker names and exit")
	flag.Parse()

	wr := worker.NewWorkerRegistry()

	wr.RegisterWorker(worker.MongoDBWriterWorker)
	wr.RegisterWorker(worker.PolymarketWatcherWorker)
	wr.RegisterWorker(worker.AljazeeraScraperWorker)
	wr.RegisterWorker(worker.BloombergRSSWorker)

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

	slog.Info("running worker", "name", *name, "concurrency", cfg.Worker.Concurrency)

	if err := wr.StartWorker(ctx, *name, cfg.Worker.Concurrency); err != nil {
		slog.Error("worker exited with error", "name", *name, "err", err)
		os.Exit(1)
	}
}
