package dap

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// CDPClient speaks Chrome DevTools Protocol to a Node.js --inspect debugger.
// It translates CDP events into the same DebugEvent type used by the DAP client
// so the handler can treat both uniformly.
type CDPClient struct {
	conn     *websocket.Conn
	mu       sync.Mutex // protects writes
	seq      atomic.Int32
	events   chan DebugEvent
	done     chan struct{}
	pending    sync.Map // id -> chan map[string]any (for waiting on responses)
	scripts    sync.Map // url (string) -> scriptId (string) — tracks parsed scripts
	scriptURLs sync.Map // scriptId (string) -> url (string) — reverse lookup
}

// NewCDPClient connects to a Node.js --inspect endpoint.
// First fetches /json/list to discover the WS debugger URL, then connects.
func NewCDPClient(host string, port int, timeout time.Duration) (*CDPClient, error) {
	httpURL := fmt.Sprintf("http://%s:%d/json/list", host, port)
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(httpURL)
	if err != nil {
		return nil, fmt.Errorf("cdp: get /json/list: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("cdp: read /json/list: %w", err)
	}

	var targets []struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal(body, &targets); err != nil {
		return nil, fmt.Errorf("cdp: parse /json/list: %w", err)
	}
	if len(targets) == 0 || targets[0].WebSocketDebuggerURL == "" {
		return nil, fmt.Errorf("cdp: no debug targets found")
	}

	wsURL := targets[0].WebSocketDebuggerURL
	slog.Info("cdp: connecting", "url", wsURL)

	dialer := websocket.Dialer{HandshakeTimeout: timeout}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("cdp: ws dial %s: %w", wsURL, err)
	}

	c := &CDPClient{
		conn:   conn,
		events: make(chan DebugEvent, 64),
		done:   make(chan struct{}),
	}

	go c.readLoop()
	return c, nil
}

// Events returns the channel of debug events.
func (c *CDPClient) Events() <-chan DebugEvent { return c.events }

// Close shuts down the connection.
func (c *CDPClient) Close() error {
	select {
	case <-c.done:
	default:
		close(c.done)
	}
	return c.conn.Close()
}

// send sends a CDP method call (fire-and-forget).
func (c *CDPClient) send(method string, params any) (int, error) {
	id := int(c.seq.Add(1))
	msg := map[string]any{
		"id":     id,
		"method": method,
	}
	if params != nil {
		msg["params"] = params
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	return id, c.conn.WriteJSON(msg)
}

// sendAndWait sends a CDP method call and waits for the response.
func (c *CDPClient) sendAndWait(method string, params any, timeout time.Duration) (map[string]any, error) {
	id := int(c.seq.Add(1))
	msg := map[string]any{
		"id":     id,
		"method": method,
	}
	if params != nil {
		msg["params"] = params
	}

	respCh := make(chan map[string]any, 1)
	c.pending.Store(id, respCh)
	defer c.pending.Delete(id)

	c.mu.Lock()
	err := c.conn.WriteJSON(msg)
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}

	select {
	case resp := <-respCh:
		if errObj, ok := resp["error"].(map[string]any); ok {
			errMsg, _ := errObj["message"].(string)
			return nil, fmt.Errorf("cdp: %s: %s", method, errMsg)
		}
		return resp, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("cdp: %s: timeout after %s", method, timeout)
	case <-c.done:
		return nil, fmt.Errorf("cdp: closed")
	}
}

// Enable enables the Debugger and Runtime domains (waits for responses).
// Also sets pause-on-uncaught-exceptions so the debugger stops at throw
// points instead of silently terminating.
func (c *CDPClient) Enable() error {
	if _, err := c.sendAndWait("Debugger.enable", nil, 5*time.Second); err != nil {
		return err
	}
	slog.Info("cdp: Debugger.enable done")
	if _, err := c.sendAndWait("Debugger.setPauseOnExceptions", map[string]any{
		"state": "uncaught",
	}, 5*time.Second); err != nil {
		slog.Warn("cdp: setPauseOnExceptions failed", "err", err)
	}
	if _, err := c.sendAndWait("Runtime.enable", nil, 5*time.Second); err != nil {
		return err
	}
	slog.Info("cdp: Runtime.enable done")
	return nil
}

// SetBreakpoint sets a breakpoint by URL regex and line number (waits for response).
// Uses urlRegex to match regardless of file:// prefix or require() path format.
// Returns the CDP breakpointId so callers can remove the breakpoint later.
func (c *CDPClient) SetBreakpoint(url string, line int, condition string) (string, error) {
	// Extract filename and use regex to match any path format
	// e.g. "file:///sandbox/user_script.mjs" → regex "user_script\\.mjs$"
	filename := url
	if idx := len(url) - 1; idx >= 0 {
		for i := len(url) - 1; i >= 0; i-- {
			if url[i] == '/' {
				filename = url[i+1:]
				break
			}
		}
	}
	// Escape dots for regex
	escaped := ""
	for _, ch := range filename {
		if ch == '.' {
			escaped += "\\."
		} else {
			escaped += string(ch)
		}
	}
	urlRegex := escaped + "$"

	slog.Info("cdp: setBreakpoint", "urlRegex", urlRegex, "line", line, "cdpLine", line-1)
	params := map[string]any{
		"urlRegex":     urlRegex,
		"lineNumber":   line - 1, // CDP is 0-based
		"columnNumber": 0,
	}
	if condition != "" {
		params["condition"] = condition
	}
	resp, err := c.sendAndWait("Debugger.setBreakpointByUrl", params, 5*time.Second)
	if err != nil {
		return "", err
	}
	var bpID string
	if result, ok := resp["result"].(map[string]any); ok {
		slog.Info("cdp: breakpoint set", "result", result)
		if id, ok := result["breakpointId"].(string); ok {
			bpID = id
		}
	}
	return bpID, nil
}

// RemoveBreakpoint removes a breakpoint previously set via SetBreakpoint by id.
func (c *CDPClient) RemoveBreakpoint(breakpointId string) error {
	if breakpointId == "" {
		return nil
	}
	slog.Info("cdp: removeBreakpoint", "id", breakpointId)
	_, err := c.sendAndWait("Debugger.removeBreakpoint", map[string]any{
		"breakpointId": breakpointId,
	}, 5*time.Second)
	return err
}

// RemoveAllBreakpoints disables then re-enables debugger.
func (c *CDPClient) RemoveAllBreakpoints() error {
	if _, err := c.sendAndWait("Debugger.disable", nil, 5*time.Second); err != nil {
		return err
	}
	_, err := c.sendAndWait("Debugger.enable", nil, 5*time.Second)
	return err
}

// RunIfWaitingForDebugger resumes execution after --inspect-brk.
// This is different from Resume() — --inspect-brk uses a V8 inspector-level pause,
// not a Debugger-domain pause. Debugger.resume has no effect on it.
func (c *CDPClient) RunIfWaitingForDebugger() error {
	slog.Info("cdp: Runtime.runIfWaitingForDebugger")
	_, err := c.sendAndWait("Runtime.runIfWaitingForDebugger", nil, 5*time.Second)
	return err
}

// Resume continues execution from a Debugger-domain pause (breakpoint, debugger;, step).
func (c *CDPClient) Resume() error {
	slog.Info("cdp: Debugger.resume")
	_, err := c.sendAndWait("Debugger.resume", nil, 5*time.Second)
	return err
}

// StepOver steps over the current statement.
func (c *CDPClient) StepOver() error {
	_, err := c.send("Debugger.stepOver", nil)
	return err
}

// StepInto steps into the current statement.
func (c *CDPClient) StepInto() error {
	_, err := c.send("Debugger.stepInto", nil)
	return err
}

// StepOut steps out of the current function.
func (c *CDPClient) StepOut() error {
	_, err := c.send("Debugger.stepOut", nil)
	return err
}

// Evaluate evaluates an expression on the current call frame.
func (c *CDPClient) Evaluate(expr string, callFrameId string) error {
	if callFrameId != "" {
		_, err := c.send("Debugger.evaluateOnCallFrame", map[string]any{
			"callFrameId": callFrameId,
			"expression":  expr,
		})
		return err
	}
	_, err := c.send("Runtime.evaluate", map[string]any{
		"expression": expr,
	})
	return err
}

// GetProperties fetches object properties (fire-and-forget, response handled in readLoop).
func (c *CDPClient) GetProperties(objectId string) error {
	_, err := c.send("Runtime.getProperties", map[string]any{
		"objectId":               objectId,
		"ownProperties":          true,
		"generatePreview":        true,
		"accessorPropertiesOnly": false,
	})
	return err
}

// ExpandObject fetches child properties of an object by its CDP objectId.
// Returns Variable slice with nested objectIds for further expansion.
//
// Behavior:
//  - Includes inherited accessor properties (status/headers/ok on Response,
//    etc.) by walking the prototype chain.
//  - Materializes accessor getters via Runtime.callFunctionOn so values appear
//    instead of "[Function]".
//  - Filters Symbol(...) and __proto__ noise.
func (c *CDPClient) ExpandObject(objectId string) ([]Variable, error) {
	// First pass: own data props (with previews) — quick wins for plain objects.
	ownResp, err := c.sendAndWait("Runtime.getProperties", map[string]any{
		"objectId":               objectId,
		"ownProperties":          true,
		"generatePreview":        true,
		"accessorPropertiesOnly": false,
	}, 3*time.Second)
	if err != nil {
		return nil, err
	}

	// Second pass: accessor properties from full prototype chain (Response.status etc.)
	accResp, _ := c.sendAndWait("Runtime.getProperties", map[string]any{
		"objectId":               objectId,
		"ownProperties":          false,
		"generatePreview":        true,
		"accessorPropertiesOnly": true,
	}, 3*time.Second)

	seen := make(map[string]bool)
	var vars []Variable

	collect := func(resp map[string]any) {
		if resp == nil {
			return
		}
		result, _ := resp["result"].(map[string]any)
		if result == nil {
			return
		}
		props, _ := result["result"].([]any)
		for _, p := range props {
			prop, ok := p.(map[string]any)
			if !ok {
				continue
			}
			name, _ := prop["name"].(string)
			if name == "" || seen[name] {
				continue
			}
			// Filter noise: Symbol(...), __proto__, internal slots
			if strings.HasPrefix(name, "__") || strings.HasPrefix(name, "Symbol(") {
				continue
			}
			seen[name] = true

			v := c.propToVariable(name, prop, objectId)
			if v.Name != "" {
				vars = append(vars, v)
			}
		}
	}
	collect(ownResp)
	collect(accResp)
	return vars, nil
}

// propToVariable converts one CDP property descriptor to a Variable, calling
// the getter if the property is an accessor without a cached value.
func (c *CDPClient) propToVariable(name string, prop map[string]any, parentId string) Variable {
	if valObj, ok := prop["value"].(map[string]any); ok && valObj != nil {
		return remoteObjectToVariable(name, valObj)
	}
	// Accessor — call the getter to materialize a value.
	if _, hasGet := prop["get"].(map[string]any); hasGet {
		valObj, err := c.callGetter(parentId, name)
		if err == nil && valObj != nil {
			return remoteObjectToVariable(name, valObj)
		}
		return Variable{Name: name, Value: "<getter>", Type: "accessor"}
	}
	return Variable{Name: name, Value: "undefined", Type: "undefined"}
}

// callGetter invokes obj[propName] in the debuggee and returns its RemoteObject.
func (c *CDPClient) callGetter(objectId, propName string) (map[string]any, error) {
	resp, err := c.sendAndWait("Runtime.callFunctionOn", map[string]any{
		"objectId":            objectId,
		"functionDeclaration": fmt.Sprintf("function(){return this[%q];}", propName),
		"returnByValue":       false,
		"generatePreview":     true,
	}, 3*time.Second)
	if err != nil {
		return nil, err
	}
	result, _ := resp["result"].(map[string]any)
	if result == nil {
		return nil, fmt.Errorf("callFunctionOn: empty result")
	}
	if exc, ok := result["exceptionDetails"].(map[string]any); ok && exc != nil {
		return nil, fmt.Errorf("getter threw: %v", exc["text"])
	}
	if ro, ok := result["result"].(map[string]any); ok {
		return ro, nil
	}
	return nil, fmt.Errorf("callFunctionOn: missing result.result")
}

// remoteObjectToVariable renders a CDP RemoteObject into a Variable, preferring
// the inline preview for compound types (so "Response { status: 200, ... }"
// appears in the panel rather than just "_Response").
func remoteObjectToVariable(name string, valObj map[string]any) Variable {
	typ, _ := valObj["type"].(string)
	subtype, _ := valObj["subtype"].(string)

	var value string
	switch typ {
	case "undefined":
		value = "undefined"
	case "string":
		if v, ok := valObj["value"].(string); ok {
			value = fmt.Sprintf("%q", v)
		}
	case "object", "function":
		if preview, ok := valObj["preview"].(map[string]any); ok {
			value = renderPreview(preview, typ)
		}
		if value == "" {
			if desc, ok := valObj["description"].(string); ok {
				value = desc
			} else if subtype != "" {
				value = subtype
			} else {
				value = "[" + typ + "]"
			}
		}
	default:
		if v, ok := valObj["value"]; ok {
			value = fmt.Sprintf("%v", v)
		}
	}

	v := Variable{Name: name, Value: value, Type: typ}
	if typ == "object" || typ == "function" {
		if oid, ok := valObj["objectId"].(string); ok {
			v.ObjectId = oid
		}
	}
	return v
}

// renderPreview turns a CDP object preview into a one-line summary like
// "Response { status: 200, statusText: 'OK', ok: true, ... }".
func renderPreview(preview map[string]any, typ string) string {
	desc, _ := preview["description"].(string)
	subtype, _ := preview["subtype"].(string)
	props, _ := preview["properties"].([]any)
	if len(props) == 0 {
		return desc
	}
	var parts []string
	for _, p := range props {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		pn, _ := pm["name"].(string)
		pt, _ := pm["type"].(string)
		pv, _ := pm["value"].(string)
		if pt == "string" {
			parts = append(parts, fmt.Sprintf("%s: %q", pn, pv))
		} else {
			parts = append(parts, fmt.Sprintf("%s: %s", pn, pv))
		}
	}
	overflow := ""
	if of, ok := preview["overflow"].(bool); ok && of {
		overflow = ", ..."
	}
	switch typ {
	case "object":
		if subtype == "array" {
			return fmt.Sprintf("[ %s%s ]", strings.Join(parts, ", "), overflow)
		}
		head := desc
		if head == "" {
			head = "Object"
		}
		return fmt.Sprintf("%s { %s%s }", head, strings.Join(parts, ", "), overflow)
	case "function":
		if desc != "" {
			return desc
		}
		return "[Function]"
	}
	return desc
}

// FindScriptId returns the CDP scriptId for a URL containing the given substring.
func (c *CDPClient) FindScriptId(urlSubstring string) (string, bool) {
	var found string
	c.scripts.Range(func(key, value any) bool {
		url := key.(string)
		if strings.Contains(url, urlSubstring) {
			found = value.(string)
			return false // stop iterating
		}
		return true
	})
	return found, found != ""
}

// SetBreakpointByScriptId sets a breakpoint using the exact scriptId (for already-loaded scripts).
func (c *CDPClient) SetBreakpointByScriptId(scriptId string, line int, condition string) error {
	slog.Info("cdp: setBreakpoint by scriptId", "scriptId", scriptId, "line", line, "cdpLine", line-1)
	params := map[string]any{
		"scriptId":   scriptId,
		"lineNumber": line - 1, // CDP is 0-based
	}
	if condition != "" {
		params["condition"] = condition
	}
	resp, err := c.sendAndWait("Debugger.setBreakpoint", params, 5*time.Second)
	if err != nil {
		return err
	}
	if result, ok := resp["result"].(map[string]any); ok {
		slog.Info("cdp: breakpoint set (scriptId)", "result", result)
	}
	return nil
}

// Disconnect terminates debugging.
// Sends Debugger.disable so Node.js exits cleanly instead of
// waiting for the debugger to disconnect.
func (c *CDPClient) Disconnect() error {
	_, _ = c.sendAndWait("Debugger.disable", nil, 2*time.Second)
	_, _ = c.sendAndWait("Runtime.disable", nil, 2*time.Second)
	return c.Close()
}

// readLoop reads CDP messages and dispatches events + responses.
func (c *CDPClient) readLoop() {
	defer close(c.events)
	defer func() {
		// Signal done so sendAndWait returns immediately instead of waiting for timeout
		select {
		case <-c.done:
		default:
			close(c.done)
		}
	}()

	for {
		select {
		case <-c.done:
			return
		default:
		}

		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			slog.Debug("cdp: readLoop ended", "err", err)
			return
		}

		var msg map[string]any
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}

		// CDP events have "method" field, responses have "id" field
		if method, ok := msg["method"].(string); ok {
			if method != "Debugger.scriptParsed" {
				slog.Info("cdp: event", "method", method, "raw", string(raw))
			}
			c.handleCDPEvent(method, msg)
		} else if id, ok := msg["id"]; ok {
			// Check if someone is waiting for this response
			idInt := intFromAny(id)
			if ch, loaded := c.pending.LoadAndDelete(idInt); loaded {
				respCh := ch.(chan map[string]any)
				respCh <- msg
			} else {
				c.handleCDPResponse(msg)
			}
		}
	}
}

func (c *CDPClient) handleCDPEvent(method string, msg map[string]any) {
	params, _ := msg["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}

	var de DebugEvent

	switch method {
	case "Debugger.scriptParsed":
		// Track script URL → scriptId for potential scriptId-based breakpoints
		url, _ := params["url"].(string)
		scriptId, _ := params["scriptId"].(string)
		if url != "" && scriptId != "" {
			c.scripts.Store(url, scriptId)
			c.scriptURLs.Store(scriptId, url)
			slog.Info("cdp: scriptParsed", "url", url, "scriptId", scriptId)
		}
		return // don't emit as DebugEvent

	case "Debugger.paused":
		// Spawn goroutine to fetch variables (requires sendAndWait which would deadlock in readLoop)
		go c.handlePausedEvent(params)
		return // event emitted from goroutine

	case "Debugger.resumed":
		de = DebugEvent{Type: "continued"}

	case "Runtime.consoleAPICalled":
		callType, _ := params["type"].(string)
		stream := "stdout"
		if callType == "error" || callType == "warn" {
			stream = "stderr"
		}
		args, _ := params["args"].([]any)
		var text string
		for _, a := range args {
			arg, ok := a.(map[string]any)
			if !ok {
				continue
			}
			if val, ok := arg["value"]; ok {
				if text != "" {
					text += " "
				}
				text += fmt.Sprintf("%v", val)
			}
		}
		// Detect tagged sandbox output (final result from output() call)
		const resultPrefix = "__SANDBOX_RESULT:"
		if strings.HasPrefix(text, resultPrefix) {
			de = DebugEvent{
				Type: "result",
				Data: strings.TrimPrefix(text, resultPrefix),
			}
		} else {
			de = DebugEvent{
				Type:   "output",
				Stream: stream,
				Data:   text + "\n",
			}
		}

	case "Runtime.executionContextDestroyed", "Inspector.detached":
		de = DebugEvent{Type: "terminated"}

	default:
		return
	}

	select {
	case c.events <- de:
	case <-c.done:
	}
}

func (c *CDPClient) handleCDPResponse(msg map[string]any) {
	result, _ := msg["result"].(map[string]any)
	if result == nil {
		return
	}

	if r, ok := result["result"].(map[string]any); ok {
		if val, ok := r["value"]; ok {
			de := DebugEvent{
				Type: "evaluate",
				Data: fmt.Sprintf("%v", val),
			}
			select {
			case c.events <- de:
			case <-c.done:
			}
		} else if desc, ok := r["description"].(string); ok {
			de := DebugEvent{
				Type: "evaluate",
				Data: desc,
			}
			select {
			case c.events <- de:
			case <-c.done:
			}
		}
	}
}

func cdpReasonToDAP(reason string) string {
	switch reason {
	case "other":
		return "breakpoint"
	case "debugCommand":
		return "pause"
	case "exception", "assert":
		return "exception"
	default:
		return reason
	}
}

// handlePausedEvent processes a Debugger.paused CDP event in a separate goroutine.
// It fetches variable values via Runtime.getProperties (which needs sendAndWait,
// so it can't run in the readLoop goroutine without deadlocking).
func (c *CDPClient) handlePausedEvent(params map[string]any) {
	reason, _ := params["reason"].(string)
	de := DebugEvent{
		Type:   "stopped",
		Reason: cdpReasonToDAP(reason),
	}

	// Extract exception description when paused on exception
	if reason == "exception" || reason == "assert" {
		if data, ok := params["data"].(map[string]any); ok {
			if desc, ok := data["description"].(string); ok {
				de.Data = desc
			} else if className, ok := data["className"].(string); ok {
				de.Data = className
			}
		}
	}

	frames, _ := params["callFrames"].([]any)
	for _, f := range frames {
		frame, ok := f.(map[string]any)
		if !ok {
			continue
		}
		loc, _ := frame["location"].(map[string]any)
		lineNum := intFromAny(loc["lineNumber"]) + 1 // CDP is 0-based
		colNum := intFromAny(loc["columnNumber"]) + 1
		funcName, _ := frame["functionName"].(string)
		if funcName == "" {
			funcName = "(anonymous)"
		}

		sf := StackFrame{
			Name:   funcName,
			Line:   lineNum,
			Column: colNum,
		}
		if url, ok := frame["url"].(string); ok && url != "" {
			sf.Source = &Source{Path: url, Name: url}
		}
		de.CallStack = append(de.CallStack, sf)
	}

	// Set line only if paused in user_script.js (not runner.js or Node internals).
	// Walk call stack to find first frame in user_script.js.
	if len(frames) > 0 {
		for _, f := range frames {
			frame, ok := f.(map[string]any)
			if !ok {
				continue
			}
			loc, _ := frame["location"].(map[string]any)
			scriptId, _ := loc["scriptId"].(string)
			if scriptId != "" {
				if url, ok := c.scriptURLs.Load(scriptId); ok {
					if strings.Contains(url.(string), "user_script") {
						de.Line = intFromAny(loc["lineNumber"]) + 1
						break
					}
				}
			}
		}
	}

	// Extract callFrameId from first frame (for evaluate expressions)
	if len(frames) > 0 {
		firstFrame, _ := frames[0].(map[string]any)
		if firstFrame != nil {
			de.CallFrameId, _ = firstFrame["callFrameId"].(string)
		}
	}

	// Fetch variable values via Runtime.getProperties for each non-global scope
	if len(frames) > 0 {
		firstFrame, _ := frames[0].(map[string]any)
		if firstFrame != nil {
			if scopeChain, ok := firstFrame["scopeChain"].([]any); ok {
				de.Variables = c.fetchScopeVariables(scopeChain)
			}
		}
	}

	slog.Info("cdp: paused", "reason", de.Reason, "line", de.Line,
		"frames", len(de.CallStack), "vars", len(de.Variables),
		"callFrameId", de.CallFrameId)

	select {
	case c.events <- de:
	case <-c.done:
	}
}

// fetchScopeVariables calls Runtime.getProperties for each non-global scope
// to get actual variable names and values.
func (c *CDPClient) fetchScopeVariables(scopeChain []any) []Variable {
	var vars []Variable
	for _, s := range scopeChain {
		scope, ok := s.(map[string]any)
		if !ok {
			continue
		}
		scopeType, _ := scope["type"].(string)
		if scopeType == "global" || scopeType == "with" {
			continue // skip global and with scopes (too noisy)
		}
		obj, _ := scope["object"].(map[string]any)
		if obj == nil {
			continue
		}
		objectId, _ := obj["objectId"].(string)
		if objectId == "" {
			continue
		}

		resp, err := c.sendAndWait("Runtime.getProperties", map[string]any{
			"objectId":      objectId,
			"ownProperties": true,
		}, 3*time.Second)
		if err != nil {
			slog.Warn("cdp: getProperties failed", "scope", scopeType, "err", err)
			continue
		}

		result, _ := resp["result"].(map[string]any)
		if result == nil {
			continue
		}
		props, _ := result["result"].([]any)
		for _, p := range props {
			prop, ok := p.(map[string]any)
			if !ok {
				continue
			}
			name, _ := prop["name"].(string)
			if name == "" || strings.HasPrefix(name, "__") {
				continue // skip internal props
			}

			valObj, _ := prop["value"].(map[string]any)
			if valObj == nil {
				continue
			}
			typ, _ := valObj["type"].(string)
			var value string
			switch typ {
			case "undefined":
				value = "undefined"
			case "object", "function":
				if desc, ok := valObj["description"].(string); ok {
					value = desc
				} else {
					value = fmt.Sprintf("[%s]", typ)
				}
			default:
				// string, number, boolean
				if v, ok := valObj["value"]; ok {
					value = fmt.Sprintf("%v", v)
				}
			}
			v := Variable{
				Name:  name,
				Value: value,
				Type:  typ,
			}
			// Store objectId for expandable types so frontend can request children
			if typ == "object" || typ == "function" {
				if oid, ok := valObj["objectId"].(string); ok {
					v.ObjectId = oid
				}
			}
			vars = append(vars, v)
		}
	}
	return vars
}
