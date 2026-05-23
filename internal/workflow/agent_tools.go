package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/llm"
	"github.com/bRRRITSCOLD/burrow/internal/sandbox"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ToolCatalog holds the tools an agent has access to during one run.
// Tools come from three sources, in priority order if names collide:
//  1. Built-in tools (code_execute, …) — reserved namespace.
//  2. Skill-supplied tools (P1.11) — prefixed with namespace__name__.
//  3. Workflow-edge-bound nodes — opt-in via data.as_tool on target.
type ToolCatalog struct {
	defs       []llm.ToolDef
	handlers   map[string]ToolHandler
	// validators holds a compiled JSON-Schema per tool name. Populated
	// lazily in Add() from each tool's InputSchema. Tools with an
	// uncompilable schema are simply omitted from this map — Execute()
	// then falls through to permissive behaviour for that tool, matching
	// pre-validation semantics.
	validators map[string]*jsonschema.Schema
}

// ToolHandler executes a tool call and returns its observation. Returning
// an error sets is_error on the tool_result block; the agent loop
// continues so the LLM can react to the failure.
type ToolHandler func(ctx context.Context, args json.RawMessage) (string, error)

// NewToolCatalog returns an empty catalog.
func NewToolCatalog() *ToolCatalog {
	return &ToolCatalog{
		handlers:   make(map[string]ToolHandler),
		validators: make(map[string]*jsonschema.Schema),
	}
}

// Add registers a tool. Returns an error on duplicate name. Compiles the
// tool's InputSchema if non-empty so Execute() can validate LLM-supplied
// args before dispatch (catches hallucinated arg shapes early so the
// model gets a structured schema-violation error back instead of the
// underlying handler exploding on a bad type cast).
func (c *ToolCatalog) Add(def llm.ToolDef, handler ToolHandler) error {
	if _, dup := c.handlers[def.Name]; dup {
		return fmt.Errorf("tool catalog: duplicate name %q", def.Name)
	}
	c.defs = append(c.defs, def)
	c.handlers[def.Name] = handler
	if len(def.InputSchema) > 0 {
		if sch, err := compileToolSchema(def.InputSchema); err == nil {
			c.validators[def.Name] = sch
		} else {
			// Don't reject the tool — schema authoring is best-effort and
			// older as_tool defs may have a permissive shape that doesn't
			// strictly validate. Log + skip so dispatch falls through to
			// the unvalidated path for this tool only.
			slog.Warn("tool catalog: schema compile failed; tool will dispatch without arg validation",
				"name", def.Name, "err", err)
		}
	}
	return nil
}

// Defs returns the tool definitions in registration order, suitable for
// passing to llm.ChatRequest.Tools.
func (c *ToolCatalog) Defs() []llm.ToolDef { return c.defs }

// FilterAllowed restricts the catalog to tools whose names appear in
// `allowed`. Used to apply a per-agent authorization policy
// (`data.allowed_tools`) AFTER the catalog has been built from every
// source. Empty `allowed` is a no-op — caller decides the open-default
// semantics. Matching is exact (no globbing in v1); names that don't
// exist in the catalog are silently dropped, mirroring the lenient
// "ignore unknown" posture the rest of the agent loop already uses.
//
// Defense-in-depth: the LLM never sees filtered defs in its tool list,
// AND a hallucinated call to a filtered name falls through to Execute's
// unknown-tool error path so the model self-corrects without firing
// any handler.
func (c *ToolCatalog) FilterAllowed(allowed []string) {
	if len(allowed) == 0 {
		return
	}
	allowSet := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		allowSet[name] = struct{}{}
	}
	if len(allowSet) == 0 {
		return
	}
	keptDefs := c.defs[:0]
	for _, d := range c.defs {
		if _, ok := allowSet[d.Name]; ok {
			keptDefs = append(keptDefs, d)
			continue
		}
		// Drop matching handler + validator entries so a hallucinated
		// call to a denied tool falls through to Execute's unknown-tool
		// error path instead of silently dispatching.
		delete(c.handlers, d.Name)
		delete(c.validators, d.Name)
	}
	c.defs = keptDefs
}

// Execute looks up a handler by name and invokes it. Returns the
// observation text. Unknown tool returns an error with a clear message
// the LLM can correct from. When a compiled InputSchema exists for the
// tool, args are validated before dispatch and a structured violation
// message is returned to the LLM (no handler runs) so the model can
// self-correct in the next iter without burning external API calls /
// state-mutating side effects on bad args.
func (c *ToolCatalog) Execute(ctx context.Context, name string, args json.RawMessage) (string, error) {
	h, ok := c.handlers[name]
	if !ok {
		known := make([]string, 0, len(c.defs))
		for _, d := range c.defs {
			known = append(known, d.Name)
		}
		return "", fmt.Errorf("unknown tool %q; available: %s", name, strings.Join(known, ", "))
	}

	if sch, ok := c.validators[name]; ok {
		// Empty Anthropic / Ollama tool calls send no Input — treat as {}
		// so a tool whose schema declares no `required` properties still
		// passes. Schemas with `required` will fail validation here.
		validateArgs := args
		if len(validateArgs) == 0 {
			validateArgs = json.RawMessage("{}")
		}
		var decoded any
		dec := json.NewDecoder(bytes.NewReader(validateArgs))
		dec.UseNumber()
		if err := dec.Decode(&decoded); err != nil {
			// Short Go error keeps the agent loop's `\nerror: <…>` suffix
			// from duplicating the long observation message.
			return fmt.Sprintf("tool %s: invalid JSON in args: %s", name, err.Error()),
				fmt.Errorf("%w: invalid JSON in args", ErrToolValidation)
		}
		if err := sch.Validate(decoded); err != nil {
			msg := fmt.Sprintf("tool %s: input failed schema validation:\n%s\n\ninput received:\n%s",
				name, sanitizeSchemaErr(err.Error(), name), string(validateArgs))
			return msg, fmt.Errorf("%w: %s", ErrToolValidation, "schema validation failed")
		}
	}

	return h(ctx, args)
}

// compileToolSchema turns a raw JSON Schema into a compiled validator
// using the same library as the skills package. Pulled into its own
// helper so the catalog doesn't depend on the skills package (would
// create an import cycle: skills -> workflow for the registry).
//
// The resource URL is namespaced as `tool:<sanitized>` so the validator's
// error messages reference the tool by name instead of leaking the
// process's filesystem path (jsonschema/v6 resolves a bare resource ID
// relative to cwd, producing `file:///<cwd>/<id>` in errors).
func compileToolSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("decode schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	resource := "tool:" + schemaResourceID()
	if err := c.AddResource(resource, v); err != nil {
		return nil, fmt.Errorf("add schema: %w", err)
	}
	return c.Compile(resource)
}

// schemaResourceID returns a stable identifier for the validator's
// internal resource registry. Atomic counter so concurrent agent runs
// can't collide on the jsonschema compiler's resource map.
var schemaResourceCounter atomic.Uint64

func schemaResourceID() string {
	return fmt.Sprintf("schema_%d", schemaResourceCounter.Add(1))
}

// sanitizeSchemaErr strips the leaking `with 'tool:schema_N#'` prefix
// the v6 jsonschema library emits at the start of every error so the
// LLM-facing observation stays focused on the actual violation.
func sanitizeSchemaErr(msg, toolName string) string {
	const prefix = "jsonschema validation failed with '"
	if idx := strings.Index(msg, prefix); idx >= 0 {
		// Drop everything from the prefix up to the closing quote +
		// trailing newline. Replace with a clean per-tool header.
		end := strings.Index(msg[idx+len(prefix):], "'")
		if end >= 0 {
			tail := msg[idx+len(prefix)+end+1:]
			tail = strings.TrimLeft(tail, "\n")
			return fmt.Sprintf("schema for tool '%s' rejected the args:\n%s", toolName, tail)
		}
	}
	return msg
}

// --- Built-in tools ---

// codeExecuteSchema is the JSON-Schema for the built-in code_execute tool.
// Lives as a constant rather than a builder so it serializes the same
// every time (caching-friendly for LLM providers that support it).
var codeExecuteSchema = json.RawMessage(`{
  "type": "object",
  "required": ["language", "code"],
  "properties": {
    "language": {
      "type": "string",
      "enum": ["javascript", "python", "golang", "rust", "php"],
      "description": "Runtime to execute the code in. Each runs in an isolated gVisor sandbox."
    },
    "code": {
      "type": "string",
      "description": "Source code. Call output(value) to return a result. Use console.log/print for stderr logs."
    },
    "network": {
      "type": "boolean",
      "default": false,
      "description": "Allow outbound HTTP egress. Default false; enable only when the task requires fetching external data."
    }
  }
}`)

// builtinCodeExecuteTool returns the code_execute tool def + handler if a
// sandbox runtime is available; nil otherwise. Differentiator: every agent
// gets a 5-language gVisor-isolated compute substrate by default.
func builtinCodeExecuteTool(rt sandbox.Runtime) (llm.ToolDef, ToolHandler, bool) {
	if rt == nil {
		return llm.ToolDef{}, nil, false
	}
	def := llm.ToolDef{
		Name: "code_execute",
		Description: `Execute code in an isolated sandbox. Use when no other tool fits the task.
Available languages: javascript, python, golang, rust, php. Each runs gVisor-isolated.
The code MUST call output(<value>) to return a result; stderr captures logs.
Set network=true ONLY when the task requires HTTP egress.`,
		InputSchema: codeExecuteSchema,
	}
	handler := func(ctx context.Context, args json.RawMessage) (string, error) {
		var in struct {
			Language string `json:"language"`
			Code     string `json:"code"`
			Network  bool   `json:"network"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("code_execute: bad args: %w", err)
		}

		req := sandbox.RunRequest{
			Language: sandbox.Language(in.Language),
			Code:     in.Code,
			Network:  in.Network,
			Timeout:  60 * time.Second, // generous for agent-driven scripts
		}
		runCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()

		result, err := rt.Run(runCtx, req)
		if err != nil {
			return "", fmt.Errorf("code_execute: %w", err)
		}

		// Format observation. Prefer the parsed Output (script's output(...) call);
		// fall back to stdout if no structured output. Always include stderr for
		// log visibility.
		var sb strings.Builder
		if result.Output != nil {
			b, _ := json.Marshal(result.Output)
			sb.WriteString("output: ")
			sb.Write(b)
			sb.WriteString("\n")
		} else if strings.TrimSpace(result.Stdout) != "" {
			sb.WriteString("stdout: ")
			sb.WriteString(result.Stdout)
			sb.WriteString("\n")
		}
		if strings.TrimSpace(result.Stderr) != "" {
			sb.WriteString("stderr: ")
			sb.WriteString(result.Stderr)
			sb.WriteString("\n")
		}
		fmt.Fprintf(&sb, "exit_code: %d\nduration: %s", result.ExitCode, result.Duration)
		if result.ExitCode != 0 {
			return sb.String(), fmt.Errorf("code_execute: non-zero exit code %d", result.ExitCode)
		}
		return sb.String(), nil
	}
	return def, handler, true
}

// fanOutSchema is the JSON-Schema for the built-in fan_out tool.
// `calls` is the ordered list of sub-tool dispatches; each entry
// names the tool and carries that tool's input args. `parallelism`
// caps concurrent in-flight calls.
var fanOutSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "calls": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "properties": {
          "tool": {"type": "string", "description": "Name of a tool registered on this agent."},
          "args": {"type": "object", "description": "Args object matching the named tool's input_schema."}
        },
        "required": ["tool"]
      }
    },
    "parallelism": {
      "type": "integer",
      "minimum": 1,
      "maximum": 10,
      "description": "Max concurrent sub-tool dispatches. Defaults to 5; capped at 10 to bound token spend."
    }
  },
  "required": ["calls"]
}`)

const (
	fanOutDefaultParallelism = 5
	fanOutMaxParallelism     = 10
)

// builtinFanOutTool returns the `fan_out` built-in. It dispatches a
// list of sub-tool calls concurrently against THIS agent's catalog —
// the same catalog the LLM already sees — so any tool the agent can
// call sequentially can also be fan-out called. Results land in the
// same order as the input calls; an individual call's failure does
// NOT abort siblings (the LLM gets the partial-success array and
// decides whether to retry). Built on top of the existing tool
// dispatch path so cycle / depth / output_transform / approval all
// carry through unchanged.
//
// `cat` is captured by reference; at execution time the catalog is
// fully assembled (built-ins + skills + as_tool targets), so any
// registered tool name is reachable.
func builtinFanOutTool(cat *ToolCatalog) (llm.ToolDef, ToolHandler) {
	def := llm.ToolDef{
		Name: "fan_out",
		Description: `Dispatch multiple sub-tool calls in parallel and aggregate the results.
Use for map-reduce patterns: e.g. summarize N documents, query M data sources,
delegate K tasks to specialist sub-agents. Returns an array of {output, error}
in the same order as the input calls. Failures don't abort siblings — pivot
on partial success.`,
		InputSchema: fanOutSchema,
	}
	handler := func(ctx context.Context, args json.RawMessage) (string, error) {
		var in struct {
			Calls []struct {
				Tool string          `json:"tool"`
				Args json.RawMessage `json:"args"`
			} `json:"calls"`
			Parallelism int `json:"parallelism"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("fan_out: bad args: %w", err)
		}
		if len(in.Calls) == 0 {
			return "", fmt.Errorf("fan_out: calls must be non-empty")
		}
		p := in.Parallelism
		if p <= 0 {
			p = fanOutDefaultParallelism
		}
		if p > fanOutMaxParallelism {
			p = fanOutMaxParallelism
		}

		type result struct {
			Output any    `json:"output,omitempty"`
			Error  string `json:"error,omitempty"`
		}
		results := make([]result, len(in.Calls))

		// Buffered channel = semaphore; bounds in-flight goroutines
		// without pulling in a sync/errgroup dep. WaitGroup blocks
		// until every sub-call returns (or errors).
		sem := make(chan struct{}, p)
		var wg sync.WaitGroup
		for i, call := range in.Calls {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				if call.Tool == "" {
					results[i] = result{Error: "fan_out: empty tool name"}
					return
				}
				callArgs := call.Args
				if len(callArgs) == 0 {
					callArgs = json.RawMessage(`{}`)
				}
				obs, err := cat.Execute(ctx, call.Tool, callArgs)
				if err != nil {
					// Validation failures happen BEFORE the as_tool target
					// closure runs, so no step_start / step_done are
					// emitted for the target node — its canvas badge
					// stays grey while the LLM sees the error. Emit
					// synthetic start + done(error) here so the failure
					// surfaces on the right node. Handler-runtime errors
					// (the target ran + threw) already emit step_done
					// from the closure, so only do this for the
					// validation path.
					if errors.Is(err, ErrToolValidation) {
						if env, ok := envFromCtx(ctx); ok {
							if targetID, hasID := env.toolNameToNodeID[call.Tool]; hasID {
								if target, hasNode := env.byID[targetID]; hasNode {
									if env.events != nil {
										env.events.Emit(stampNow(RunEvent{
											Type:     EventStepStart,
											NodeID:   target.ID,
											NodeType: target.Type,
										}))
										env.events.Emit(stampNow(RunEvent{
											Type:     EventStepDone,
											NodeID:   target.ID,
											NodeType: target.Type,
											Error:    err.Error(),
											IsError:  true,
										}))
									}
									if env.toolSteps != nil {
										env.toolSteps.add(StepResult{
											NodeID:       target.ID,
											NodeType:     target.Type,
											Error:        err.Error(),
											ViaAgentTool: true,
										})
									}
								}
							}
						}
					}
					// Include any observation the failing tool managed
					// to emit alongside the error so the LLM has more
					// than just an error string to reason over.
					r := result{Error: err.Error()}
					if obs != "" {
						r.Output = obs
					}
					results[i] = r
					return
				}
				// Try to decode the tool's string result back into a
				// structured value so the LLM sees JSON instead of a
				// double-encoded string. Fall back to the raw string
				// when decoding fails (e.g. code_execute observation
				// blocks).
				var decoded any
				if jerr := json.Unmarshal([]byte(obs), &decoded); jerr == nil {
					results[i] = result{Output: decoded}
				} else {
					results[i] = result{Output: obs}
				}
			}()
		}
		wg.Wait()

		out := struct {
			Results []result `json:"results"`
		}{Results: results}
		b, err := json.Marshal(out)
		if err != nil {
			return "", fmt.Errorf("fan_out: marshal results: %w", err)
		}
		return string(b), nil
	}
	return def, handler
}

// --- as_tool: existing nodes opt-in as agent tools ---

// ErrToolValidation marks tool-dispatch failures where the args
// didn't match the tool's declared input_schema OR weren't valid JSON
// — i.e. the handler never ran. Surfaced for callers (fan_out) that
// need to distinguish "the handler errored mid-flight" (already
// emitted step_done) from "the handler never started" (caller must
// emit a synthetic step_done so the canvas shows the failure).
var ErrToolValidation = errors.New("tool input validation failed")

// asToolDef extracts a ToolDef from a target node's data.as_tool block.
// Returns ok=false when the node hasn't opted in.
func asToolDef(target Node, prefix string) (llm.ToolDef, bool) {
	at, ok := mapAny(target.Data["as_tool"])
	if !ok {
		return llm.ToolDef{}, false
	}
	if enabled, _ := at["enabled"].(bool); !enabled {
		return llm.ToolDef{}, false
	}
	name, _ := at["name"].(string)
	if name == "" {
		// fall back to step name or node ID
		name, _ = target.Data["name"].(string)
		if name == "" {
			name = string(target.Type) + "_" + target.ID
		}
	}
	if prefix != "" {
		name = prefix + "_" + name
	}
	desc, _ := at["description"].(string)

	// input_schema may arrive as a JSON string, a map[string]any (UI/JSON
	// path), or a bson.D / bson.M (MongoDB-decoded nested doc).
	var schema json.RawMessage
	switch v := at["input_schema"].(type) {
	case string:
		schema = json.RawMessage(v)
	case nil:
		schema = json.RawMessage(`{"type":"object"}`)
	default:
		if m, ok := mapAny(v); ok {
			b, err := json.Marshal(m)
			if err != nil {
				return llm.ToolDef{}, false
			}
			schema = b
		} else {
			// unknown shape — permit any object
			schema = json.RawMessage(`{"type":"object"}`)
		}
	}

	return llm.ToolDef{
		Name:        sanitizeToolName(name),
		Description: desc,
		InputSchema: schema,
	}, true
}

// sanitizeToolName ensures the name matches LLM tool-name conventions
// (alphanumeric + underscore only). Anthropic accepts a–zA–Z0–9_-; OpenAI
// is similar. Replace anything else with underscore.
func sanitizeToolName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}
