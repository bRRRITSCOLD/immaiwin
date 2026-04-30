package sandbox

import (
	"context"
	"errors"
)

// SandboxLabel is applied to all sandbox containers/pods for orphan cleanup.
const SandboxLabel = "burrow.sandbox"

// Runtime executes user code in an isolated environment.
// Implementations: docker (default) and k3s (Linux + gVisor).
type Runtime interface {
	Run(ctx context.Context, req RunRequest) (*RunResult, error)
	StreamRun(ctx context.Context, req RunRequest) (<-chan OutputEvent, error)

	StartDebug(ctx context.Context, req DebugRequest) (*DebugSession, error)
	StopDebug(sessionID string) error
	DebugPort(sessionID string) (int, error)
	GetDebugSession(sessionID string) (*DebugSession, bool)

	CleanupOrphans(ctx context.Context)
	Close() error
}

// ErrNotSupported is returned by backends that do not implement an operation.
var ErrNotSupported = errors.New("sandbox: operation not supported by this backend")
