package worker

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
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
}

func NewWorkerRegistry() *WorkerRegistry {
	return &WorkerRegistry{registry: map[string]Worker{}}
}

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

	return <-errc // first error, or nil when all exited cleanly
}
