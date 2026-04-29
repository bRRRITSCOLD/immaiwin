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

	"github.com/bRRRITSCOLD/immaiwin-go/internal/workflow"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
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

// parseStopAt accepts either a JSON string or array of strings and
// returns a deduplicated []string. Empty/null returns nil.
func parseStopAt(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	// Try array first.
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		return dedupeNonEmpty(arr)
	}
	// Fall back to single string.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && s != "" {
		return []string{s}
	}
	return nil
}

func dedupeNonEmpty(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
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
// to the browser. Closing the socket cancels the in-flight run via
// context propagation; on the agent side that aborts any in-flight LLM
// call (loopCtx is derived from the request ctx).
func RunWorkflowWS(store WorkflowStore, exec *workflow.WorkflowExecutor) gin.HandlerFunc {
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

		// Cancel-on-disconnect: derive a child context that cancels when
		// either the request context ends OR the client closes the
		// socket. The same goroutine also relays additional client
		// frames into the run — `{type:"continue"}` releases a pre-exec
		// breakpoint pause via env.continueCh.
		runCtx, cancel := context.WithCancel(c.Request.Context())
		defer cancel()
		continueCh := make(chan struct{}, 4)
		// Buffered approveCh holds verdicts that arrive before the agent
		// loop reaches its waitForApproval read. 16 is generous — a single
		// agent iter is gated sequentially, so 1 would suffice; the slack
		// covers UI double-clicks + future parallel-tool dispatch.
		approveCh := make(chan workflow.ApprovalDecision, 16)
		// breakpointsCh lets the live UI mutate the run's breakpoint set
		// mid-run via `set_breakpoints` frames. Buffered to absorb burst
		// updates from rapid clicks; the executor's forwarder applies
		// the most recent list.
		breakpointsCh := make(chan []string, 8)
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
				switch head.Type {
				case "continue":
					select {
					case continueCh <- struct{}{}:
					default:
						// Buffer full — drop to keep the read loop snappy.
					}
				case "approve_tool":
					var msg struct {
						Type       string `json:"type"`
						ToolCallID string `json:"tool_call_id"`
						Approved   bool   `json:"approved"`
						Reason     string `json:"reason,omitempty"`
					}
					if json.Unmarshal(raw, &msg) != nil {
						continue
					}
					select {
					case approveCh <- workflow.ApprovalDecision{
						ToolCallID: msg.ToolCallID,
						Approved:   msg.Approved,
						Reason:     msg.Reason,
					}:
					default:
						// Buffer full — drop. Pathological under our 16-slot
						// buffer; would mean the agent loop stopped reading.
					}
				case "set_breakpoints":
					var msg struct {
						Type    string   `json:"type"`
						NodeIDs []string `json:"node_ids"`
					}
					if json.Unmarshal(raw, &msg) != nil {
						continue
					}
					select {
					case breakpointsCh <- msg.NodeIDs:
					default:
						// Buffer full — drop. The executor's forwarder
						// applies the most recent list, so a missed burst
						// just means the user has to click again.
					}
				}
				// Future: handle "interrupt", etc.
			}
		}()

		wf, err := store.GetByID(runCtx, id)
		if err != nil {
			writeWfWsError(ws, err.Error())
			return
		}

		emitter := &wsEventEmitter{ws: ws}

		// RunResumable internally emits the terminal `run_done` event with
		// the correct status + run_id from inside the BFS, so we don't
		// need a duplicate emit on the WS handler side. (Earlier code
		// double-emitted, which made the second frame land with a zero
		// timestamp.) Keep the outcome captured for any future logic that
		// needs the final status outside the stream.
		stopAtIDs := parseStopAt(req.StopAt)
		_, err = exec.RunResumable(runCtx, wf, workflow.RunOpts{
			StopAtIDs:     stopAtIDs,
			Input:         req.Input,
			ResumeRunID:   req.ResumeRunID,
			Emitter:       emitter,
			ContinueCh:    continueCh,
			ApproveCh:     approveCh,
			BreakpointsCh: breakpointsCh,
		})
		if err != nil {
			// Cost cap breaches already emit `cost_exceeded` from inside the
			// executor; re-emitting as a generic `error` produces a duplicate
			// toast. Suppress here for that one typed case.
			if !workflow.IsCostExceeded(err) {
				emitter.Emit(workflow.RunEvent{Type: "error", Error: err.Error()})
			}
			return
		}
	}
}
