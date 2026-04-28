package handler

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/sandbox"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/sandbox/dap"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

var debugUpgrader = websocket.Upgrader{
	CheckOrigin:     func(r *http.Request) bool { return true },
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
}

// wsMessage is the envelope for messages between browser and this handler.
type wsMessage struct {
	Type        string               `json:"type"`
	Language    sandbox.Language     `json:"language,omitempty"`
	Code        string               `json:"code,omitempty"`
	Input       any                  `json:"input,omitempty"`
	Context     map[string]any       `json:"context,omitempty"`
	Image       string               `json:"image,omitempty"`    // custom Docker image override
	Packages    string               `json:"packages,omitempty"` // comma-separated package names for auto-build
	Breakpoints []sandbox.Breakpoint `json:"breakpoints,omitempty"`
	Expression  string               `json:"expression,omitempty"`
	ObjectId    string               `json:"objectId,omitempty"` // CDP object reference for expand
	SessionID   string               `json:"session_id,omitempty"`
	// Response fields
	Reason    string           `json:"reason,omitempty"`
	Line      int              `json:"line,omitempty"`
	Variables []dap.Variable   `json:"variables,omitempty"`
	CallStack []dap.StackFrame `json:"callStack,omitempty"`
	Stream    string           `json:"stream,omitempty"`
	Data      string           `json:"data,omitempty"`
	Error     string           `json:"error,omitempty"`
}

// DebugSandbox returns a gin handler that upgrades to WebSocket and proxies
// debug commands between the browser and a DAP/CDP debug adapter in a container.
func DebugSandbox(mgr sandbox.Runtime) gin.HandlerFunc {
	return func(c *gin.Context) {
		if mgr == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "sandbox not enabled"})
			return
		}

		ws, err := debugUpgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			slog.Error("sandbox-debug: ws upgrade failed", "err", err)
			return
		}
		defer func() { _ = ws.Close() }()

		// Wait for first message — must be "start"
		_, raw, err := ws.ReadMessage()
		if err != nil {
			return
		}

		var msg wsMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			writeWS(ws, wsMessage{Type: "error", Error: "invalid JSON"})
			return
		}

		if msg.Type != "start" {
			writeWS(ws, wsMessage{Type: "error", Error: "first message must be 'start'"})
			return
		}

		ctx := c.Request.Context()

		req := sandbox.DebugRequest{
			RunRequest: sandbox.RunRequest{
				Language: msg.Language,
				Code:     msg.Code,
				Input:    msg.Input,
				Context:  msg.Context,
				Image:    msg.Image,
				Packages: parsePackages(msg.Packages),
				Network:  true,
			},
			Breakpoints: msg.Breakpoints,
		}

		session, err := mgr.StartDebug(ctx, req)
		if err != nil {
			writeWS(ws, wsMessage{Type: "error", Error: err.Error()})
			return
		}
		defer func() { _ = mgr.StopDebug(session.ID) }()

		writeWS(ws, wsMessage{Type: "started", SessionID: session.ID})

		// connectAndProxy owns WS reads from here on — no race
		if err := connectAndProxy(ws, session, msg.Breakpoints); err != nil {
			slog.Error("sandbox-debug: proxy error", "err", err, "session", session.ID)
			writeWS(ws, wsMessage{Type: "error", Error: err.Error()})
		}
	}
}

// connectAndProxy connects to the debug adapter (DAP for Python, CDP for JS),
// sets initial breakpoints, then relays events/commands between browser and debugger.
func connectAndProxy(ws *websocket.Conn, session *sandbox.DebugSession, initialBPs []sandbox.Breakpoint) error {
	addr := fmt.Sprintf("localhost:%d", session.DebugPort)

	var debugEvents <-chan dap.DebugEvent
	var debugController debugProxy

	// Retry connection — debugger needs time to start
	maxRetries := 30
	for i := 0; i < maxRetries; i++ {
		time.Sleep(500 * time.Millisecond)

		switch session.Language {
		case sandbox.LangPython:
			client, err := dap.NewClient(addr, 3*time.Second)
			if err != nil {
				if i == maxRetries-1 {
					return fmt.Errorf("connect to debugpy: %w", err)
				}
				continue
			}
			debugEvents = client.Events()
			debugController = &dapProxy{client: client}

		case sandbox.LangJavaScript:
			client, err := dap.NewCDPClient("localhost", session.DebugPort, 3*time.Second)
			if err != nil {
				if i == maxRetries-1 {
					return fmt.Errorf("connect to node inspect: %w", err)
				}
				continue
			}
			debugEvents = client.Events()
			debugController = &cdpProxy{client: client}

		default:
			return fmt.Errorf("unsupported debug language: %s", session.Language)
		}
		break
	}

	if debugController == nil {
		return fmt.Errorf("failed to connect to debug adapter")
	}
	defer func() { _ = debugController.Close() }()

	// Initialize → set breakpoints → start execution
	if err := debugController.Initialize(); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}

	// Set initial breakpoints BEFORE starting execution
	if len(initialBPs) > 0 {
		slog.Debug("sandbox-debug: setting initial breakpoints", "count", len(initialBPs))
		if err := debugController.SetBreakpoints(initialBPs); err != nil {
			slog.Warn("sandbox-debug: set initial breakpoints failed", "err", err)
		}
	}

	// Start execution (resume from initial pause)
	if err := debugController.Start(); err != nil {
		return fmt.Errorf("start execution: %w", err)
	}

	writeWS(ws, wsMessage{Type: "connected"})

	// Relay debugger events → browser (in background)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for event := range debugEvents {
			// Track callFrameId for evaluate expressions (CDP-specific)
			if event.Type == "stopped" && event.CallFrameId != "" {
				if cp, ok := debugController.(*cdpProxy); ok {
					cp.mu.Lock()
					cp.callFrameId = event.CallFrameId
					cp.mu.Unlock()
				}
			}

			outMsg := wsMessage{
				Type:      event.Type,
				Reason:    event.Reason,
				Line:      event.Line,
				Variables: event.Variables,
				CallStack: event.CallStack,
				Stream:    event.Stream,
				Data:      event.Data,
			}
			writeWS(ws, outMsg)

			// Debug adapter says session over — close browser WS to unblock
			// main read loop so connectAndProxy returns and StopDebug runs.
			if event.Type == "terminated" {
				_ = ws.Close()
				return
			}
		}
		// Events channel closed without terminated event (adapter crashed) — notify browser
		writeWS(ws, wsMessage{Type: "terminated"})
		_ = ws.Close()
	}()

	// Read browser commands → forward to debugger
	for {
		_, raw, err := ws.ReadMessage()
		if err != nil {
			return nil
		}

		var msg wsMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "set_breakpoints":
			if err := debugController.SetBreakpoints(msg.Breakpoints); err != nil {
				writeWS(ws, wsMessage{Type: "error", Error: err.Error()})
			}
		case "continue":
			if err := debugController.Continue(); err != nil {
				writeWS(ws, wsMessage{Type: "error", Error: err.Error()})
			}
		case "step_over":
			if err := debugController.StepOver(); err != nil {
				writeWS(ws, wsMessage{Type: "error", Error: err.Error()})
			}
		case "step_in":
			if err := debugController.StepIn(); err != nil {
				writeWS(ws, wsMessage{Type: "error", Error: err.Error()})
			}
		case "step_out":
			if err := debugController.StepOut(); err != nil {
				writeWS(ws, wsMessage{Type: "error", Error: err.Error()})
			}
		case "evaluate":
			if err := debugController.Evaluate(msg.Expression); err != nil {
				writeWS(ws, wsMessage{Type: "error", Error: err.Error()})
			}
		case "expand":
			vars, err := debugController.Expand(msg.ObjectId)
			if err != nil {
				writeWS(ws, wsMessage{Type: "error", Error: err.Error()})
			} else {
				writeWS(ws, wsMessage{Type: "expand", ObjectId: msg.ObjectId, Variables: vars})
			}
		case "disconnect":
			_ = debugController.Disconnect()
			return nil
		}
	}
}

// debugProxy abstracts DAP vs CDP for the handler.
type debugProxy interface {
	Initialize() error
	SetBreakpoints(bps []sandbox.Breakpoint) error
	Start() error // resume from initial pause (after breakpoints set)
	Continue() error
	StepOver() error
	StepIn() error
	StepOut() error
	Evaluate(expr string) error
	Expand(objectId string) ([]dap.Variable, error)
	Disconnect() error
	Close() error
}

// --- DAP proxy (Python / debugpy) ---

type dapProxy struct {
	client      *dap.Client
	breakpoints []sandbox.Breakpoint
	started     bool
}

func (p *dapProxy) Initialize() error {
	return p.client.Initialize()
}

func (p *dapProxy) SetBreakpoints(bps []sandbox.Breakpoint) error {
	p.breakpoints = bps
	if !p.started {
		// Before Start() — store only. DAP requires: attach → initialized → setBreakpoints.
		return nil
	}
	// After Start() — send to debugpy immediately (runtime breakpoint update)
	var srcBps []dap.SourceBreakpoint
	for _, bp := range bps {
		srcBps = append(srcBps, dap.SourceBreakpoint{
			Line:      bp.Line,
			Condition: bp.Condition,
		})
	}
	return p.client.SetBreakpoints("/sandbox/user_script.py", srcBps)
}

func (p *dapProxy) Start() error {
	// DAP protocol: initialize → attach → (initialized event) → setBreakpoints → configurationDone
	// attach response is DEFERRED until after configurationDone (DAP spec), so fire-and-forget.

	// 1. Attach (fire-and-forget)
	if err := p.client.Attach(); err != nil {
		return err
	}

	// 2. Wait for initialized event (adapter ready for breakpoint configuration)
	select {
	case <-p.client.Initialized():
		slog.Info("dap-proxy: initialized event received")
	case <-time.After(5 * time.Second):
		return fmt.Errorf("timeout waiting for DAP initialized event")
	}

	// 3. Set breakpoints (stored from earlier SetBreakpoints call)
	if len(p.breakpoints) > 0 {
		var srcBps []dap.SourceBreakpoint
		for _, bp := range p.breakpoints {
			srcBps = append(srcBps, dap.SourceBreakpoint{
				Line:      bp.Line,
				Condition: bp.Condition,
			})
		}
		if err := p.client.SetBreakpoints("/sandbox/user_script.py", srcBps); err != nil {
			slog.Warn("dap-proxy: setBreakpoints failed", "err", err)
		}
	}

	// 4. ConfigurationDone → debugpy unblocks wait_for_client() → entrypoint runs user code
	p.started = true
	return p.client.ConfigurationDone()
}

func (p *dapProxy) Continue() error              { return p.client.Continue(1) }
func (p *dapProxy) StepOver() error              { return p.client.Next(1) }
func (p *dapProxy) StepIn() error                { return p.client.StepIn(1) }
func (p *dapProxy) StepOut() error               { return p.client.StepOut(1) }
func (p *dapProxy) Evaluate(expr string) error                   { return p.client.Evaluate(expr, 0) }
func (p *dapProxy) Expand(_ string) ([]dap.Variable, error)      { return nil, fmt.Errorf("expand not supported for DAP") }
func (p *dapProxy) Disconnect() error                            { return p.client.Disconnect() }
func (p *dapProxy) Close() error                                 { return p.client.Close() }

// --- CDP proxy (JavaScript / Node.js --inspect) ---

type cdpProxy struct {
	client      *dap.CDPClient
	mu          sync.Mutex
	callFrameId string
	breakpoints []sandbox.Breakpoint // stored for re-set after script loads
	bpIDs       map[int]string       // line → CDP breakpointId for removal
}

func (p *cdpProxy) Initialize() error {
	return p.client.Enable()
}

func (p *cdpProxy) SetBreakpoints(bps []sandbox.Breakpoint) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.bpIDs == nil {
		p.bpIDs = make(map[int]string)
	}

	// Build set of desired lines for quick lookup.
	desired := make(map[int]sandbox.Breakpoint, len(bps))
	for _, bp := range bps {
		desired[bp.Line] = bp
	}

	// Remove breakpoints that are no longer in the desired set.
	for line, id := range p.bpIDs {
		if _, keep := desired[line]; keep {
			continue
		}
		if err := p.client.RemoveBreakpoint(id); err != nil {
			slog.Warn("cdp-proxy: removeBreakpoint failed", "line", line, "id", id, "err", err)
		}
		delete(p.bpIDs, line)
	}

	// Add breakpoints not yet set. Skip lines we already have to avoid duplicates.
	for line, bp := range desired {
		if _, exists := p.bpIDs[line]; exists {
			continue
		}
		id, err := p.client.SetBreakpoint("file:///sandbox/user_script.mjs", bp.Line, bp.Condition)
		if err != nil {
			return err
		}
		p.bpIDs[line] = id
	}

	p.breakpoints = bps // keep latest snapshot for any future re-set logic
	return nil
}

func (p *cdpProxy) Start() error {
	// Resume from --inspect-brk. This is an inspector-level pause,
	// NOT a Debugger-domain pause — must use Runtime.runIfWaitingForDebugger.
	slog.Info("cdp-proxy: Runtime.runIfWaitingForDebugger (resume from --inspect-brk)")
	if err := p.client.RunIfWaitingForDebugger(); err != nil {
		return err
	}

	// Wait for "Break on start" stopped event from --inspect-brk, then auto-resume.
	// After resume, dynamic import() loads user_script.mjs → urlRegex breakpoints resolve → V8 pauses at user BPs.
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()

	for {
		select {
		case event, ok := <-p.client.Events():
			if !ok {
				return fmt.Errorf("debug event channel closed before initial pause")
			}
			if event.Type == "stopped" {
				slog.Info("cdp-proxy: Break on start, auto-resuming",
					"reason", event.Reason, "line", event.Line)
				return p.client.Resume()
			}
			slog.Debug("cdp-proxy: skipping event during startup", "type", event.Type)

		case <-timer.C:
			slog.Warn("cdp-proxy: no Break on start within 10s, continuing anyway")
			return nil
		}
	}
}

func (p *cdpProxy) Continue() error  { return p.client.Resume() }
func (p *cdpProxy) StepOver() error  { return p.client.StepOver() }
func (p *cdpProxy) StepIn() error    { return p.client.StepInto() }
func (p *cdpProxy) StepOut() error   { return p.client.StepOut() }
func (p *cdpProxy) Evaluate(expr string) error {
	p.mu.Lock()
	cfId := p.callFrameId
	p.mu.Unlock()
	return p.client.Evaluate(expr, cfId)
}
func (p *cdpProxy) Expand(objectId string) ([]dap.Variable, error) {
	return p.client.ExpandObject(objectId)
}
func (p *cdpProxy) Disconnect() error { return p.client.Disconnect() }
func (p *cdpProxy) Close() error      { return p.client.Close() }

// writeWS writes a JSON message to the WebSocket. Thread-safe via mutex.
var wsMu sync.Mutex

func writeWS(ws *websocket.Conn, msg wsMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	wsMu.Lock()
	defer wsMu.Unlock()
	_ = ws.WriteMessage(websocket.TextMessage, data)
}
