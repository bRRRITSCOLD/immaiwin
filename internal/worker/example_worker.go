package worker

import (
	"context"
	"log/slog"
	"time"
)

// ExampleWorker is a template — copy this file to add a new worker.
var ExampleWorker = &exampleWorker{}

type exampleWorker struct{}

func (w *exampleWorker) Name() string { return "example" }

func (w *exampleWorker) Run(ctx context.Context) error {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("example worker stopped")
			return nil
		case t := <-ticker.C:
			slog.Info("example worker tick", "time", t)
		}
	}
}
