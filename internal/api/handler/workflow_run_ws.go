// Workflow run streaming over WebSocket.
//
// Protocol (browser ↔ this handler):
//
//	→ {"type": "run", "input": <any>, "stop_at": "<id>" | ["id1","id2"]}
//	→ {"type": "continue"}                                            # release pre-exec breakpoint
//	→ {"type": "approve_tool", "tool_call_id": "...", "approved": true|false, "reason": "..."}
//	→ {"type": "set_breakpoints", "node_ids": ["id1","id2"]}          # mid-run breakpoint update
//	← {"type": "step_start", "node_id": "...", "node_type": "..."}
//	← {"type": "step_done",  "node_id": "...", "output": <any>, "error": "...", "is_error": true}
//	← {"type": "agent_iter",          "node_id": "<agent-node>", "iter": 0}
//	← {"type": "agent_llm",           "node_id": "...", "iter": 0, "text": "...", "usage": {...}}
//	← {"type": "agent_tool_call",     "node_id": "...", "iter": 0, "tool_name": "...", "tool_id": "...", "tool_args": {...}}
//	← {"type": "agent_tool_approval", "node_id": "...", "iter": 0, "tool_name": "...", "tool_id": "...", "tool_args": {...}}
//	← {"type": "agent_tool_result",   "node_id": "...", "iter": 0, "tool_name": "...", "tool_id": "...", "result": "...", "is_error": false}
//	← {"type": "agent_final",         "node_id": "...", "iter": 0, "text": "..."}
//	← {"type": "run_done"}
//	← {"type": "error", "error": "..."}
//
// The first frame from the browser MUST be {"type": "run"}. Closing the
// socket cancels the run via context propagation.

package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/workflow"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/oklog/ulid/v2"
)

// runWfWsRequest is the only client→server message we currently accept.
// `stop_at` accepts either a single node ID string (legacy) or an array
// of node IDs (multi-breakpoint). Both forms route through the same
// executor breakpoint set.
type runWfWsRequest struct {
	Type        string          `json:"type"`
	Input       any             `json:"input,omitempty"`
	StopAt      json.RawMessage `json:"stop_at,omitempty"`
	ResumeRunID string          `json:"resume_run_id,omitempty"`
}

// runWfWsFrame is the server→client envelope. Plain alias of
// workflow.RunEvent — earlier this carried an OUTER `Error` field which
// shadowed the embedded `RunEvent.Error` (same `json:"error"` tag, outer
// wins) and silently stripped error messages off every step_done/error
// event, leaving the UI's red-error pane empty. Pre-run failures that
// don't yet have a RunEvent context still fit by setting
// RunEvent.Type="error" + RunEvent.Error in writeWfWsError.
type runWfWsFrame struct {
	workflow.RunEvent
}

// wsEventEmitter relays workflow.RunEvents over a WebSocket. Implements
// workflow.EventEmitter. Single mutex per connection serialises writes
// (gorilla websocket connections are not safe for concurrent writers).
type wsEventEmitter struct {
	ws *websocket.Conn
	mu sync.Mutex
}

func (e *wsEventEmitter) Emit(ev workflow.RunEvent) {
	frame := runWfWsFrame{RunEvent: ev}
	data, err := json.Marshal(frame)
	if err != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = e.ws.WriteMessage(websocket.TextMessage, data)
}

// ping writes a control PingMessage through the same mutex the
// event emitter uses. gorilla/websocket is not safe for concurrent
// writers; idle keepalive shares the lock with normal frames.
// Returns true on success, false on write failure (caller exits
// the read/forward loop on failure since the socket is dead).
func (e *wsEventEmitter) ping() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = e.ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
	defer func() { _ = e.ws.SetWriteDeadline(time.Time{}) }()
	return e.ws.WriteMessage(websocket.PingMessage, nil) == nil
}

// writeWfWsError is a one-off error frame writer for failures that
// happen before the run starts (bad upgrade, missing workflow, etc.).
func writeWfWsError(ws *websocket.Conn, msg string) {
	frame := runWfWsFrame{RunEvent: workflow.RunEvent{Type: "error", Error: msg}}
	data, err := json.Marshal(frame)
	if err != nil {
		return
	}
	wsMu.Lock()
	defer wsMu.Unlock()
	_ = ws.WriteMessage(websocket.TextMessage, data)
}

// RunWorkflowWS upgrades to WebSocket and streams a workflow run's events
// to the browser.
//
// As of the canvas-WS / lease unification: this handler does NOT execute
// the workflow itself. It dispatches via the lease path (persists a run
// record + publishes a wakeup) and subscribes to the per-run Redis event
// channel that the worker publishes to. Browser frames that used to
// drive the run inline (`continue`, `approve_tool`, `set_breakpoints`)
// are no-ops here — they belong on REST endpoints + a control-channel
// bridge that the worker listens on. For now, browsers should:
//   - Approve a tool / node gate via `POST /api/v1/workflow_runs/:id/approval`
//     (same endpoint the `/runs/:id` page uses).
//   - Continue / set_breakpoints via TODO REST endpoints (Phase 2).
//
// Closing the socket cancels the WS context but does NOT cancel the
// run — the worker holds the lease and finishes regardless. That's the
// whole point of durable execution.
//
// resume_run_id is intentionally rejected here for now: the legacy
// stopAt-pause resume flow used in-process channels, and migrating it
// to lease is its own change (PR-3.4-ish). Use Replay on /runs/:id
// instead.
func RunWorkflowWS(store WorkflowStore, runStore workflow.WorkflowRunStore, exec *workflow.WorkflowExecutor) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if id == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
			return
		}

		ws, err := debugUpgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			slog.Error("workflow-run-ws: upgrade failed", "err", err)
			return
		}
		defer func() { _ = ws.Close() }()

		// Wait for "run" message — first frame must be a run kickoff.
		_, raw, err := ws.ReadMessage()
		if err != nil {
			return
		}
		var req runWfWsRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeWfWsError(ws, "invalid JSON")
			return
		}
		if req.Type != "run" {
			writeWfWsError(ws, "first message must be 'run'")
			return
		}
		if req.ResumeRunID != "" {
			writeWfWsError(ws, "resume_run_id over WS is no longer supported — use Replay on /runs/:id (lease path)")
			return
		}

		runCtx, cancel := context.WithCancel(c.Request.Context())
		defer cancel()

		// Drain incoming frames so a closed socket cancels the WS ctx.
		// We don't act on `continue` / `approve_tool` / `set_breakpoints`
		// here any more — those are control signals that need to reach
		// the worker, not this handler. Logged at info so a misbehaving
		// front-end is visible without breaking the stream.
		go func() {
			for {
				_, raw, err := ws.ReadMessage()
				if err != nil {
					cancel()
					return
				}
				var head struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(raw, &head) != nil {
					continue
				}
				if head.Type == "continue" || head.Type == "approve_tool" || head.Type == "set_breakpoints" {
					// Legacy control frame from a UI build that predates
					// the canvas-WS / lease unification. Drop it
					// silently in production; a debug-level note keeps
					// the breadcrumb without spamming logs.
					slog.Debug("ws: ignoring legacy control frame; route via REST instead",
						"type", head.Type)
				}
			}
		}()

		wf, err := store.GetByID(runCtx, id)
		if err != nil {
			writeWfWsError(ws, err.Error())
			return
		}

		if runStore == nil || exec == nil || exec.ApprovalBroker == nil {
			writeWfWsError(ws, "run dispatch not configured (run store or approval broker missing)")
			return
		}

		// Allocate the run id, persist the record, kick the worker via
		// wakeup. The worker will claim the lease, run the BFS, and
		// publish RunEvents on `burrow:run_events:<runID>` — this
		// handler subscribes below and forwards each event to the
		// browser.
		runID := ulid.Make().String()
		// Decode stop_at: accepts a single string id (legacy) or a
		// list (canvas multi-breakpoint). Stamped onto the run record
		// so the worker's RunFromCheckpoint seeds the executor's
		// stopAtSet on first claim — the async dispatch path was
		// otherwise stop_at-deaf, so canvas Debug ↓ runs would skip
		// every breakpoint that wasn't the very first node.
		var initialBreakpoints []string
		if len(req.StopAt) > 0 {
			var single string
			if err := json.Unmarshal(req.StopAt, &single); err == nil && single != "" {
				initialBreakpoints = []string{single}
			} else {
				var multi []string
				if err := json.Unmarshal(req.StopAt, &multi); err == nil {
					for _, id := range multi {
						if id != "" {
							initialBreakpoints = append(initialBreakpoints, id)
						}
					}
				}
			}
		}
		rec := workflow.WorkflowRun{
			ID:         runID,
			WorkflowID: wf.ID,
			TenantID:   wf.TenantID,
			QueuedAt:   time.Now().UTC(),
			// Same dispatch contract as POST /run: queued until a
			// worker claims the lease + flips status=running atomically.
			// StartedAt stays nil until ClaimLease stamps it.
			Status:             workflow.RunStatusQueued,
			Config: wf.Config,
			TriggerInput:       req.Input,
			InitialBreakpoints: initialBreakpoints,
		}
		if _, cerr := runStore.Create(runCtx, rec); cerr != nil {
			writeWfWsError(ws, "create run: "+cerr.Error())
			return
		}
		if _, perr := exec.ApprovalBroker.PublishWithCount(runCtx, workflow.WakeupChannel, []byte("1")); perr != nil {
			slog.Warn("ws: wakeup publish failed", "run_id", runID, "err", perr)
		}
		slog.Info("ws: dispatched run via lease", "run_id", runID, "wf_id", wf.ID)

		emitter := &wsEventEmitter{ws: ws}
		// Send a synthetic run_started event so the UI knows which run
		// id to navigate / track. The worker emits RunEvents from
		// inside the BFS but doesn't include a "started" envelope; this
		// is the canvas's first cue.
		emitter.Emit(workflow.RunEvent{
			Type:  workflow.EventRunStart,
			RunID: runID,
		})

		// Subscribe to the per-run event channel and pump frames to the
		// browser until either the run terminates (run_done event) or
		// the socket closes.
		sub := exec.ApprovalBroker.Subscribe(runCtx, workflow.RunEventChannel(runID))
		defer func() { _ = sub.Close() }()
		ch := sub.Channel()

		// Keepalive: send a WS ping every 20s so an idle pause
		// (debug breakpoint, long approval wait, slow LLM) doesn't
		// silently time the socket out at the browser / proxy
		// (most defaults are 30–60s of silence). Without this, the
		// connection drops mid-pause; the api's run_events sub
		// stays subscribed but its writes go to a dead socket and
		// fail, so the next worker reclaim's step_pending event
		// publishes into the void and the canvas never reflects
		// the resumed paused state.
		pingT := time.NewTicker(20 * time.Second)
		defer pingT.Stop()

		for {
			select {
			case <-runCtx.Done():
				return
			case <-pingT.C:
				if !emitter.ping() {
					return
				}
			case msg, ok := <-ch:
				if !ok {
					return
				}
				// Forward raw payload — already JSON-encoded RunEvent.
				_ = ws.WriteMessage(websocket.TextMessage, []byte(msg.Payload))
				// Decode just enough to detect the terminal event.
				var ev workflow.RunEvent
				if err := json.Unmarshal([]byte(msg.Payload), &ev); err == nil {
					if ev.Type == workflow.EventRunDone {
						return
					}
				}
			}
		}
	}
}
