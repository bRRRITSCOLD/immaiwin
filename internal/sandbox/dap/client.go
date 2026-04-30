package dap

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Client is a DAP TCP client that speaks the Debug Adapter Protocol
// (Content-Length header framing over TCP).
type Client struct {
	conn        net.Conn
	reader      *bufio.Reader
	mu          sync.Mutex // protects writes
	seq         atomic.Int32
	events      chan DebugEvent
	done        chan struct{}
	pending     sync.Map      // seq (int) -> chan map[string]any (for waiting on responses)
	initialized chan struct{} // closed when adapter sends initialized event
}

// NewClient dials a DAP server at the given address.
func NewClient(addr string, timeout time.Duration) (*Client, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("dap: dial %s: %w", addr, err)
	}

	c := &Client{
		conn:        conn,
		reader:      bufio.NewReader(conn),
		events:      make(chan DebugEvent, 64),
		done:        make(chan struct{}),
		initialized: make(chan struct{}),
	}

	go c.readLoop()
	return c, nil
}

// Events returns the channel of debug events from the adapter.
func (c *Client) Events() <-chan DebugEvent { return c.events }

// Close shuts down the connection.
func (c *Client) Close() error {
	select {
	case <-c.done:
	default:
		close(c.done)
	}
	return c.conn.Close()
}

// Send sends a raw DAP request and returns the sequence number.
func (c *Client) Send(command string, args any) (int, error) {
	seq := int(c.seq.Add(1))
	req := Request{
		Message:   Message{Seq: seq, Type: "request"},
		Command:   command,
		Arguments: args,
	}
	return seq, c.writeMessage(req)
}

// SendAndWait sends a DAP request and waits for the response.
func (c *Client) SendAndWait(command string, args any, timeout time.Duration) (map[string]any, error) {
	seq := int(c.seq.Add(1))
	req := Request{
		Message:   Message{Seq: seq, Type: "request"},
		Command:   command,
		Arguments: args,
	}

	respCh := make(chan map[string]any, 1)
	c.pending.Store(seq, respCh)
	defer c.pending.Delete(seq)

	if err := c.writeMessage(req); err != nil {
		return nil, err
	}

	select {
	case resp := <-respCh:
		success, _ := resp["success"].(bool)
		if !success {
			errMsg, _ := resp["message"].(string)
			cmd, _ := resp["command"].(string)
			return resp, fmt.Errorf("dap: %s: %s", cmd, errMsg)
		}
		return resp, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("dap: %s: timeout after %s", command, timeout)
	case <-c.done:
		return nil, fmt.Errorf("dap: closed")
	}
}

// Initialize sends the DAP initialize request and waits for response.
func (c *Client) Initialize() error {
	slog.Info("dap: initialize")
	_, err := c.SendAndWait("initialize", InitializeRequestArgs{
		ClientID:        "burrow",
		ClientName:      "burrow-debugger",
		AdapterID:       "burrow",
		LinesStartAt1:   true,
		ColumnsStartAt1: true,
		PathFormat:      "path",
	}, 5*time.Second)
	return err
}

// Launch sends the DAP launch request.
func (c *Client) Launch(program string) error {
	_, err := c.Send("launch", LaunchRequestArgs{
		Program:    program,
		JustMyCode: true,
	})
	return err
}

// Attach sends the DAP attach request (fire-and-forget).
// DAP spec: attach response is deferred until after configurationDone.
// Using SendAndWait would deadlock.
func (c *Client) Attach() error {
	slog.Info("dap: attach (fire-and-forget)")
	_, err := c.Send("attach", AttachRequestArgs{
		JustMyCode:     true,
		RedirectOutput: true,
	})
	return err
}

// Initialized returns a channel that's closed when the adapter sends the initialized event.
func (c *Client) Initialized() <-chan struct{} { return c.initialized }

// SetBreakpoints sets breakpoints for a source file and waits for response.
func (c *Client) SetBreakpoints(file string, breakpoints []SourceBreakpoint) error {
	slog.Info("dap: setBreakpoints", "file", file, "count", len(breakpoints))
	resp, err := c.SendAndWait("setBreakpoints", SetBreakpointsArgs{
		Source:      Source{Path: file, Name: file},
		Breakpoints: breakpoints,
	}, 5*time.Second)
	if err != nil {
		return err
	}
	if body, ok := resp["body"].(map[string]any); ok {
		slog.Info("dap: breakpoints set", "body", body)
	}
	return nil
}

// ConfigurationDone tells the adapter configuration is complete and waits for response.
func (c *Client) ConfigurationDone() error {
	slog.Info("dap: configurationDone")
	_, err := c.SendAndWait("configurationDone", ConfigurationDoneArgs{}, 5*time.Second)
	return err
}

// Continue resumes execution.
func (c *Client) Continue(threadId int) error {
	_, err := c.Send("continue", ContinueArgs{ThreadId: threadId})
	return err
}

// Next steps over.
func (c *Client) Next(threadId int) error {
	_, err := c.Send("next", NextArgs{ThreadId: threadId})
	return err
}

// StepIn steps into.
func (c *Client) StepIn(threadId int) error {
	_, err := c.Send("stepIn", StepInArgs{ThreadId: threadId})
	return err
}

// StepOut steps out.
func (c *Client) StepOut(threadId int) error {
	_, err := c.Send("stepOut", StepOutArgs{ThreadId: threadId})
	return err
}

// Threads fetches the list of threads.
func (c *Client) Threads() error {
	_, err := c.Send("threads", ThreadsArgs{})
	return err
}

// StackTrace requests the call stack for a thread.
func (c *Client) StackTrace(threadId int) error {
	_, err := c.Send("stackTrace", StackTraceArgs{ThreadId: threadId, Levels: 20})
	return err
}

// Scopes requests the scopes for a frame.
func (c *Client) Scopes(frameId int) error {
	_, err := c.Send("scopes", ScopesArgs{FrameId: frameId})
	return err
}

// Variables requests variables for a scope.
func (c *Client) Variables(ref int) error {
	_, err := c.Send("variables", VariablesArgs{VariablesReference: ref})
	return err
}

// Evaluate evaluates an expression.
func (c *Client) Evaluate(expr string, frameId int) error {
	_, err := c.Send("evaluate", EvaluateArgs{
		Expression: expr,
		FrameId:    frameId,
		Context:    "repl",
	})
	return err
}

// Disconnect terminates the debug session.
func (c *Client) Disconnect() error {
	_, err := c.Send("disconnect", DisconnectArgs{TerminateDebuggee: true})
	return err
}

// writeMessage serializes a DAP message with Content-Length header framing.
func (c *Client) writeMessage(msg any) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("dap: marshal: %w", err)
	}

	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := io.WriteString(c.conn, header); err != nil {
		return fmt.Errorf("dap: write header: %w", err)
	}
	if _, err := c.conn.Write(body); err != nil {
		return fmt.Errorf("dap: write body: %w", err)
	}
	return nil
}

// readLoop reads DAP messages and dispatches events.
func (c *Client) readLoop() {
	defer close(c.events)

	for {
		select {
		case <-c.done:
			return
		default:
		}

		msg, err := c.readMessage()
		if err != nil {
			return
		}

		msgType, _ := msg["type"].(string)

		switch msgType {
		case "event":
			c.handleEvent(msg)
		case "response":
			// Route to pending waiter if someone is waiting for this response
			reqSeq := intFromAny(msg["request_seq"])
			if ch, loaded := c.pending.LoadAndDelete(reqSeq); loaded {
				ch.(chan map[string]any) <- msg
			} else {
				c.handleResponse(msg)
			}
		}
	}
}

// readMessage reads a single DAP message (Content-Length framed).
func (c *Client) readMessage() (map[string]any, error) {
	// Read headers until blank line
	var contentLength int
	for {
		line, err := c.reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "Content-Length:") {
			val := strings.TrimSpace(strings.TrimPrefix(line, "Content-Length:"))
			contentLength, _ = strconv.Atoi(val)
		}
	}

	if contentLength == 0 {
		return nil, fmt.Errorf("dap: missing Content-Length")
	}

	body := make([]byte, contentLength)
	if _, err := io.ReadFull(c.reader, body); err != nil {
		return nil, fmt.Errorf("dap: read body: %w", err)
	}

	var msg map[string]any
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, fmt.Errorf("dap: unmarshal: %w", err)
	}

	return msg, nil
}

// handleEvent processes DAP events and emits DebugEvents.
func (c *Client) handleEvent(msg map[string]any) {
	event, _ := msg["event"].(string)
	body, _ := msg["body"].(map[string]any)
	if body == nil {
		body = map[string]any{}
	}

	var de DebugEvent

	switch event {
	case "stopped":
		// Spawn goroutine to enrich with stack trace + variables via SendAndWait
		// (can't call SendAndWait from readLoop — would deadlock)
		reason, _ := body["reason"].(string)
		threadId := intFromAny(body["threadId"])
		go c.handleStoppedEvent(reason, threadId)
		return // event emitted from goroutine

	case "output":
		category, _ := body["category"].(string)
		output, _ := body["output"].(string)
		// Filter debugpy/ptvsd startup noise
		trimmed := strings.TrimSpace(output)
		if category == "stderr" || category == "console" {
			if trimmed == "ptvsd" || trimmed == "debugpy" ||
				strings.HasPrefix(trimmed, "debugpy:") ||
				strings.HasPrefix(trimmed, "pydevd") {
				return
			}
		}
		stream := "stdout"
		if category == "stderr" {
			stream = "stderr"
		}
		de = DebugEvent{
			Type:   "output",
			Stream: stream,
			Data:   output,
		}

	case "terminated":
		de = DebugEvent{Type: "terminated"}

	case "exited":
		de = DebugEvent{Type: "terminated"}

	case "initialized":
		slog.Info("dap: initialized event received")
		select {
		case <-c.initialized:
			// already closed
		default:
			close(c.initialized)
		}
		return

	default:
		return
	}

	select {
	case c.events <- de:
	case <-c.done:
	}
}

// handleResponse processes DAP responses — we extract variable/stack info
// from relevant responses and emit as events so the handler can relay them.
func (c *Client) handleResponse(msg map[string]any) {
	command, _ := msg["command"].(string)
	body, _ := msg["body"].(map[string]any)
	if body == nil {
		return
	}

	switch command {
	case "stackTrace":
		raw, _ := json.Marshal(body)
		var stBody StackTraceResponseBody
		_ = json.Unmarshal(raw, &stBody)
		de := DebugEvent{
			Type:      "stopped",
			CallStack: stBody.StackFrames,
		}
		select {
		case c.events <- de:
		case <-c.done:
		}

	case "variables":
		raw, _ := json.Marshal(body)
		var vBody VariablesResponseBody
		_ = json.Unmarshal(raw, &vBody)
		de := DebugEvent{
			Type:      "variables",
			Variables: vBody.Variables,
		}
		select {
		case c.events <- de:
		case <-c.done:
		}

	case "evaluate":
		raw, _ := json.Marshal(body)
		var eBody EvaluateResponseBody
		_ = json.Unmarshal(raw, &eBody)
		de := DebugEvent{
			Type: "evaluate",
			Data: eBody.Result,
		}
		select {
		case c.events <- de:
		case <-c.done:
		}
	}
}

// handleStoppedEvent processes a DAP stopped event in a separate goroutine.
// It fetches stack trace, scopes, and variables via SendAndWait (which would
// deadlock if called from readLoop), then emits an enriched DebugEvent.
func (c *Client) handleStoppedEvent(reason string, threadId int) {
	de := DebugEvent{
		Type:     "stopped",
		Reason:   reason,
		ThreadId: threadId,
	}

	// 1. Fetch stack trace
	stResp, err := c.SendAndWait("stackTrace", StackTraceArgs{
		ThreadId: threadId,
		Levels:   20,
	}, 3*time.Second)
	if err != nil {
		slog.Warn("dap: stackTrace failed", "err", err)
	} else if body, ok := stResp["body"].(map[string]any); ok {
		raw, _ := json.Marshal(body)
		var stBody StackTraceResponseBody
		if json.Unmarshal(raw, &stBody) == nil {
			de.CallStack = stBody.StackFrames
			if len(stBody.StackFrames) > 0 {
				de.Line = stBody.StackFrames[0].Line
			}
		}
	}

	// 2. Fetch scopes for top frame → then variables for each scope
	if len(de.CallStack) > 0 {
		topFrameId := de.CallStack[0].Id
		scopesResp, err := c.SendAndWait("scopes", ScopesArgs{
			FrameId: topFrameId,
		}, 3*time.Second)
		if err != nil {
			slog.Warn("dap: scopes failed", "err", err)
		} else if body, ok := scopesResp["body"].(map[string]any); ok {
			raw, _ := json.Marshal(body)
			var scBody ScopesResponseBody
			if json.Unmarshal(raw, &scBody) == nil {
				seen := make(map[string]bool)
				for _, scope := range scBody.Scopes {
					if scope.Expensive || scope.VariablesReference == 0 {
						continue
					}
					varsResp, err := c.SendAndWait("variables", VariablesArgs{
						VariablesReference: scope.VariablesReference,
					}, 3*time.Second)
					if err != nil {
						slog.Warn("dap: variables failed", "scope", scope.Name, "err", err)
						continue
					}
					if body, ok := varsResp["body"].(map[string]any); ok {
						raw, _ := json.Marshal(body)
						var vBody VariablesResponseBody
						if json.Unmarshal(raw, &vBody) == nil {
							for _, v := range vBody.Variables {
								if !seen[v.Name] {
									seen[v.Name] = true
									de.Variables = append(de.Variables, v)
								}
							}
						}
					}
				}
			}
		}
	}

	slog.Info("dap: stopped", "reason", de.Reason, "line", de.Line,
		"threadId", de.ThreadId, "frames", len(de.CallStack), "vars", len(de.Variables))

	select {
	case c.events <- de:
	case <-c.done:
	}
}

func intFromAny(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}
