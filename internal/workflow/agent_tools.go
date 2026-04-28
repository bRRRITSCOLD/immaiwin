package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/llm"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/sandbox"
)

// ToolCatalog holds the tools an agent has access to during one run.
// Tools come from three sources, in priority order if names collide:
//  1. Built-in tools (code_execute, …) — reserved namespace.
//  2. Skill-supplied tools (P1.11) — prefixed with namespace__name__.
//  3. Workflow-edge-bound nodes — opt-in via data.as_tool on target.
type ToolCatalog struct {
	defs     []llm.ToolDef
	handlers map[string]ToolHandler
}

// ToolHandler executes a tool call and returns its observation. Returning
// an error sets is_error on the tool_result block; the agent loop
// continues so the LLM can react to the failure.
type ToolHandler func(ctx context.Context, args json.RawMessage) (string, error)

// NewToolCatalog returns an empty catalog.
func NewToolCatalog() *ToolCatalog {
	return &ToolCatalog{handlers: make(map[string]ToolHandler)}
}

// Add registers a tool. Returns an error on duplicate name.
func (c *ToolCatalog) Add(def llm.ToolDef, handler ToolHandler) error {
	if _, dup := c.handlers[def.Name]; dup {
		return fmt.Errorf("tool catalog: duplicate name %q", def.Name)
	}
	c.defs = append(c.defs, def)
	c.handlers[def.Name] = handler
	return nil
}

// Defs returns the tool definitions in registration order, suitable for
// passing to llm.ChatRequest.Tools.
func (c *ToolCatalog) Defs() []llm.ToolDef { return c.defs }

// Execute looks up a handler by name and invokes it. Returns the
// observation text. Unknown tool returns an error with a clear message
// the LLM can correct from.
func (c *ToolCatalog) Execute(ctx context.Context, name string, args json.RawMessage) (string, error) {
	h, ok := c.handlers[name]
	if !ok {
		known := make([]string, 0, len(c.defs))
		for _, d := range c.defs {
			known = append(known, d.Name)
		}
		return "", fmt.Errorf("unknown tool %q; available: %s", name, strings.Join(known, ", "))
	}
	return h(ctx, args)
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

// --- as_tool: existing nodes opt-in as agent tools ---

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
