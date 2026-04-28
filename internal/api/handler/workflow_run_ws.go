// Workflow run streaming over WebSocket.
//
// Protocol (browser ↔ this handler):
//
//	→ {"type": "run", "input": <any>, "stop_at": "<node-id>"}
//	← {"type": "step_start", "node_id": "...", "node_type": "..."}
//	← {"type": "step_done",  "node_id": "...", "output": <any>, "error": "...", "is_error": true}
//	← {"type": "agent_iter",        "node_id": "<agent-node>", "iter": 0}
//	← {"type": "agent_llm",         "node_id": "...", "iter": 0, "text": "...", "usage": {...}}
//	← {"type": "agent_tool_call",   "node_id": "...", "iter": 0, "tool_name": "...", "tool_id": "...", "tool_args": {...}}
//	← {"type": "agent_tool_result", "node_id": "...", "iter": 0, "tool_name": "...", "tool_id": "...", "result": "...", "is_error": false}
//	← {"type": "agent_final",       "node_id": "...", "iter": 0, "text": "..."}
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
type runWfWsRequest struct {
	Type   string `json:"type"`
	Input  any    `json:"input,omitempty"`
	StopAt string `json:"stop_at,omitempty"`
}

// runWfWsFrame is the server→client envelope. Same shape as
// workflow.RunEvent plus a generic Error field for upgrade/load failures.
type runWfWsFrame struct {
	workflow.RunEvent
	Error string `json:"error,omitempty"`
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

// writeWfWsError is a one-off error frame writer for failures that
// happen before the run starts (bad upgrade, missing workflow, etc.).
func writeWfWsError(ws *websocket.Conn, msg string) {
	frame := runWfWsFrame{Error: msg, RunEvent: workflow.RunEvent{Type: "error"}}
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
		// socket. A parallel goroutine listens for the inevitable read
		// error from a closed conn and triggers the cancel.
		runCtx, cancel := context.WithCancel(c.Request.Context())
		defer cancel()
		go func() {
			for {
				if _, _, err := ws.ReadMessage(); err != nil {
					cancel()
					return
				}
				// Future: handle "interrupt", "approve_tool", etc.
				// For now further messages are ignored.
			}
		}()

		wf, err := store.GetByID(runCtx, id)
		if err != nil {
			writeWfWsError(ws, err.Error())
			return
		}

		emitter := &wsEventEmitter{ws: ws}

		var initialInput []any
		if req.Input != nil {
			initialInput = []any{req.Input}
		}

		if _, err := exec.RunWithEvents(runCtx, wf, req.StopAt, emitter, initialInput...); err != nil {
			emitter.Emit(workflow.RunEvent{Type: "error", Error: err.Error()})
			return
		}
	}
}
