package worker

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/mongodb"
)

// registry maps worker names to their implementations.
// Add a new entry here to register a worker.
// var registry = map[string]Worker{
// 	jobs.ExampleWorker.Name():           jobs.ExampleWorker,
// 	jobs.MongoDBWriterWorker.Name():     jobs.MongoDBWriterWorker,
// 	jobs.PolymarketWatcherWorker.Name(): jobs.PolymarketWatcherWorker,
// }

type WorkerRegistry struct {
	registry map[string]Worker
	// health is optional — when set, the registry records start /
	// heartbeat / stop / error events to this repo so /api/v1/workers/
	// health can surface live worker state. nil → no observability
	// (registry stays usable in tooling that doesn't have a Mongo
	// connection).
	health *mongodb.WorkerHealthRepository
}

func NewWorkerRegistry() *WorkerRegistry {
	return &WorkerRegistry{registry: map[string]Worker{}}
}

// WithHealth enables heartbeat persistence. Pass nil to disable
// (default). Wired from cmd/worker/main.go after Mongo is up.
func (wr *WorkerRegistry) WithHealth(h *mongodb.WorkerHealthRepository) *WorkerRegistry {
	wr.health = h
	return wr
}

// healthBeatInterval is how often the registry pings worker_health
// while a worker's Run is live. Decoupled from the worker's internal
// tick — a worker that ticks every 5s shouldn't write to mongo every
// 5s; a worker that blocks for 10min on a long task still wants its
// "alive" signal updating.
const healthBeatInterval = 30 * time.Second

// Register adds a worker to the registry. Panics on duplicate name.
func (wr *WorkerRegistry) RegisterWorker(w Worker) {
	if _, exists := wr.registry[w.Name()]; exists {
		slog.Debug("worker already registered", "name", w.Name())
	} else {
		wr.registry[w.Name()] = w

		slog.Debug("worker registered", "name", w.Name())
	}

}

// Names returns a sorted list of all registered worker names.
func (wr *WorkerRegistry) RegisteredWorkerNames() []string {
	names := make([]string, 0, len(wr.registry))
	for k := range wr.registry {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func (wr *WorkerRegistry) StartWorker(ctx context.Context, name string, concurrency int) error {
	w, ok := wr.registry[name]
	if !ok {
		return fmt.Errorf("unknown worker %q — registered workers: %v", name, wr.RegisteredWorkerNames())
	}

	if concurrency < 1 {
		concurrency = 1
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errc := make(chan error, concurrency)

	var wg sync.WaitGroup

	// Health: record one start row per worker name (not per instance —
	// if N instances run, they share the row; tick_count then tracks
	// aggregate beats). Background heartbeat goroutine pings every
	// healthBeatInterval so dashboards can detect a stuck Run that
	// never writes its own logs.
	if wr.health != nil {
		if err := wr.health.MarkStarted(ctx, name); err != nil {
			slog.Warn("worker: health MarkStarted failed (non-fatal)", "name", name, "err", err)
		}
		hbCtx, hbCancel := context.WithCancel(ctx)
		defer hbCancel()
		go func() {
			tick := time.NewTicker(healthBeatInterval)
			defer tick.Stop()
			for {
				select {
				case <-hbCtx.Done():
					return
				case <-tick.C:
					// Use a short deadline so a hung Mongo doesn't
					// block the goroutine forever — the heartbeat is
					// best-effort visibility, not a critical path.
					beatCtx, beatCancel := context.WithTimeout(hbCtx, 5*time.Second)
					if err := wr.health.Beat(beatCtx, name); err != nil {
						slog.Warn("worker: health Beat failed (non-fatal)", "name", name, "err", err)
					}
					beatCancel()
				}
			}
		}()
	}

	for i := range concurrency {
		wg.Go(func() {
			slog.Info("worker instance starting", "name", name, "instance", i)
			if err := w.Run(ctx); err != nil {
				errc <- err
				cancel()
			}
		})
	}

	wg.Wait()
	close(errc)

	runErr := <-errc // first error, or nil when all exited cleanly

	// Health: distinguish clean stop vs fatal error so dashboards can
	// flag flapping workers (errored → restarted → errored loop).
	if wr.health != nil {
		// Use a fresh context — the ctx passed in is cancelled by now
		// and would reject the write.
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if runErr != nil {
			if err := wr.health.RecordError(shutdownCtx, name, runErr); err != nil {
				slog.Warn("worker: health RecordError failed", "name", name, "err", err)
			}
		} else {
			if err := wr.health.MarkStopped(shutdownCtx, name); err != nil {
				slog.Warn("worker: health MarkStopped failed", "name", name, "err", err)
			}
		}
	}

	return runErr
}
