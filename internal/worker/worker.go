package worker

import (
	"context"
)

// Worker is the interface every background worker must implement.
type Worker interface {
	// Name returns the unique identifier used to select this worker at startup.
	Name() string
	// Run starts one instance of the worker and blocks until ctx is cancelled
	// or a fatal error occurs.
	Run(ctx context.Context) error
}
