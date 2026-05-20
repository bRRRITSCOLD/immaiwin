// Per-run event bus over Redis pub/sub.
//
// Workers run executions out-of-process; the canvas WS handler streams
// events to the browser. Without a cross-process bridge those two are
// blind to each other. This bus is the bridge:
//
//   - The worker wires `RedisRunEventEmitter` as the executor's
//     EventEmitter when running a run-pass. Each `Emit(ev)` is JSON-
//     marshalled and PUBLISHed on `burrow:run_events:<runID>`.
//   - The WS handler subscribes to the same channel and forwards each
//     event to the connected browser, decoded via the same RunEvent
//     shape the in-memory emitter used.
//
// The same channel covers control signals back the other way:
// `burrow:run_control:<runID>` carries the browser's "continue" and
// "set_breakpoints" frames. The worker's runEnv subscribes during
// hydration; ApprovalDecisions still ride the existing
// `burrow:approval:<runID>` channel via ApprovalRegistry.

package workflow

import (
	"context"
	"encoding/json"
	"log/slog"
)

// RunEventChannel is the per-run pub/sub topic for outbound RunEvents
// (worker → WS). One topic per run keeps fan-out narrow + lets the WS
// handler unsubscribe cleanly when the socket closes. Exported so the
// WS handler in another package can subscribe to the same name.
func RunEventChannel(runID string) string {
	return "burrow:run_events:" + runID
}

// RunControlChannel is the per-run pub/sub topic for inbound control
// signals (WS → worker). Carries continue / set_breakpoints frames.
// Exported for symmetry with RunEventChannel; not yet wired (Phase 2
// of the canvas-WS / lease unification).
func RunControlChannel(runID string) string {
	return "burrow:run_control:" + runID
}

// RedisRunEventEmitter wraps a Redis publisher into the EventEmitter
// interface so the executor can stay channel-agnostic. Drops the event
// silently on JSON marshal failure (logs at warn) — emitter contracts
// say Emit must not block, so we don't block on publish errors either.
//
// Pub uses ApprovalSubscriber rather than a publish-only interface so
// the worker can pass the same rediss.Client it already wires for
// ApprovalBroker — keeps connection plumbing flat.
type RedisRunEventEmitter struct {
	Pub   ApprovalSubscriber
	RunID string
}

// Emit publishes the event to the per-run Redis channel. Best-effort:
// publish errors are logged at warn but don't propagate (the emitter
// interface returns void by design).
func (e *RedisRunEventEmitter) Emit(ev RunEvent) {
	if e == nil || e.Pub == nil || e.RunID == "" {
		return
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		slog.Warn("run_event_bus: marshal failed", "run_id", e.RunID, "type", ev.Type, "err", err)
		return
	}
	if _, err := e.Pub.PublishWithCount(context.Background(), RunEventChannel(e.RunID), payload); err != nil {
		slog.Warn("run_event_bus: publish failed", "run_id", e.RunID, "type", ev.Type, "err", err)
	}
}

// RunControlMessage is what the WS handler publishes to the control
// channel when a browser frame arrives. The worker's runEnv subscriber
// decodes it and routes into the appropriate in-process channel.
type RunControlMessage struct {
	Type    string   `json:"type"`               // "continue" | "set_breakpoints"
	NodeIDs []string `json:"node_ids,omitempty"` // for set_breakpoints
}

// ControlChannels is the worker → executor handoff for canvas
// Continue + set_breakpoints control signals. The worker owns the
// Redis subscription on `burrow:run_control:<runID>` and routes
// decoded RunControlMessages into these chans; the executor's
// runEnv listens on the chans the same way it does on a legacy
// in-process WS-resumed run.
//
// Nil chans (zero value) disable the corresponding feature — a
// run without control chans behaves exactly like a pre-bridge
// lease run (post-exec breakpoint persistence; no live continue).
type ControlChannels struct {
	Continue    chan struct{} // pre-exec breakpoint release
	Breakpoints chan []string // live set_breakpoints updates
}

type controlChannelsCtxKey struct{}

// WithControlChannels stashes the worker-owned control chans on
// ctx so the executor's RunWithEvents hydration block can wire
// them onto runEnv without a signature change on RunFromCheckpoint.
func WithControlChannels(ctx context.Context, cc *ControlChannels) context.Context {
	if cc == nil {
		return ctx
	}
	return context.WithValue(ctx, controlChannelsCtxKey{}, cc)
}

// ControlChannelsFromCtx returns the worker-supplied control chans
// (if any). Executor uses these to hook runEnv.continueCh +
// runEnv.SetBreakpoints into the worker's Redis sub goroutine.
func ControlChannelsFromCtx(ctx context.Context) (*ControlChannels, bool) {
	cc, ok := ctx.Value(controlChannelsCtxKey{}).(*ControlChannels)
	return cc, ok && cc != nil
}
