package dap

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
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
	conn   net.Conn
	reader *bufio.Reader
	mu     sync.Mutex // protects writes
	seq    atomic.Int32
	events chan DebugEvent
	done   chan struct{}
}

// NewClient dials a DAP server at the given address.
func NewClient(addr string, timeout time.Duration) (*Client, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("dap: dial %s: %w", addr, err)
	}

	c := &Client{
		conn:   conn,
		reader: bufio.NewReader(conn),
		events: make(chan DebugEvent, 64),
		done:   make(chan struct{}),
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

// Initialize sends the DAP initialize request.
func (c *Client) Initialize() error {
	_, err := c.Send("initialize", InitializeRequestArgs{
		ClientID:        "immaiwin",
		ClientName:      "immaiwin-debugger",
		AdapterID:       "immaiwin",
		LinesStartAt1:   true,
		ColumnsStartAt1: true,
		PathFormat:      "path",
	})
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

// SetBreakpoints sets breakpoints for a source file.
func (c *Client) SetBreakpoints(file string, breakpoints []SourceBreakpoint) error {
	_, err := c.Send("setBreakpoints", SetBreakpointsArgs{
		Source:      Source{Path: file, Name: file},
		Breakpoints: breakpoints,
	})
	return err
}

// ConfigurationDone tells the adapter configuration is complete.
func (c *Client) ConfigurationDone() error {
	_, err := c.Send("configurationDone", ConfigurationDoneArgs{})
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
			c.handleResponse(msg)
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
		reason, _ := body["reason"].(string)
		threadId := intFromAny(body["threadId"])
		de = DebugEvent{
			Type:     "stopped",
			Reason:   reason,
			ThreadId: threadId,
		}

	case "output":
		category, _ := body["category"].(string)
		output, _ := body["output"].(string)
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
