package workflow

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/sandbox"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// defaultMaxResponseBytes caps http_request response body size when the node
// doesn't override it. 10 MiB matches typical document/API responses while
// preventing runaway memory.
const defaultMaxResponseBytes int64 = 10 * 1024 * 1024

// Publisher broadcasts serialised payloads to a named channel.
type Publisher interface {
	Publish(ctx context.Context, channel string, payload []byte) error
}

// RawUpserter persists arbitrary documents to a named collection.
type RawUpserter interface {
	UpsertRaw(ctx context.Context, collection string, filter, update bson.M, upsert bool) (matched, inserted int64, err error)
}

// StepResult holds the outcome of a single node execution.
type StepResult struct {
	NodeID   string   `json:"node_id"`
	NodeType NodeType `json:"node_type"`
	Output   any      `json:"output,omitempty"`
	Error    string   `json:"error,omitempty"`
}

// StepContext holds the input, output, and (for for_each) current item of a named step.
//
// For regular nodes:   Input = what the node received; Output = what it produced.
// For for_each nodes:  Input = the full array; Item = the current iteration element.
//                      Output is only populated after all iterations complete (not useful in body).
type StepContext struct {
	Input  any `json:"input"`
	Output any `json:"output"`
	Item   any `json:"item,omitempty"`
}

// runCtx is a per-run map from step name → StepContext.
// JS transforms receive this as the "context" global.
type runCtx map[string]StepContext

// WorkflowExecutor runs a Workflow graph node by node.
type WorkflowExecutor struct {
	HTTPClient   *http.Client
	DB           RawUpserter
	Pub          Publisher
	ConnResolver *ConnectionResolver
	SandboxRT    sandbox.Runtime
	// AI agent dependencies (optional — agent nodes error if unset).
	Memory  AgentMemory      // chat memory backend
	RunRepo WorkflowRunStore // for run persistence + agent traces
}

// runEnv carries per-run graph context that the agent loop needs but
// other node handlers don't. Stored in ctx via runEnvKey rather than
// expanding every handler's signature.
type runEnv struct {
	wf       *Workflow
	byID     map[string]Node
	adj      map[string][]adjEntry
	runID    string // WorkflowRun.ID for trace persistence; empty if no RunRepo
	tenantID string // "default" until multi-tenant
}

type runEnvKeyT struct{}

var runEnvKey runEnvKeyT

func envFromCtx(ctx context.Context) (*runEnv, bool) {
	v, ok := ctx.Value(runEnvKey).(*runEnv)
	return v, ok
}

// adjEntry is one outgoing edge from a node.
type adjEntry struct {
	targetID     string
	sourceHandle string
}

// Run executes a workflow starting from all trigger nodes using BFS.
// for_each nodes iterate input arrays; their "item" edge targets are run
// once per element (not by the main BFS).
// If stopAt is non-empty, execution halts after the node with that ID executes,
// returning partial results (useful for debug/breakpoint runs).
// Optional initialInput is injected as trigger node output (used by event-driven triggers like RabbitMQ).
func (e *WorkflowExecutor) Run(ctx context.Context, wf Workflow, stopAt string, initialInput ...any) ([]StepResult, error) {
	byID := make(map[string]Node, len(wf.Nodes))
	for _, n := range wf.Nodes {
		byID[n.ID] = n
	}

	adj := make(map[string][]adjEntry, len(wf.Edges))
	for _, edge := range wf.Edges {
		h := strings.ToLower(edge.SourceHandle)
		// Prefer paletteType for all edges (not just dh-*); legacy edges
		// without paletteType fall through to sourceHandle for backwards compat.
		if pt, ok := edge.Data["paletteType"].(string); ok && pt != "" {
			h = strings.ToLower(pt)
		}
		adj[edge.Source] = append(adj[edge.Source], adjEntry{
			targetID:     edge.Target,
			sourceHandle: h,
		})
	}

	// body nodes — full subgraph reachable via "item" edges from for_each nodes.
	// These are skipped by main BFS; runForEach executes them per-item instead.
	forEachBodies := buildForEachBodies(wf.Nodes, adj)

	type queueItem struct {
		nodeID string
		input  any
	}
	// When initialInput is provided (e.g. from RabbitMQ message), trigger nodes
	// produce that value as output instead of nil.
	var triggerOutput any
	if len(initialInput) > 0 {
		triggerOutput = initialInput[0]
	}

	var queue []queueItem
	for _, n := range wf.Nodes {
		if n.Type == NodeTypeTrigger {
			queue = append(queue, queueItem{nodeID: n.ID, input: triggerOutput})
		}
	}

	visited := make(map[string]bool)
	var results []StepResult
	wfCtx := make(runCtx)
	params := wf.Params
	if params == nil {
		params = map[string]string{}
	}

	// Stash run env so agent loop (and any future node type that needs
	// graph access) can find tool-edges + node lookup tables without us
	// expanding every handler's signature.
	env := &runEnv{
		wf:       &wf,
		byID:     byID,
		adj:      adj,
		tenantID: "default",
	}
	ctx = context.WithValue(ctx, runEnvKey, env)

	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]
		if visited[item.nodeID] || forEachBodies[item.nodeID] {
			continue
		}
		visited[item.nodeID] = true

		node, ok := byID[item.nodeID]
		if !ok {
			continue
		}

		var (
			output       any
			err          error
			extraResults []StepResult
		)

		if node.Type == NodeTypeForEach {
			output, extraResults, err = e.runForEach(ctx, node, item.input, adj, byID, wfCtx, params, stopAt)
			results = append(results, extraResults...)
		} else {
			output, err = e.runNode(ctx, node, item.input, wfCtx, params)
		}

		// Populate context for named nodes
		if name, _ := node.Data["name"].(string); name != "" {
			wfCtx[name] = StepContext{Input: item.input, Output: output}
		}

		sr := StepResult{NodeID: node.ID, NodeType: node.Type, Output: output}
		handle := "success"
		if err != nil {
			sr.Error = err.Error()
			handle = "error"
			slog.Warn("workflow: node error", "node", node.ID, "type", node.Type, "err", err)
		}
		results = append(results, sr)

		if stopAt != "" && node.ID == stopAt {
			return results, nil
		}

		for _, et := range adj[item.nodeID] {
			if et.sourceHandle == "item" {
				continue // for_each body; not traversed by main BFS
			}
			if et.sourceHandle == handle || et.sourceHandle == "" || et.sourceHandle == "start" {
				if !visited[et.targetID] {
					queue = append(queue, queueItem{nodeID: et.targetID, input: output})
				}
			}
		}
	}

	return results, nil
}

// buildForEachBodies BFS-marks all nodes reachable via "item" edges from for_each nodes.
// These nodes are the loop body and are skipped by the main BFS.
func buildForEachBodies(nodes []Node, adj map[string][]adjEntry) map[string]bool {
	bodies := make(map[string]bool)
	for _, n := range nodes {
		if n.Type != NodeTypeForEach {
			continue
		}
		for _, et := range adj[n.ID] {
			if et.sourceHandle != "item" {
				continue
			}
			// BFS from item target to mark full body subgraph
			q := []string{et.targetID}
			for len(q) > 0 {
				id := q[0]
				q = q[1:]
				if bodies[id] {
					continue
				}
				bodies[id] = true
				for _, next := range adj[id] {
					if !bodies[next.targetID] {
						q = append(q, next.targetID)
					}
				}
			}
		}
	}
	return bodies
}

// runForEach iterates input as an array and runs the full body chain
// (starting from each "item" target) once per element.
// Returns aggregated step results and a slice of per-item final outputs.
func (e *WorkflowExecutor) runForEach(
	ctx context.Context,
	node Node,
	input any,
	adj map[string][]adjEntry,
	byID map[string]Node,
	wfCtx runCtx,
	params map[string]string,
	stopAt string,
) (any, []StepResult, error) {
	items := toSlice(input)

	var itemTargetIDs []string
	for _, et := range adj[node.ID] {
		if et.sourceHandle == "item" {
			itemTargetIDs = append(itemTargetIDs, et.targetID)
		}
	}

	var allResults []StepResult
	var outputs []any

	forEachName, _ := node.Data["name"].(string)

	for _, item := range items {
		// clone parent context per iteration so body steps don't bleed across iterations
		iterCtx := make(runCtx, len(wfCtx))
		for k, v := range wfCtx {
			iterCtx[k] = v
		}
		// expose current iteration element via context if for_each is named
		// body nodes access it as: context.stepName.item.field
		if forEachName != "" {
			iterCtx[forEachName] = StepContext{Input: input, Item: item}
		}

		for _, startID := range itemTargetIDs {
			chainResults, lastOut := e.runBodyChain(ctx, startID, item, adj, byID, iterCtx, params, stopAt)
			allResults = append(allResults, chainResults...)
			if len(chainResults) > 0 && chainResults[len(chainResults)-1].Error == "" {
				outputs = append(outputs, lastOut)
			}
		}
	}

	return outputs, allResults, nil
}

// runBodyChain executes a linear chain starting from startID with the given input.
// Follows success/error edges within the body; does not re-enter main BFS nodes.
func (e *WorkflowExecutor) runBodyChain(
	ctx context.Context,
	startID string,
	input any,
	adj map[string][]adjEntry,
	byID map[string]Node,
	wfCtx runCtx,
	params map[string]string,
	stopAt string,
) ([]StepResult, any) {
	var results []StepResult
	currentID := startID
	currentInput := input
	visited := make(map[string]bool)

	for currentID != "" {
		if visited[currentID] {
			break
		}
		visited[currentID] = true

		node, ok := byID[currentID]
		if !ok {
			break
		}

		output, err := e.runNode(ctx, node, currentInput, wfCtx, params)

		// Populate context for named body nodes
		if name, _ := node.Data["name"].(string); name != "" {
			wfCtx[name] = StepContext{Input: currentInput, Output: output}
		}

		sr := StepResult{NodeID: node.ID, NodeType: node.Type, Output: output}
		handle := "success"
		if err != nil {
			sr.Error = err.Error()
			handle = "error"
			slog.Warn("for_each body: node error", "node", node.ID, "err", err)
		}
		results = append(results, sr)
		if stopAt != "" && node.ID == stopAt {
			return results, output
		}
		currentInput = output

		currentID = ""
		for _, et := range adj[node.ID] {
			if et.sourceHandle == handle || et.sourceHandle == "" {
				currentID = et.targetID
				break
			}
		}
	}

	var lastOut any
	if len(results) > 0 {
		lastOut = results[len(results)-1].Output
	}
	return results, lastOut
}

// runNode dispatches execution to the appropriate handler for node.Type.
// Params are resolved in all string data fields before dispatch (except "script").
func (e *WorkflowExecutor) runNode(ctx context.Context, node Node, input any, wfCtx runCtx, params map[string]string) (any, error) {
	data := applyParamsToData(node.Data, params)
	switch node.Type {
	case NodeTypeTrigger:
		return input, nil // pass-through; initialInput flows via the queue item
	case NodeTypeHTTPRequest:
		return e.runHTTPRequest(ctx, data, input, wfCtx)
	case NodeTypeSandboxScript:
		return e.runSandboxScript(ctx, data, input, wfCtx, params)
	case NodeTypeForEach:
		return nil, fmt.Errorf("for_each dispatched via runNode — use runForEach instead")
	case NodeTypeMongoUpsert:
		return e.runMongoUpsert(ctx, data, input)
	case NodeTypeRedisPublish:
		return e.runRedisPublish(ctx, data, input)
	case NodeTypeNotify:
		return runNotify(data, input)
	case NodeTypeAIAgent:
		return e.runAIAgent(ctx, node, data, input, wfCtx, params)
	default:
		return nil, fmt.Errorf("unknown node type: %s", node.Type)
	}
}

// runHTTPRequest performs an arbitrary HTTP request with full Go http.Client
// parity. Strings (url, header values, query values, body, bearer_token,
// basic_auth_*) support {{input.FIELD}} / {{context.stepName.*.FIELD}} templates.
//
// Data fields:
//
//	url                       string  required
//	method                    string  default "GET"
//	headers                   map[string]string
//	query                     map[string]string  appended to URL query string
//	body                      string             raw request body
//	body_json                 any                marshalled to JSON; sets Content-Type
//	body_form                 map[string]string  form-urlencoded; sets Content-Type
//	timeout_seconds           number  default 30
//	follow_redirects          bool    default true
//	max_redirects             number  default 10
//	basic_auth_username       string
//	basic_auth_password       string
//	bearer_token              string  sent as "Authorization: Bearer ..."
//	user_agent                string  overrides default User-Agent
//	tls_insecure_skip_verify  bool    default false
//	parse_json                bool    parse response body as JSON into output.json
//	max_response_bytes        number  default 10 MiB; 0 = unlimited
//	accept_any_status         bool    default false; when true, non-2xx is success
//
// Output:
//
//	ok           bool
//	status       int
//	status_text  string
//	headers      map[string][]string
//	body         string
//	json         any (only when parse_json=true and decode succeeds)
func (e *WorkflowExecutor) runHTTPRequest(ctx context.Context, data map[string]any, input any, wfCtx runCtx) (any, error) {
	rawURL, _ := data["url"].(string)
	if rawURL == "" {
		return nil, fmt.Errorf("http_request: url is required")
	}
	rawURL = applyTemplate(rawURL, input, wfCtx)

	method := strings.ToUpper(strings.TrimSpace(getStringData(data, "method")))
	if method == "" {
		method = http.MethodGet
	}

	// Build URL with query merge
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("http_request: parse url: %w", err)
	}
	if qm := stringMap(data["query"]); len(qm) > 0 {
		q := parsedURL.Query()
		for k, v := range qm {
			q.Set(k, applyTemplate(v, input, wfCtx))
		}
		parsedURL.RawQuery = q.Encode()
	}

	// Build body
	var (
		bodyReader  io.Reader
		contentType string
	)
	if bj, ok := data["body_json"]; ok && bj != nil {
		b, err := json.Marshal(bj)
		if err != nil {
			return nil, fmt.Errorf("http_request: marshal body_json: %w", err)
		}
		// Apply templates only to string-valued bodies (body_json is structured)
		bodyReader = bytes.NewReader(b)
		contentType = "application/json"
	} else if bf := stringMap(data["body_form"]); len(bf) > 0 {
		form := url.Values{}
		for k, v := range bf {
			form.Set(k, applyTemplate(v, input, wfCtx))
		}
		bodyReader = strings.NewReader(form.Encode())
		contentType = "application/x-www-form-urlencoded"
	} else if raw, _ := data["body"].(string); raw != "" {
		bodyReader = strings.NewReader(applyTemplate(raw, input, wfCtx))
	}

	req, err := http.NewRequestWithContext(ctx, method, parsedURL.String(), bodyReader)
	if err != nil {
		return nil, fmt.Errorf("http_request: build request: %w", err)
	}

	// Headers
	if hm := stringMap(data["headers"]); len(hm) > 0 {
		for k, v := range hm {
			req.Header.Set(k, applyTemplate(v, input, wfCtx))
		}
	}
	if contentType != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", contentType)
	}
	if ua := getStringData(data, "user_agent"); ua != "" {
		req.Header.Set("User-Agent", applyTemplate(ua, input, wfCtx))
	} else if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; immaiwin/1.0)")
	}

	// Auth
	if bt := getStringData(data, "bearer_token"); bt != "" {
		req.Header.Set("Authorization", "Bearer "+applyTemplate(bt, input, wfCtx))
	}
	if u := getStringData(data, "basic_auth_username"); u != "" {
		p := getStringData(data, "basic_auth_password")
		req.SetBasicAuth(applyTemplate(u, input, wfCtx), applyTemplate(p, input, wfCtx))
	}

	// Client (per-request to honour timeout/redirects/TLS overrides)
	client := buildHTTPClient(e.HTTPClient, data)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http_request: %w", err)
	}
	defer resp.Body.Close()

	maxBytes := defaultMaxResponseBytes
	if v, ok := numberData(data, "max_response_bytes"); ok {
		maxBytes = int64(v)
	}
	var bodyBytes []byte
	if maxBytes > 0 {
		bodyBytes, err = io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
		if err != nil {
			return nil, fmt.Errorf("http_request: read body: %w", err)
		}
		if int64(len(bodyBytes)) > maxBytes {
			return nil, fmt.Errorf("http_request: response body exceeds max_response_bytes (%d)", maxBytes)
		}
	} else {
		bodyBytes, err = io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("http_request: read body: %w", err)
		}
	}

	ok2xx := resp.StatusCode >= 200 && resp.StatusCode < 300
	acceptAny, _ := data["accept_any_status"].(bool)
	if !ok2xx && !acceptAny {
		return nil, fmt.Errorf("http_request: status %d from %s", resp.StatusCode, parsedURL.String())
	}

	out := map[string]any{
		"ok":          ok2xx,
		"status":      resp.StatusCode,
		"status_text": resp.Status,
		"headers":     map[string][]string(resp.Header),
		"body":        string(bodyBytes),
	}
	if parse, _ := data["parse_json"].(bool); parse && len(bodyBytes) > 0 {
		var parsed any
		if jerr := json.Unmarshal(bodyBytes, &parsed); jerr == nil {
			out["json"] = parsed
		} else {
			out["json_error"] = jerr.Error()
		}
	}
	return out, nil
}

// buildHTTPClient produces an http.Client honouring per-request overrides:
// timeout, redirect policy, TLS skip-verify. Falls back to base when no
// overrides apply (preserving any shared transport).
func buildHTTPClient(base *http.Client, data map[string]any) *http.Client {
	timeout := 30 * time.Second
	if v, ok := numberData(data, "timeout_seconds"); ok && v > 0 {
		timeout = time.Duration(v) * time.Second
	}

	follow := true
	if v, ok := data["follow_redirects"].(bool); ok {
		follow = v
	}
	maxRedirects := 10
	if v, ok := numberData(data, "max_redirects"); ok && v > 0 {
		maxRedirects = int(v)
	}

	insecure, _ := data["tls_insecure_skip_verify"].(bool)

	// If no TLS override and base client present, reuse its transport with our
	// timeout + redirect policy. Otherwise build a fresh transport.
	var transport http.RoundTripper
	if insecure {
		transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // user-opt-in
		}
	} else if base != nil && base.Transport != nil {
		transport = base.Transport
	}

	c := &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
	if !follow {
		c.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}
	} else {
		c.CheckRedirect = func(_ *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errors.New("max redirects exceeded")
			}
			return nil
		}
	}
	return c
}

// stringMap coerces an arbitrary value into map[string]string. Accepts
// map[string]string and map[string]any (values stringified). Returns nil on
// type mismatch.
func stringMap(v any) map[string]string {
	switch m := v.(type) {
	case map[string]string:
		return m
	case map[string]any:
		out := make(map[string]string, len(m))
		for k, val := range m {
			out[k] = fmt.Sprint(val)
		}
		return out
	}
	return nil
}

// numberData reads a numeric data field (accepts float64/int variants).
func numberData(data map[string]any, key string) (float64, bool) {
	v, ok := data[key]
	if !ok {
		return 0, false
	}
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

// wfCtxToJS converts runCtx to map[string]any with lowercase keys so sandbox scripts
// access context.stepName.input / .output / .item (not .Input / .Output / .Item).
func wfCtxToJS(wfCtx runCtx) map[string]any {
	js := make(map[string]any, len(wfCtx))
	for name, sc := range wfCtx {
		entry := map[string]any{
			"input":  sc.Input,
			"output": sc.Output,
		}
		if sc.Item != nil {
			entry["item"] = sc.Item
		}
		js[name] = entry
	}
	return js
}

// applyParamsToData resolves {{params.key}} placeholders in all string data fields.
// The "script" key is skipped — sandbox scripts access params via the params global instead.
func applyParamsToData(data map[string]any, params map[string]string) map[string]any {
	if len(params) == 0 {
		return data
	}
	resolved := make(map[string]any, len(data))
	for k, v := range data {
		if k == "script" {
			resolved[k] = v // scripts use params global, not template substitution
			continue
		}
		if s, ok := v.(string); ok {
			for pk, pv := range params {
				s = strings.ReplaceAll(s, "{{params."+pk+"}}", pv)
			}
			resolved[k] = s
		} else {
			resolved[k] = v
		}
	}
	return resolved
}

// runMongoUpsert upserts a single document into the target collection.
// Use for_each upstream to iterate arrays; this node handles one item at a time.
// If data["connection_id"] is set and ConnResolver is available, uses that connection.
func (e *WorkflowExecutor) runMongoUpsert(ctx context.Context, data map[string]any, input any) (any, error) {
	collection, _ := data["collection"].(string)
	filterField, _ := data["filter_field"].(string)

	if collection == "" {
		return nil, fmt.Errorf("mongo_upsert: collection is required")
	}
	if filterField == "" {
		return nil, fmt.Errorf("mongo_upsert: filter_field is required")
	}

	// Resolve DB — connection_id → specific connection, empty → default
	db := e.DB
	if connID, _ := data["connection_id"].(string); connID != "" && e.ConnResolver != nil {
		resolved, err := e.ConnResolver.ResolveDB(ctx, connID)
		if err != nil {
			return nil, fmt.Errorf("mongo_upsert: %w", err)
		}
		db = resolved
	}
	if db == nil {
		return nil, fmt.Errorf("mongo_upsert: no DB configured")
	}

	item, ok := input.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("mongo_upsert: input must be a map (got %T); use for_each for arrays", input)
	}

	filterVal, ok := item[filterField]
	if !ok {
		return nil, fmt.Errorf("mongo_upsert: filter_field %q not found in input", filterField)
	}

	filter := bson.M{filterField: filterVal}
	update := bson.M{
		"$set":         item,
		"$setOnInsert": bson.M{"created_at": time.Now().UTC()},
	}
	_, ins, err := db.UpsertRaw(ctx, collection, filter, update, true)
	if err != nil {
		return nil, fmt.Errorf("mongo_upsert: %w", err)
	}

	return map[string]any{
		"upserted": ins > 0,
		"input":    input,
	}, nil
}

// runRedisPublish publishes the JSON-serialised input to a Redis channel.
// If data["connection_id"] is set and ConnResolver is available, uses that connection.
func (e *WorkflowExecutor) runRedisPublish(ctx context.Context, data map[string]any, input any) (any, error) {
	channel, _ := data["channel"].(string)
	if channel == "" {
		return nil, fmt.Errorf("redis_publish: channel is required")
	}

	// Resolve publisher — connection_id → specific connection, empty → default
	pub := e.Pub
	if connID, _ := data["connection_id"].(string); connID != "" && e.ConnResolver != nil {
		resolved, err := e.ConnResolver.ResolvePub(ctx, connID)
		if err != nil {
			return nil, fmt.Errorf("redis_publish: %w", err)
		}
		pub = resolved
	}
	if pub == nil {
		return nil, fmt.Errorf("redis_publish: no publisher configured")
	}

	payload, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("redis_publish: marshal: %w", err)
	}
	if err := pub.Publish(ctx, channel, payload); err != nil {
		return nil, fmt.Errorf("redis_publish: %w", err)
	}
	return map[string]any{"channel": channel, "published": true}, nil
}

// runNotify logs input and returns a message.
func runNotify(data map[string]any, input any) (any, error) {
	msg, _ := data["message"].(string)
	if msg == "" {
		msg = fmt.Sprint(input)
	}
	slog.Info("workflow notify", "message", msg, "input", input)
	return map[string]any{"message": msg}, nil
}

// applyTemplate replaces template placeholders in s.
//
// Supported patterns:
//
//	{{input.FIELD}}                   — field from immediate input
//	{{context.stepName.input.FIELD}}  — field from named step's input
//	{{context.stepName.output.FIELD}} — field from named step's output
//	{{context.stepName.item.FIELD}}   — current iteration element (for_each body only)
func applyTemplate(s string, input any, wfCtx runCtx) string {
	if m, ok := input.(map[string]any); ok {
		for k, v := range m {
			s = strings.ReplaceAll(s, "{{input."+k+"}}", fmt.Sprint(v))
		}
	}
	for name, sc := range wfCtx {
		if m, ok := sc.Input.(map[string]any); ok {
			for k, v := range m {
				s = strings.ReplaceAll(s, "{{context."+name+".input."+k+"}}", fmt.Sprint(v))
			}
		}
		if m, ok := sc.Output.(map[string]any); ok {
			for k, v := range m {
				s = strings.ReplaceAll(s, "{{context."+name+".output."+k+"}}", fmt.Sprint(v))
			}
		}
		if m, ok := sc.Item.(map[string]any); ok {
			for k, v := range m {
				s = strings.ReplaceAll(s, "{{context."+name+".item."+k+"}}", fmt.Sprint(v))
			}
		}
	}
	return s
}

// runSandboxScript executes user code in an isolated Docker container.
//
// Node data fields:
//
//	data.language   string  "javascript" | "python"
//	data.script     string  user code
//	data.timeout    float64 seconds (default 30)
//	data.mem_limit  float64 MB (default 128)
//	data.cpu_limit  float64 cores (default 0.5)
//	data.network    bool    allow outbound network (default false)
func (e *WorkflowExecutor) runSandboxScript(ctx context.Context, data map[string]any, input any, wfCtx runCtx, params map[string]string) (any, error) {
	if e.SandboxRT == nil {
		return nil, fmt.Errorf("sandbox_script: sandbox manager not configured")
	}

	lang, _ := data["language"].(string)
	if lang == "" {
		lang = "javascript"
	}
	script, _ := data["script"].(string)
	if script == "" {
		return input, nil // pass-through
	}

	timeout := 30 * time.Second
	if t, ok := data["timeout"].(float64); ok && t > 0 {
		timeout = time.Duration(t) * time.Second
	}
	memLimit := int64(sandbox.DefaultMemLimit)
	if m, ok := data["mem_limit"].(float64); ok && m > 0 {
		memLimit = int64(m) * 1024 * 1024
	}
	cpuLimit := sandbox.DefaultCPULimit
	if c, ok := data["cpu_limit"].(float64); ok && c > 0 {
		cpuLimit = c
	}
	network, _ := data["network"].(bool)
	customImage, _ := data["custom_image"].(string)

	var packages []string
	if pkgStr, ok := data["packages"].(string); ok && pkgStr != "" {
		for _, p := range strings.Split(pkgStr, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				packages = append(packages, p)
			}
		}
	}

	result, err := e.SandboxRT.Run(ctx, sandbox.RunRequest{
		Language: sandbox.Language(lang),
		Code:     script,
		Input:    input,
		Context:  wfCtxToJS(wfCtx),
		Params:   params,
		Timeout:  timeout,
		MemLimit: memLimit,
		CPULimit: cpuLimit,
		Network:  network,
		Image:    customImage,
		Packages: packages,
	})
	if err != nil {
		return nil, fmt.Errorf("sandbox_script: %w", err)
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("sandbox_script: exit code %d: %s", result.ExitCode, result.Stderr)
	}
	return result.Output, nil
}

// toSlice normalises input to []any. Single values become a one-element slice.
func toSlice(input any) []any {
	switch v := input.(type) {
	case []any:
		return v
	case nil:
		return nil
	default:
		return []any{v}
	}
}

