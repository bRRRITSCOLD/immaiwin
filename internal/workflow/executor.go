package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/news"
	"github.com/dop251/goja"
	"go.mongodb.org/mongo-driver/v2/bson"
)

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
	HTTPClient *http.Client
	DB         RawUpserter
	Pub        Publisher
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
func (e *WorkflowExecutor) Run(ctx context.Context, wf Workflow, stopAt string) ([]StepResult, error) {
	byID := make(map[string]Node, len(wf.Nodes))
	for _, n := range wf.Nodes {
		byID[n.ID] = n
	}

	adj := make(map[string][]adjEntry, len(wf.Edges))
	for _, edge := range wf.Edges {
		adj[edge.Source] = append(adj[edge.Source], adjEntry{
			targetID:     edge.Target,
			sourceHandle: strings.ToLower(edge.SourceHandle),
		})
	}

	// body nodes — full subgraph reachable via "item" edges from for_each nodes.
	// These are skipped by main BFS; runForEach executes them per-item instead.
	forEachBodies := buildForEachBodies(wf.Nodes, adj)

	type queueItem struct {
		nodeID string
		input  any
	}
	var queue []queueItem
	for _, n := range wf.Nodes {
		if n.Type == NodeTypeTrigger {
			queue = append(queue, queueItem{nodeID: n.ID, input: nil})
		}
	}

	visited := make(map[string]bool)
	var results []StepResult
	wfCtx := make(runCtx)
	params := wf.Params
	if params == nil {
		params = map[string]string{}
	}

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
			if et.sourceHandle == handle || et.sourceHandle == "" {
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
		return nil, nil
	case NodeTypeHTTPFetch:
		return e.runHTTPFetch(ctx, data, input, wfCtx)
	case NodeTypeJSTransform:
		return runJSTransform(data, input, wfCtx, params)
	case NodeTypeForEach:
		return nil, fmt.Errorf("for_each dispatched via runNode — use runForEach instead")
	case NodeTypeMongoUpsert:
		return e.runMongoUpsert(ctx, data, input)
	case NodeTypeRedisPublish:
		return e.runRedisPublish(ctx, data, input)
	case NodeTypeNotify:
		return runNotify(data, input)
	default:
		return nil, fmt.Errorf("unknown node type: %s", node.Type)
	}
}

// runHTTPFetch performs an HTTP GET.
// URL supports {{input.FIELD}} and {{context.stepName.input.FIELD}} / {{context.stepName.output.FIELD}} templates.
func (e *WorkflowExecutor) runHTTPFetch(_ context.Context, data map[string]any, input any, wfCtx runCtx) (any, error) {
	rawURL, _ := data["url"].(string)
	if rawURL == "" {
		return nil, fmt.Errorf("http_fetch: url is required")
	}
	rawURL = applyTemplate(rawURL, input, wfCtx)

	client := e.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("http_fetch: build request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; immaiwin-scraper/1.0)")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http_fetch: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("http_fetch: status %d from %s", resp.StatusCode, rawURL)
	}

	return map[string]any{
		"ok":     true,
		"status": resp.StatusCode,
		"body":   string(body),
	}, nil
}

// runJSTransform runs user-supplied sync JS against input.
// Script is wrapped: (function(input){ USER_SCRIPT })(input) so top-level return works.
//
// Globals available to scripts:
//
//	input                      — output of the previous node
//	context.stepName.input     — what the named step received
//	context.stepName.output    — what the named step produced
//	context.stepName.item      — current element (for_each body only)
//	params                     — workflow-level parameter map (params.key)
func runJSTransform(data map[string]any, input any, wfCtx runCtx, params map[string]string) (any, error) {
	script, _ := data["script"].(string)
	if script == "" {
		return input, nil // pass-through if no script
	}

	vm := goja.New()
	if err := news.SetTransformBindings(vm); err != nil {
		return nil, fmt.Errorf("js_transform: setup: %w", err)
	}
	if err := vm.Set("input", input); err != nil {
		return nil, fmt.Errorf("js_transform: set input: %w", err)
	}
	if err := vm.Set("context", wfCtxToJS(wfCtx)); err != nil {
		return nil, fmt.Errorf("js_transform: set context: %w", err)
	}
	if err := vm.Set("params", params); err != nil {
		return nil, fmt.Errorf("js_transform: set params: %w", err)
	}

	wrapped := fmt.Sprintf("(function(input) { %s })(input)", script)
	result, err := vm.RunString(wrapped)
	if err != nil {
		return nil, fmt.Errorf("js_transform: runtime: %w", err)
	}
	return result.Export(), nil
}

// wfCtxToJS converts runCtx to map[string]any with lowercase keys so goja
// exposes context.stepName.input / .output / .item (not .Input / .Output / .Item).
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
// The "script" key is skipped — JS transforms access params via the params global instead.
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
func (e *WorkflowExecutor) runMongoUpsert(ctx context.Context, data map[string]any, input any) (any, error) {
	collection, _ := data["collection"].(string)
	filterField, _ := data["filter_field"].(string)

	if collection == "" {
		return nil, fmt.Errorf("mongo_upsert: collection is required")
	}
	if filterField == "" {
		return nil, fmt.Errorf("mongo_upsert: filter_field is required")
	}
	if e.DB == nil {
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
	_, ins, err := e.DB.UpsertRaw(ctx, collection, filter, update, true)
	if err != nil {
		return nil, fmt.Errorf("mongo_upsert: %w", err)
	}

	return map[string]any{
		"upserted": ins > 0,
		"input":    input,
	}, nil
}

// runRedisPublish publishes the JSON-serialised input to a Redis channel.
func (e *WorkflowExecutor) runRedisPublish(ctx context.Context, data map[string]any, input any) (any, error) {
	channel, _ := data["channel"].(string)
	if channel == "" {
		return nil, fmt.Errorf("redis_publish: channel is required")
	}
	if e.Pub == nil {
		return nil, fmt.Errorf("redis_publish: no publisher configured")
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("redis_publish: marshal: %w", err)
	}
	if err := e.Pub.Publish(ctx, channel, payload); err != nil {
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

