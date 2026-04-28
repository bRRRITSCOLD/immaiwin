package workflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/llm"
)

// Agent run defaults — overridable via node data fields.
const (
	defaultMaxIterations       = 8
	defaultMaxToolCallsPerIter = 5
	defaultMaxTokens           = 4096
	defaultAgentTimeout        = 5 * time.Minute
	defaultMaxMemoryMessages   = 30
)

// runAIAgent executes an AI Agent node: a reason-act-observe loop that
// drives an LLM to call tools and produce a final response.
//
// Data fields read:
//
//	llm_connection_id      string  required — references workflow Connection of type anthropic/openai/ollama
//	system_prompt          string  template-enabled
//	user_input             string  template-enabled, defaults to JSON of input
//	model_override         string  override provider's default model
//	memory_session_id      string  template-enabled; empty = no persistence
//	max_iterations         int     default 8
//	max_tool_calls_per_iter int    default 5
//	max_tokens             int     default 4096
//	temperature            float   default provider's default
//	timeout_seconds        int     default 300 (5min)
//
// Tool catalog assembly:
//   - Outgoing tool edges from the agent node → target node's data.as_tool
//   - Built-in code_execute tool (when SandboxRT available)
//   - Skills (P1.11; not yet wired here)
func (e *WorkflowExecutor) runAIAgent(ctx context.Context, node Node, data map[string]any,
	input any, wfCtx runCtx, params map[string]string) (any, error) {

	if e.ConnResolver == nil {
		return nil, fmt.Errorf("ai_agent: ConnResolver not configured")
	}

	env, ok := envFromCtx(ctx)
	if !ok {
		return nil, fmt.Errorf("ai_agent: run env missing from context")
	}

	// 1. Resolve LLM provider
	connID, _ := data["llm_connection_id"].(string)
	if connID == "" {
		return nil, fmt.Errorf("ai_agent: llm_connection_id required")
	}
	provider, err := e.ConnResolver.ResolveLLM(ctx, connID)
	if err != nil {
		return nil, fmt.Errorf("ai_agent: resolve llm: %w", err)
	}

	// 2. Bounds + config
	maxIters := getIntData(data, "max_iterations", defaultMaxIterations)
	maxToolCalls := getIntData(data, "max_tool_calls_per_iter", defaultMaxToolCallsPerIter)
	maxTokens := getIntData(data, "max_tokens", defaultMaxTokens)
	timeoutSec := getIntData(data, "timeout_seconds", int(defaultAgentTimeout.Seconds()))
	temperature := getFloat64Data(data, "temperature")
	model, _ := data["model_override"].(string)

	// 3. Resolve templates in prompt + user input
	systemPrompt := applyTemplate(getStringData(data, "system_prompt"), input, wfCtx)
	userInputRaw := getStringData(data, "user_input")
	var userInputText string
	if userInputRaw != "" {
		userInputText = applyTemplate(userInputRaw, input, wfCtx)
	} else {
		// Default: marshal input as JSON so the LLM has the upstream payload.
		b, _ := json.Marshal(input)
		userInputText = string(b)
	}

	// 4. Tool catalog
	catalog, err := e.buildAgentToolCatalog(node, env, wfCtx, params)
	if err != nil {
		return nil, fmt.Errorf("ai_agent: build tool catalog: %w", err)
	}

	// 5. Memory (optional)
	sessionID := applyTemplate(getStringData(data, "memory_session_id"), input, wfCtx)
	var history []llm.Message
	if sessionID != "" && e.Memory != nil {
		history, err = e.Memory.Load(ctx, sessionID, defaultMaxMemoryMessages)
		if err != nil {
			slog.Warn("ai_agent: memory load failed (continuing without history)", "session", sessionID, "err", err)
			history = nil
		}
	}

	// 6. Per-agent timeout — bounds the ReAct loop wall-clock.
	loopCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	// 7. Build initial messages = history + new user turn
	messages := append([]llm.Message{}, history...)
	messages = append(messages, llm.UserText(userInputText))

	// Track totals across the loop for the return payload + run record.
	var usageTotal UsageTotal
	var traceEvents []TraceEvent

	emitTrace := func(ev TraceEvent) {
		ev.At = time.Now().UTC()
		traceEvents = append(traceEvents, ev)
		// Best-effort persistence; don't fail the run on trace errors.
		if env.runID != "" && e.RunRepo != nil {
			_ = e.RunRepo.AppendTrace(ctx, env.runID, node.ID, ev)
		}
	}

	// 8. ReAct loop
	for iter := 0; iter < maxIters; iter++ {
		emitTrace(TraceEvent{Type: "iter_start", Iter: iter})

		req := llm.ChatRequest{
			Model:       model,
			System:      systemPrompt,
			Messages:    messages,
			Tools:       catalog.Defs(),
			MaxTokens:   maxTokens,
			Temperature: temperature,
		}

		resp, err := provider.Chat(loopCtx, req)
		if err != nil {
			return nil, fmt.Errorf("ai_agent: llm chat (iter %d): %w", iter, err)
		}

		usageTotal.Add(resp.Usage.InputTokens, resp.Usage.OutputTokens, resp.Usage.CostUSD)
		emitTrace(TraceEvent{
			Type: "llm_call",
			Iter: iter,
			Text: extractText(resp.Content),
			Usage: &UsageTotal{
				InputTokens:  resp.Usage.InputTokens,
				OutputTokens: resp.Usage.OutputTokens,
				TotalTokens:  resp.Usage.TotalTokens,
				CostUSD:      resp.Usage.CostUSD,
			},
		})

		// Always append the assistant turn so the LLM sees its own emission
		// when we feed back observations.
		messages = append(messages, llm.Message{Role: llm.RoleAssistant, Content: resp.Content})

		switch resp.StopReason {
		case llm.StopReasonEndTurn:
			finalText := extractText(resp.Content)
			emitTrace(TraceEvent{Type: "final", Iter: iter, Text: finalText})

			// Persist memory (only the new turns, not the existing history)
			if sessionID != "" && e.Memory != nil {
				newTurns := []llm.Message{
					llm.UserText(userInputText),
					{Role: llm.RoleAssistant, Content: resp.Content},
				}
				if err := e.Memory.Append(ctx, sessionID, newTurns); err != nil {
					slog.Warn("ai_agent: memory append failed", "session", sessionID, "err", err)
				}
				_ = e.Memory.Trim(ctx, sessionID, defaultMaxMemoryMessages)
			}

			// Return shape: rich result with usage + trace for downstream nodes.
			return map[string]any{
				"output":     finalText,
				"usage":      usageTotal,
				"iterations": iter + 1,
				"trace":      traceEvents,
			}, nil

		case llm.StopReasonToolUse:
			// Execute every tool_use block in the assistant turn.
			toolCalls := filterToolUseBlocks(resp.Content)
			if len(toolCalls) == 0 {
				return nil, fmt.Errorf("ai_agent: stop_reason=tool_use but no tool_use blocks (iter %d)", iter)
			}
			if len(toolCalls) > maxToolCalls {
				return nil, fmt.Errorf("ai_agent: model emitted %d tool calls; max is %d", len(toolCalls), maxToolCalls)
			}

			results := make([]llm.Content, 0, len(toolCalls))
			for _, call := range toolCalls {
				emitTrace(TraceEvent{
					Type:     "tool_call",
					Iter:     iter,
					ToolName: call.Name,
					ToolID:   call.ID,
					ToolArgs: rawJSONToAny(call.Input),
				})
				obs, err := catalog.Execute(loopCtx, call.Name, call.Input)
				isErr := err != nil
				if isErr {
					obs = err.Error()
				}
				results = append(results, llm.ToolResultBlock(call.ID, obs, isErr))
				emitTrace(TraceEvent{
					Type:     "tool_result",
					Iter:     iter,
					ToolName: call.Name,
					ToolID:   call.ID,
					Result:   obs,
					IsError:  isErr,
				})
			}
			messages = append(messages, llm.ToolResultMessage(results))
			continue

		case llm.StopReasonMaxTokens:
			return nil, fmt.Errorf("ai_agent: hit max_tokens at iter %d (output truncated)", iter)

		default:
			return nil, fmt.Errorf("ai_agent: unexpected stop_reason %q at iter %d", resp.StopReason, iter)
		}
	}

	return nil, fmt.Errorf("ai_agent: max iterations (%d) exceeded", maxIters)
}

// buildAgentToolCatalog assembles the tools available to one agent run:
// edge-bound nodes (data.as_tool) + built-in code_execute. Skills wired in P1.11.
func (e *WorkflowExecutor) buildAgentToolCatalog(agent Node, env *runEnv,
	wfCtx runCtx, params map[string]string) (*ToolCatalog, error) {

	cat := NewToolCatalog()

	// Built-in code_execute (if sandbox available)
	if def, h, ok := builtinCodeExecuteTool(e.SandboxRT); ok {
		if err := cat.Add(def, h); err != nil {
			return nil, err
		}
	}

	// Edge-bound nodes
	for _, et := range env.adj[agent.ID] {
		if et.sourceHandle != EdgeHandleTool {
			continue
		}
		target, ok := env.byID[et.targetID]
		if !ok {
			continue
		}
		def, ok := asToolDef(target, "")
		if !ok {
			continue // node didn't opt in
		}
		// Snapshot so the closure doesn't capture loop-mutated vars.
		targetNode := target
		toolName := def.Name
		handler := func(ctx context.Context, args json.RawMessage) (string, error) {
			// Decode args into any so they become the target node's input.
			var argInput any
			if len(args) > 0 {
				if err := json.Unmarshal(args, &argInput); err != nil {
					return "", fmt.Errorf("tool %s: bad args: %w", toolName, err)
				}
			}
			out, err := e.runNode(ctx, targetNode, argInput, wfCtx, params)
			if err != nil {
				return "", err
			}
			b, _ := json.Marshal(out)
			return string(b), nil
		}
		if err := cat.Add(def, handler); err != nil {
			slog.Warn("ai_agent: tool collision dropped", "name", def.Name, "err", err)
			continue
		}
	}

	return cat, nil
}

// --- helpers ---

func getIntData(data map[string]any, key string, def int) int {
	if v, ok := data[key]; ok {
		switch x := v.(type) {
		case int:
			if x > 0 {
				return x
			}
		case int32:
			if x > 0 {
				return int(x)
			}
		case int64:
			if x > 0 {
				return int(x)
			}
		case float64:
			if x > 0 {
				return int(x)
			}
		}
	}
	return def
}

func getFloat64Data(data map[string]any, key string) *float64 {
	v, ok := data[key]
	if !ok {
		return nil
	}
	switch x := v.(type) {
	case float64:
		return &x
	case float32:
		fx := float64(x)
		return &fx
	case int:
		fx := float64(x)
		return &fx
	}
	return nil
}

func getStringData(data map[string]any, key string) string {
	s, _ := data[key].(string)
	return s
}

func extractText(blocks []llm.Content) string {
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == llm.ContentTypeText {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

func filterToolUseBlocks(blocks []llm.Content) []llm.Content {
	var out []llm.Content
	for _, b := range blocks {
		if b.Type == llm.ContentTypeToolUse {
			out = append(out, b)
		}
	}
	return out
}

func rawJSONToAny(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	return v
}

// newRunID returns a 16-char hex ID for new WorkflowRun records. Caller
// uses this when persistence is enabled. Lives here because the executor
// is the natural place to mint run IDs.
func newRunID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
