package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/llm"
	"github.com/oklog/ulid/v2"
)

// isInfraToolError reports whether a tool-handler error originates
// from sandbox infrastructure (k3s pod attach, docker daemon, image
// pull, etc.) rather than from user-space tool logic. The agent loop
// treats infra failures as terminal regardless of `stop_on_tool_error`
// because the LLM has no plausible recovery path — retrying / pivoting
// won't bring the sandbox back. Detection by string prefix is brittle
// but matches every internal/sandbox emit-site (`fmt.Errorf("sandbox/<engine>: …")`)
// + needs no API change to the sandbox package.
func isInfraToolError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "sandbox/k3s:") ||
		strings.Contains(msg, "sandbox/docker:") ||
		strings.Contains(msg, "sandbox/dap:") ||
		strings.Contains(msg, "sandbox/cdp:")
}

// Agent run defaults — overridable via node data fields.
const (
	defaultMaxIterations       = 8
	defaultMaxToolCallsPerIter = 5
	defaultMaxTokens           = 4096
	defaultAgentTimeout        = 5 * time.Minute
	defaultMaxMemoryMessages   = 30

	// finalAnswerToolName is the reserved tool name registered onto an
	// agent's catalog when its `output_schema` field is non-empty. The
	// LLM is instructed to call this tool with the structured answer
	// (validated against the schema) instead of returning free text.
	finalAnswerToolName = "submit_final_answer"
)

// normaliseOutputSchema confirms the user-supplied JSON Schema parses,
// then re-marshals into compact bytes so the wire payload to the LLM
// provider stays small. Returns an error when the schema isn't valid
// JSON (e.g. the user pasted YAML); silent acceptance would let the
// validator fall through to permissive at runtime.
func normaliseOutputSchema(raw string) (json.RawMessage, error) {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, err
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(out), nil
}

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
	requireApproval := getBoolData(data, "require_approval")
	// stopOnToolError: when set, any tool-handler error from
	// catalog.Execute aborts the agent loop instead of feeding the
	// error back to the LLM as a tool_result. The agent's StepResult
	// then carries Error, which the run-status logic in
	// RunFromCheckpoint promotes to the run-level `error` badge.
	// Default false preserves the LLM's ability to self-correct on
	// transient tool failures (e.g. a flaky upstream API). Strict
	// agents that should NOT hallucinate around infra errors set
	// `stop_on_tool_error: true` in node data. Sandbox-engine
	// failures (k3s pod attach failed, docker daemon down, etc.)
	// abort regardless because the LLM has no way to recover from
	// them — we treat any err prefixed with `sandbox/` as terminal.
	stopOnToolError := getBoolData(data, "stop_on_tool_error")

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

	// 4. Tool catalog (also populates env.skillSystemFragments if skills opted in)
	catalog, err := e.buildAgentToolCatalog(node, env, wfCtx, params, input)
	if err != nil {
		return nil, fmt.Errorf("ai_agent: build tool catalog: %w", err)
	}

	// Output-schema gate: when an agent declares `output_schema`, we
	// append a synthetic `submit_final_answer` tool whose input_schema is
	// the user's output schema. The agent's instructions tell it to call
	// this tool when it's done. The catalog's input-validation layer
	// (just shipped) enforces shape before the call lands here, so by
	// the time we capture `finalAnswerArgs` it's already validated. The
	// loop terminates as soon as the model calls this tool.
	var (
		finalAnswerArgs   any
		finalAnswerCalled bool
	)
	outputSchemaRaw := strings.TrimSpace(getStringData(data, "output_schema"))
	if outputSchemaRaw != "" {
		schemaBytes, schemaErr := normaliseOutputSchema(outputSchemaRaw)
		if schemaErr != nil {
			return nil, fmt.Errorf("ai_agent: output_schema invalid JSON: %w", schemaErr)
		}
		finalDef := llm.ToolDef{
			Name:        finalAnswerToolName,
			Description: "Submit your final structured answer. Call this when you have everything needed to fulfil the user's request. The args passed here become this agent's output. Do NOT call this until you've gathered all information needed.",
			InputSchema: schemaBytes,
		}
		finalHandler := func(ctx context.Context, args json.RawMessage) (string, error) {
			var decoded any
			if len(args) > 0 {
				if uerr := json.Unmarshal(args, &decoded); uerr != nil {
					return "", fmt.Errorf("submit_final_answer: bad args: %w", uerr)
				}
			}
			finalAnswerArgs = decoded
			finalAnswerCalled = true
			return "answer accepted", nil
		}
		if addErr := catalog.Add(finalDef, finalHandler); addErr != nil {
			return nil, fmt.Errorf("ai_agent: register submit_final_answer: %w", addErr)
		}
	}

	// Append skill-supplied system-prompt fragments. Skill fragments come
	// AFTER the agent-author prompt so explicit per-agent instructions
	// take precedence over generic skill guidance.
	if len(env.skillSystemFragments) > 0 {
		var sb strings.Builder
		sb.WriteString(systemPrompt)
		for _, frag := range env.skillSystemFragments {
			if sb.Len() > 0 {
				sb.WriteString("\n\n")
			}
			sb.WriteString(frag)
		}
		systemPrompt = sb.String()
	}

	// Output-schema directive — appended LAST so it can't be overridden
	// by the user's prompt or a skill fragment. Tells the model the
	// terminal action is the synthetic tool call.
	if outputSchemaRaw != "" {
		var sb strings.Builder
		sb.WriteString(systemPrompt)
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString("OUTPUT FORMAT — MANDATORY: When you have everything needed to fulfil the request, you MUST call the `submit_final_answer` tool with the structured answer. Do NOT respond with a plain text final message — only the args you pass to `submit_final_answer` count as your output. Free-text closing messages will be discarded.")
		systemPrompt = sb.String()
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

	// 7. Build initial messages, or hydrate from a paused-run snapshot when
	// the workflow run was started via `resume_run_id`. The hydrated path
	// skips the user turn (already in messages) and starts the ReAct loop
	// at the saved iter so the LLM picks up immediately after the previous
	// tool_result.
	var (
		messages    []llm.Message
		startIter   int
		usageTotal  UsageTotal
		traceEvents []TraceEvent
	)
	// snapshotApprovedFlags captures env.approvedToolCallNames +
	// env.approvedNodeIDs into the bson-friendly shapes
	// AgentPauseState carries so a yield can persist them. Saved
	// approvals stay sticky across yield-resume cycles within the
	// same dispatch (cleared at iter-end). Closures rebind on every
	// call so we always read the current env state.
	snapshotApprovedToolCallNames := func() map[string]ApprovalDecision {
		if len(env.approvedToolCallNames) == 0 {
			return nil
		}
		out := make(map[string]ApprovalDecision, len(env.approvedToolCallNames))
		for k, v := range env.approvedToolCallNames {
			if v != nil {
				out[k] = *v
			}
		}
		return out
	}
	snapshotApprovedNodeIDs := func() []string {
		if len(env.approvedNodeIDs) == 0 {
			return nil
		}
		out := make([]string, 0, len(env.approvedNodeIDs))
		for k := range env.approvedNodeIDs {
			out = append(out, k)
		}
		return out
	}

	// Partial-tool-dispatch resume: when the agent yielded mid-iter
	// because an approval gate fired AFTER one or more parallel
	// tool_uses had already been dispatched, the saved PausedAgent
	// carries the assistant turn (un-popped), the partially-collected
	// tool_results, and the index where dispatch was interrupted. On
	// resume we skip the provider.Chat call for that iter and pick up
	// the dispatch loop where it left off — without this, re-prompting
	// produces the same parallel tool_uses and the gate fires again on
	// the first call → infinite approve-and-yield loop.
	var (
		partialResumeActive  bool
		partialToolCalls     []llm.Content
		partialToolResults   []llm.Content
		partialNextIndex     int
	)
	if env.resumeAgentState != nil && env.resumeAgentState.AgentNodeID == node.ID {
		startIter = env.resumeAgentState.Iter
		messages = env.resumeAgentState.Messages
		usageTotal = env.resumeAgentState.UsageTotal
		traceEvents = env.resumeAgentState.Trace
		if len(env.resumeAgentState.PartialToolCalls) > 0 {
			partialResumeActive = true
			partialToolCalls = env.resumeAgentState.PartialToolCalls
			partialToolResults = env.resumeAgentState.PartialToolResults
			partialNextIndex = env.resumeAgentState.PartialNextIndex
		}
		// Restore per-call approval flags so a nested-gate cascade
		// (per-tool then node-level on the same call) doesn't re-fire
		// the first gate on the second yield's resume. The
		// checkpoint hydration (executor) already adds the freshly-
		// landed decision into env.approvedToolCallNames /
		// env.approvedNodeIDs from priorState.Pending; merge with the
		// PausedAgent-saved state so previously-approved gates within
		// this dispatch stay approved.
		if len(env.resumeAgentState.ApprovedToolCallNames) > 0 {
			if env.approvedToolCallNames == nil {
				env.approvedToolCallNames = map[string]*ApprovalDecision{}
			}
			for name, dec := range env.resumeAgentState.ApprovedToolCallNames {
				if _, already := env.approvedToolCallNames[name]; already {
					continue
				}
				cp := dec
				env.approvedToolCallNames[name] = &cp
			}
		}
		if len(env.resumeAgentState.ApprovedNodeIDs) > 0 {
			if env.approvedNodeIDs == nil {
				env.approvedNodeIDs = map[string]bool{}
			}
			for _, id := range env.resumeAgentState.ApprovedNodeIDs {
				env.approvedNodeIDs[id] = true
			}
		}
		// Use the saved prompt/input verbatim — the workflow-author may
		// have edited the agent node between pause and resume, but loop
		// state must be deterministic for the LLM.
		systemPrompt = env.resumeAgentState.SystemPrompt
		userInputText = env.resumeAgentState.UserInput
		slog.Info("ai_agent: resuming from paused state",
			"agent", node.ID, "run_id", env.runID, "iter", startIter,
			"messages", len(messages), "trace_events", len(traceEvents))
	} else {
		messages = append([]llm.Message{}, history...)
		messages = append(messages, llm.UserText(userInputText))
	}

	emitTrace := func(ev TraceEvent) {
		ev.At = time.Now().UTC()
		traceEvents = append(traceEvents, ev)
		// Best-effort persistence; don't fail the run on trace errors.
		if env.runID != "" && e.RunRepo != nil {
			_ = e.RunRepo.AppendTrace(ctx, env.runID, node.ID, ev)
		}
		// Mirror onto the run-level event stream so live UI clients see
		// agent loop progress in real time. Mapping is direct: trace
		// types map onto RunEvent types one-for-one. Missing types map
		// to no-op (defensive) so adding a new TraceEvent.Type never
		// breaks the stream.
		if env.events != nil {
			env.events.Emit(traceToRunEvent(ev, node))
		}
	}

	// 8. ReAct loop
	for iter := startIter; iter < maxIters; iter++ {
		// Halt if a debug breakpoint was hit during the previous iter's
		// tool dispatch. Persist the agent's working state to the
		// WorkflowRun record so the next Run with `resume_run_id` can
		// pick up exactly here. Returns the current trace + usage so the
		// UI sees what got done before the stop.
		if env.stopAtHit {
			if env.runID != "" && e.RunRepo != nil {
				if rec, gerr := e.RunRepo.Get(ctx, env.runID); gerr == nil {
					rec.Status = RunStatusPaused
					rec.PausedAgent = &AgentPauseState{
						AgentNodeID:           node.ID,
						Iter:                  iter,
						Messages:              messages,
						UsageTotal:            usageTotal,
						Trace:                 traceEvents,
						SystemPrompt:          systemPrompt,
						UserInput:             userInputText,
						ApprovedToolCallNames: snapshotApprovedToolCallNames(),
						ApprovedNodeIDs:       snapshotApprovedNodeIDs(),
					}
					if uerr := e.RunRepo.Update(ctx, rec); uerr != nil {
						slog.Warn("ai_agent: persist paused state failed", "run_id", env.runID, "err", uerr)
					}
				}
			}
			return map[string]any{
				"output":     "(paused — resume to continue)",
				"usage":      usageTotal,
				"iterations": iter,
				"trace":      traceEvents,
				"paused":     true,
				"run_id":     env.runID,
			}, nil
		}
		emitTrace(TraceEvent{Type: "iter_start", Iter: iter})

		// Cost cap PRE-CALL gate. Same logic as post-call below, run BEFORE
		// the chat to avoid burning one extra LLM call after the cap was
		// breached by a previous iter or a sibling workflow run. Bounded
		// "1-call slip": only iter 0 can slip through because there's no
		// post-iter check to gate iter 0 with — the pre-run cap check at
		// executor.RunResumable handles that case (and we re-check here
		// using `prior` so a sibling run that finished mid-flight is
		// caught on the very next iter).
		if env.wf != nil && env.wf.CostLimits != nil {
			if cap := env.wf.CostLimits.MaxRunUSD; cap > 0 && usageTotal.CostUSD >= cap {
				ce := &CostExceededError{Axis: "run", CapUSD: cap, SpentUSD: usageTotal.CostUSD}
				if env.events != nil {
					env.events.Emit(stampNow(RunEvent{Type: EventCostExceeded, NodeID: node.ID, NodeType: node.Type, Error: ce.Error()}))
				}
				return nil, ce
			}
			if cap := env.wf.CostLimits.MaxDailyUSD; cap > 0 && e.RunRepo != nil {
				prior, sumErr := e.RunRepo.SumCostSince(ctx, env.wf.ID, startOfUTCDay(time.Now()))
				if sumErr == nil && prior+usageTotal.CostUSD >= cap {
					ce := &CostExceededError{Axis: "daily", CapUSD: cap, SpentUSD: prior + usageTotal.CostUSD}
					if env.events != nil {
						env.events.Emit(stampNow(RunEvent{Type: EventCostExceeded, NodeID: node.ID, NodeType: node.Type, Error: ce.Error()}))
					}
					return nil, ce
				}
			}
		}

		// Partial-resume: skip the Chat call. Messages already contain
		// the original assistant turn; we have the toolCalls list +
		// the partially-completed results + the index to resume at.
		// Synthesise a ChatResponse with stop_reason=tool_use so the
		// switch below routes correctly. Token / usage tracking
		// already accounted for the original Chat call (counted in
		// usageTotal at original-iter time).
		var resp *llm.ChatResponse
		var startCallIdx int
		var preDispatchedResults []llm.Content
		if partialResumeActive && iter == startIter {
			// Reconstruct the assistant content the original iter
			// emitted by reading the trailing turn off `messages`.
			// On a clean resume the trailing turn is the assistant
			// turn with the tool_use blocks the user saw on the
			// approval gate. If the trailing turn isn't an assistant
			// turn the saved state is malformed — fall back to a
			// fresh Chat call rather than crash.
			lastIdx := len(messages) - 1
			if lastIdx < 0 || messages[lastIdx].Role != llm.RoleAssistant {
				slog.Warn("ai_agent: partial resume state corrupt (trailing turn not assistant); falling back to fresh Chat", "run_id", env.runID, "iter", iter)
				partialResumeActive = false
			} else {
				resp = &llm.ChatResponse{
					StopReason: llm.StopReasonToolUse,
					Content:    messages[lastIdx].Content,
				}
				startCallIdx = partialNextIndex
				preDispatchedResults = partialToolResults
				partialResumeActive = false // one-shot
			}
		}
		if resp == nil {
			req := llm.ChatRequest{
				Model:       model,
				System:      systemPrompt,
				Messages:    messages,
				Tools:       catalog.Defs(),
				MaxTokens:   maxTokens,
				Temperature: temperature,
			}

			var cerr error
			resp, cerr = provider.Chat(loopCtx, req)
			if cerr != nil {
				return nil, fmt.Errorf("ai_agent: llm chat (iter %d): %w", iter, cerr)
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
				Provider: provider.Name(),
				Model:    resp.Model,
			})

			// (Cost cap enforcement is the PRE-CALL block above this iter's
			// `provider.Chat`. The post-call slot is intentionally empty so
			// iter N's call can't be blocked retroactively after billing —
			// iter N+1's pre-call check catches the breach using the freshly-
			// updated usageTotal.)

			// Always append the assistant turn so the LLM sees its own emission
			// when we feed back observations.
			messages = append(messages, llm.Message{Role: llm.RoleAssistant, Content: resp.Content})
		}

		switch resp.StopReason {
		case llm.StopReasonEndTurn:
			finalText := extractText(resp.Content)

			// Output-schema enforcement: model emitted free text instead
			// of calling submit_final_answer. Feed back a corrective
			// observation and re-prompt. Bounded by the loop's max-iter
			// cap so a stubborn model can't run forever.
			if outputSchemaRaw != "" && !finalAnswerCalled {
				correction := "You produced a free-text response, but this agent's output is REQUIRED to come from a `submit_final_answer` tool call. Re-issue your answer by calling submit_final_answer with the structured args. Do not respond with text."
				messages = append(messages, llm.UserText(correction))
				emitTrace(TraceEvent{Type: "llm_call", Iter: iter, Text: "(server: rejected free-text answer; demanded submit_final_answer)"})
				continue
			}

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
			// On partial resume we trust the saved toolCalls list +
			// already-collected results + start-index instead of
			// re-extracting from the synthetic resp; that way a
			// model that re-emitted a different shape can't shift
			// the dispatch out from under the already-completed
			// ones.
			var toolCalls []llm.Content
			if startCallIdx > 0 && len(partialToolCalls) > 0 {
				toolCalls = partialToolCalls
			} else {
				toolCalls = filterToolUseBlocks(resp.Content)
			}
			if len(toolCalls) == 0 {
				return nil, fmt.Errorf("ai_agent: stop_reason=tool_use but no tool_use blocks (iter %d)", iter)
			}
			if len(toolCalls) > maxToolCalls {
				return nil, fmt.Errorf("ai_agent: model emitted %d tool calls; max is %d", len(toolCalls), maxToolCalls)
			}

			results := make([]llm.Content, 0, len(toolCalls))
			if startCallIdx > 0 && len(preDispatchedResults) > 0 {
				results = append(results, preDispatchedResults...)
			}
			for callIdx, call := range toolCalls {
				if callIdx < startCallIdx {
					continue
				}
				emitTrace(TraceEvent{
					Type:     "tool_call",
					Iter:     iter,
					ToolName: call.Name,
					ToolID:   call.ID,
					ToolArgs: rawJSONToAny(call.Input),
				})

				// Approval gate. Three paths:
				//   1. Pre-approved on resume — `env.approvedToolCallNames`
				//      carries a decision keyed by tool name (set by the
				//      checkpoint hydration when priorState.Pending was a
				//      tool_call gate that resolved). One-shot consumption.
				//   2. Live UI (WS) — env.approveCh wired, decisions
				//      arrive over the open socket; fast path.
				//   3. Out-of-band — server-side run with no socket.
				//      Under a lease (env.yieldOnApproval=true), the
				//      gate persists the agent's PausedAgent + writes
				//      execution_state.pending{kind:"tool_call"} +
				//      releases the lease + bubbles errYieldForApproval.
				//      The legacy fall-through (no lease) still
				//      registers a Redis channel + blocks via
				//      ApprovalRegistry — covers eval runs / pre-PR-71
				//      paths until those migrate.
				gateActive := false
				approveCh := env.approveCh
				oob := false
				if requireApproval && env.approvedToolCallNames != nil {
					if pre, ok := env.approvedToolCallNames[call.Name]; ok && pre != nil {
						// Apply pre-recorded decision. Skip gate.
						// DON'T delete the entry: a cascading
						// node-level gate (require_node_approval on
						// the as_tool target) might yield mid-
						// dispatch on the same call. If we delete
						// here, the second yield's resume re-fires
						// THIS gate again because the flag is gone
						// → infinite per-tool ↔ node-level ping-pong.
						// Iter-end cleanup clears both maps after a
						// successful full dispatch.
						if !pre.Approved {
							// Mirror the rejection path below: emit a
							// rejected tool_result + skip dispatch.
							reason := pre.Reason
							if reason == "" {
								reason = "rejected by user"
							}
							obs := "tool call rejected by user: " + reason
							results = append(results, llm.ToolResultBlock(call.ID, obs, true))
							emitTrace(TraceEvent{
								Type:     "tool_result",
								Iter:     iter,
								ToolName: call.Name,
								ToolID:   call.ID,
								Result:   obs,
								IsError:  true,
							})
							continue
						}
						// Approved: fall through to dispatch as if no gate.
						goto dispatchTool
					}
				}
				if requireApproval {
					if approveCh != nil {
						gateActive = true
					} else if env.yieldOnApproval && env.runID != "" && e.RunRepo != nil {
						// Lease-yield path. Mirror the UI-facing
						// PendingApproval (existing top-level field
						// the /runs UI reads) + dispatch OOB
						// notification + persist agent's mid-loop
						// snapshot (with trailing assistant turn
						// popped so resume can re-prompt cleanly) +
						// stash gate identity on env so the BFS
						// yield handler builds the right
						// PendingExecutionGate. Then return
						// errYieldForApproval to bubble up.
						pending := PendingApprovalState{
							Kind:        "tool_call",
							AgentNodeID: node.ID,
							Iter:        iter,
							ToolCallID:  call.ID,
							ToolName:    call.Name,
							ToolArgs:    rawJSONToAny(call.Input),
							RequestedAt: time.Now().UTC(),
							TokenID:     ulid.Make().String(),
						}
						if rec, gerr := e.RunRepo.Get(ctx, env.runID); gerr == nil {
							rec.Status = RunStatusPendingApproval
							rec.PendingApproval = &pending
							// Mid-iter yield: keep the assistant turn
							// on Messages so resume can replay the
							// dispatch from `callIdx` without re-
							// prompting the model. partialNextIndex
							// is the call we're about to gate (the
							// one not yet run); previously-completed
							// dispatches are already in `results`.
							rec.PausedAgent = &AgentPauseState{
								AgentNodeID:           node.ID,
								Iter:                  iter,
								Messages:              messages,
								UsageTotal:            usageTotal,
								Trace:                 traceEvents,
								SystemPrompt:          systemPrompt,
								UserInput:             userInputText,
								PartialToolCalls:      toolCalls,
								PartialToolResults:    append([]llm.Content{}, results...),
								PartialNextIndex:      callIdx,
								ApprovedToolCallNames: snapshotApprovedToolCallNames(),
								ApprovedNodeIDs:       snapshotApprovedNodeIDs(),
							}
							if uerr := e.RunRepo.Update(ctx, rec); uerr != nil {
								slog.Warn("ai_agent: persist tool_call yield failed", "run_id", env.runID, "err", uerr)
							}
							if env.events != nil {
								env.events.Emit(stampNow(RunEvent{
									Type:     EventAgentToolApproval,
									NodeID:   node.ID,
									NodeType: node.Type,
									Iter:     iter,
									ToolName: call.Name,
									ToolID:   call.ID,
									ToolArgs: rawJSONToAny(call.Input),
									RunID:    env.runID,
								}))
							}
							if env.wf != nil {
								e.dispatchApprovalNotification(*env.wf, env.runID, pending)
							}
						}
						env.pendingNodeID = node.ID
						env.pendingKind = "tool_call"
						env.pendingToolName = call.Name
						env.pendingToolCallID = call.ID
						return nil, errYieldForApproval
					} else if env.runID != "" && e.RunRepo != nil {
						// Legacy OOB block-on-Redis-channel path. Used
						// by eval runs + any pre-PR-71 caller that
						// doesn't go through the lease worker.
						oobReg := e.approvalRegistryFor()
						if oobReg != nil {
							oob = true
							gateActive = true
							approveCh = oobReg.Register(ctx, env.runID)
							defer oobReg.Unregister(env.runID)
						}
						if rec, gerr := e.RunRepo.Get(ctx, env.runID); gerr == nil {
							pending := PendingApprovalState{
								Kind:        "tool_call",
								AgentNodeID: node.ID,
								Iter:        iter,
								ToolCallID:  call.ID,
								ToolName:    call.Name,
								ToolArgs:    rawJSONToAny(call.Input),
								RequestedAt: time.Now().UTC(),
								TokenID:     ulid.Make().String(),
							}
							rec.Status = RunStatusPendingApproval
							rec.PendingApproval = &pending
							if uerr := e.RunRepo.Update(ctx, rec); uerr != nil {
								slog.Warn("ai_agent: persist pending_approval failed", "run_id", env.runID, "err", uerr)
							}
							if env.wf != nil {
								e.dispatchApprovalNotification(*env.wf, env.runID, pending)
							}
						}
					}
					// else: no live ch + no RunRepo → can't OOB; auto-approve.
				}
			dispatchTool:
				if gateActive {
					if env.events != nil {
						env.events.Emit(stampNow(RunEvent{
							Type:     EventAgentToolApproval,
							NodeID:   node.ID,
							NodeType: node.Type,
							Iter:     iter,
							ToolName: call.Name,
							ToolID:   call.ID,
							ToolArgs: rawJSONToAny(call.Input),
							// RunID lets the live UI route a "resolve via
							// /runs/:id" banner when the OOB dispatcher
							// (Slack / email) failed — without it the canvas
							// would have no reference for the deep-link.
							RunID: env.runID,
						}))
					}
					decision, ok := waitForApproval(loopCtx, approveCh, call.ID)
					if !ok {
						// Context cancelled while waiting (worker
						// shutdown / run cancelled / loop timeout).
						// Emit a synthetic tool_result so the trace
						// makes it obvious the tool was NOT executed —
						// the prior `tool_call` event already landed,
						// otherwise the UI shows an orphan call that
						// looks like it ran. Also clear pending state
						// so the UI doesn't keep polling.
						obs := "tool call NOT executed: approval wait cancelled before decision"
						emitTrace(TraceEvent{
							Type:     "tool_result",
							Iter:     iter,
							ToolName: call.Name,
							ToolID:   call.ID,
							Result:   obs,
							IsError:  true,
						})
						if oob && env.runID != "" && e.RunRepo != nil {
							if rec, gerr := e.RunRepo.Get(ctx, env.runID); gerr == nil {
								rec.PendingApproval = nil
								_ = e.RunRepo.Update(ctx, rec)
							}
						}
						return nil, fmt.Errorf("ai_agent: approval wait cancelled (iter %d, tool %s) — tool not executed", iter, call.Name)
					}
					// Decision arrived — flip status back to running and
					// clear the pending state on the run record so the
					// UI doesn't keep showing the Approve buttons.
					if oob {
						if rec, gerr := e.RunRepo.Get(ctx, env.runID); gerr == nil {
							rec.Status = RunStatusRunning
							rec.PendingApproval = nil
							if uerr := e.RunRepo.Update(ctx, rec); uerr != nil {
								slog.Warn("ai_agent: clear pending_approval failed", "run_id", env.runID, "err", uerr)
							}
						}
					}
					if !decision.Approved {
						reason := decision.Reason
						if reason == "" {
							reason = "rejected by user"
						}
						obs := "tool call rejected by user: " + reason

						// Mirror the rejection onto the as_tool target node
						// so the canvas flips it red ("rejected") instead
						// of leaving it as "not executed". Skill /
						// built-in tools have no canvas node — those skip
						// this block.
						if targetID, ok := env.toolNameToNodeID[call.Name]; ok {
							if targetNode, hasNode := env.byID[targetID]; hasNode {
								rejErr := "rejected by user: " + reason
								if env.events != nil {
									env.events.Emit(stampNow(RunEvent{
										Type:     EventStepStart,
										NodeID:   targetNode.ID,
										NodeType: targetNode.Type,
									}))
									env.events.Emit(stampNow(RunEvent{
										Type:     EventStepDone,
										NodeID:   targetNode.ID,
										NodeType: targetNode.Type,
										Error:    rejErr,
										IsError:  true,
									}))
								}
								if env.toolSteps != nil {
									env.toolSteps.add(StepResult{
										NodeID:   targetNode.ID,
										NodeType: targetNode.Type,
										Error:    rejErr,
									})
								}
							}
						}

						results = append(results, llm.ToolResultBlock(call.ID, obs, true))
						emitTrace(TraceEvent{
							Type:     "tool_result",
							Iter:     iter,
							ToolName: call.Name,
							ToolID:   call.ID,
							Result:   obs,
							IsError:  true,
						})
						continue
					}
				}

				obs, err := catalog.Execute(loopCtx, call.Name, call.Input)
				// Approval-gate yield: an `as_tool` target's
				// `require_node_approval=true` fired the lease-yield path.
				// Treat this NOT as a tool error — feeding it back to the
				// LLM as a tool_result would loop the model on retries —
				// but as a hard signal to persist the agent's mid-loop
				// state and bubble the yield up the BFS so the worker
				// releases its lease. On resume the worker rehydrates
				// from PausedAgent + applies the gate decision before
				// re-dispatching the same tool call.
				if errors.Is(err, errYieldForApproval) {
					if env.runID != "" && e.RunRepo != nil {
						// Mid-iter yield: keep the assistant turn on
						// Messages and snapshot the partial-dispatch
						// state (toolCalls + already-collected
						// results + the index of the call that
						// fired the gate). On resume the agent
						// skips the Chat call and continues
						// dispatching from PartialNextIndex.
						// Without this snapshot, multi-tool-use
						// responses re-prompt the model on every
						// gate, the model emits the same parallel
						// tool_uses, the first call's gate fires
						// again → infinite approve-and-yield loop.
						if rec, gerr := e.RunRepo.Get(ctx, env.runID); gerr == nil {
							rec.PausedAgent = &AgentPauseState{
								AgentNodeID:           node.ID,
								Iter:                  iter,
								Messages:              messages,
								UsageTotal:            usageTotal,
								Trace:                 traceEvents,
								SystemPrompt:          systemPrompt,
								UserInput:             userInputText,
								PartialToolCalls:      toolCalls,
								PartialToolResults:    append([]llm.Content{}, results...),
								PartialNextIndex:      callIdx,
								ApprovedToolCallNames: snapshotApprovedToolCallNames(),
								ApprovedNodeIDs:       snapshotApprovedNodeIDs(),
							}
							if uerr := e.RunRepo.Update(ctx, rec); uerr != nil {
								slog.Warn("ai_agent: persist paused-agent on yield failed", "run_id", env.runID, "agent", node.ID, "err", uerr)
							}
						}
					}
					return nil, err
				}
				isErr := err != nil
				if isErr {
					// Prefer the handler's rich string (often includes stderr +
					// exit code) so the LLM can self-diagnose. Fall back to
					// err.Error() only when the handler returned no detail.
					if obs == "" {
						obs = err.Error()
					} else {
						obs = obs + "\nerror: " + err.Error()
					}
					// Terminal-error classification: if the agent was
					// configured to stop on any tool error, OR the
					// underlying error is a sandbox-engine failure
					// (k3s pod failed, docker daemon down, …) which
					// the LLM has no path to recover from, surface
					// the trace + return error from runAIAgent. The
					// BFS picks up the err on the agent's runNode
					// return → agent's StepResult.Error populated →
					// RunFromCheckpoint's status-promotion scan
					// flips the run badge to `error`.
					if stopOnToolError || isInfraToolError(err) {
						emitTrace(TraceEvent{
							Type:     "tool_result",
							Iter:     iter,
							ToolName: call.Name,
							ToolID:   call.ID,
							Result:   obs,
							IsError:  true,
						})
						return nil, fmt.Errorf("ai_agent: tool %q errored (terminal): %w", call.Name, err)
					}
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

			// Output-schema terminator. If the model called
			// submit_final_answer, the synthetic handler stashed the
			// validated args in finalAnswerArgs. Exit the loop now
			// instead of feeding tool_results back for another iter —
			// the answer is the args, not whatever the model would say
			// next. Schema validation happened in catalog.Execute, so
			// finalAnswerArgs is already shape-correct.
			if finalAnswerCalled {
				emitTrace(TraceEvent{Type: "final", Iter: iter})
				if sessionID != "" && e.Memory != nil {
					newTurns := []llm.Message{
						llm.UserText(userInputText),
						{Role: llm.RoleAssistant, Content: resp.Content},
					}
					if mErr := e.Memory.Append(ctx, sessionID, newTurns); mErr != nil {
						slog.Warn("ai_agent: memory append failed", "session", sessionID, "err", mErr)
					}
					_ = e.Memory.Trim(ctx, sessionID, defaultMaxMemoryMessages)
				}
				return map[string]any{
					"output":     finalAnswerArgs,
					"usage":      usageTotal,
					"iterations": iter + 1,
					"trace":      traceEvents,
				}, nil
			}

			messages = append(messages, llm.ToolResultMessage(results))
			// Iter dispatch fully completed without yielding. Clear
			// per-call approval flags so a re-occurrence of the same
			// tool name / node ID in a LATER iter requires fresh
			// approval. Within-iter persistence is preserved by
			// PausedAgent across yield-resume cycles; this clear is
			// the across-iter scope reset.
			env.approvedToolCallNames = nil
			env.approvedNodeIDs = nil
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
//  1. Built-in `code_execute` (if SandboxRT is configured).
//  2. Skill-supplied tools (if SkillRes is configured AND data.skills is set).
//  3. Edge-bound `as_tool` target nodes.
//
// Order matters: collisions resolve in the order tools are added, so a
// later registration with the same name is dropped (NewToolCatalog.Add
// rejects duplicates with a logged warn). Built-ins win over skill tools
// win over node tools — that's by design so reserved names stay reserved.
func (e *WorkflowExecutor) buildAgentToolCatalog(agent Node, env *runEnv,
	wfCtx runCtx, params map[string]string, input any) (*ToolCatalog, error) {

	cat := NewToolCatalog()

	// 1. Built-in code_execute (if sandbox available)
	if def, h, ok := builtinCodeExecuteTool(e.SandboxRT); ok {
		if err := cat.Add(def, h); err != nil {
			return nil, err
		}
	}

	// 2. Skill-supplied tools (P1.11). Reads data.skills as []SkillReq,
	// resolves to a lockfile via SkillRes, then registers each tool with the
	// agent using a `<sanitized-slug>__<tool_id>` prefix to avoid collisions.
	skillFragments, err := e.appendSkillTools(agent, cat, params, wfCtx, input)
	if err != nil {
		return nil, err
	}
	if len(skillFragments) > 0 {
		// Stash for runAIAgent to append onto the system prompt. We thread it
		// through env to avoid changing this function's signature.
		env.skillSystemFragments = skillFragments
	}

	// Edge-bound nodes
	var toolEdges, optedIn int
	for _, et := range env.adj[agent.ID] {
		if et.sourceHandle != EdgeHandleTool {
			continue
		}
		toolEdges++
		target, ok := env.byID[et.targetID]
		if !ok {
			slog.Warn("ai_agent: tool edge target missing", "agent", agent.ID, "target", et.targetID)
			continue
		}
		def, ok := asToolDef(target, "")
		if !ok {
			slog.Warn("ai_agent: tool edge target did not opt in (data.as_tool missing or disabled)",
				"agent", agent.ID, "target", target.ID, "type", target.Type)
			continue // node didn't opt in
		}
		optedIn++
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
			// Pre-exec breakpoint: hold the agent's tool call until the
			// live UI sends a continue frame when this as_tool target is
			// in the breakpoint set. Skipped when no continueCh wired.
			if env, ok := envFromCtx(ctx); ok && env.isBreakpoint(targetNode.ID) && env.continueCh != nil {
				env.waitAtBreakpoint(ctx, targetNode)
			}

			// Pre-exec NODE-level approval gate. The BFS-side gate
			// never visits as_tool target nodes (they're agent-driven),
			// so mirror the gate here. Per-tool gate (`require_approval`
			// on the agent) already fired upstream — this is the
			// extra layer when the target node opted into
			// `require_node_approval`.
			if env, ok := envFromCtx(ctx); ok {
				approved, gateErr := e.preExecApproval(ctx, env, targetNode, argInput)
				if gateErr != nil {
					return "", gateErr
				}
				if !approved {
					rejErr := "rejected by user (node approval)"
					if env.events != nil {
						env.events.Emit(stampNow(RunEvent{
							Type:     EventStepStart,
							NodeID:   targetNode.ID,
							NodeType: targetNode.Type,
						}))
						env.events.Emit(stampNow(RunEvent{
							Type:     EventStepDone,
							NodeID:   targetNode.ID,
							NodeType: targetNode.Type,
							Error:    rejErr,
							IsError:  true,
						}))
					}
					if env.toolSteps != nil {
						env.toolSteps.add(StepResult{
							NodeID:   targetNode.ID,
							NodeType: targetNode.Type,
							Error:    rejErr,
						})
					}
					return "", fmt.Errorf("node approval rejected for %s", targetNode.ID)
				}
			}

			// Emit step_start for the tool-invoked node so live-stream
			// clients see it transition into running. BFS skips these
			// nodes (they're agent-driven), so without this hook the
			// canvas stays grey on tool calls until the final snapshot.
			if env, ok := envFromCtx(ctx); ok && env.events != nil {
				env.events.Emit(stampNow(RunEvent{
					Type:     EventStepStart,
					NodeID:   targetNode.ID,
					NodeType: targetNode.Type,
				}))
			}

			out, err := e.runNode(ctx, targetNode, argInput, wfCtx, params)

			// Record a StepResult for the tool-invoked node so the UI shows
			// success/error on it (otherwise NodeDebugPanel renders "not executed").
			// ViaAgentTool=true keeps this Error from being scanned by the
			// run-status promotion logic — the agent itself owns the abort
			// decision (`stop_on_tool_error` or sandbox-infra classification).
			if env, ok := envFromCtx(ctx); ok && env.toolSteps != nil {
				sr := StepResult{NodeID: targetNode.ID, NodeType: targetNode.Type, Output: out, ViaAgentTool: true}
				if err != nil {
					sr.Error = err.Error()
				}
				env.toolSteps.add(sr)
			}

			// Mirror step_done so the canvas flips to success/error live.
			if env, ok := envFromCtx(ctx); ok && env.events != nil {
				done := RunEvent{
					Type:     EventStepDone,
					NodeID:   targetNode.ID,
					NodeType: targetNode.Type,
					Output:   out,
				}
				if err != nil {
					done.Error = err.Error()
					done.IsError = true
				}
				env.events.Emit(stampNow(done))
			}

			// Post-exec breakpoint fallback: only activates when no
			// continueCh is wired (i.e. caller didn't enable pre-exec).
			// Pre-exec already paused the agent before runNode; setting
			// stopAtHit again would force a second pause + persist a
			// PausedAgent we don't need.
			if env, ok := envFromCtx(ctx); ok && env.isBreakpoint(targetNode.ID) && env.continueCh == nil {
				env.stopAtHit = true
			}

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
		// Index for the approval gate so a rejection can flip the
		// corresponding canvas node to "rejected" instead of leaving it
		// stuck on "not executed".
		if env.toolNameToNodeID == nil {
			env.toolNameToNodeID = make(map[string]string, optedIn)
		}
		env.toolNameToNodeID[def.Name] = targetNode.ID
	}

	slog.Info("ai_agent: tool catalog built",
		"agent", agent.ID, "tool_edges", toolEdges, "opted_in", optedIn, "total_tools", len(cat.Defs()))
	return cat, nil
}

// --- helpers ---

// traceToRunEvent converts an agent TraceEvent into a streaming RunEvent.
// Done here (not on TraceEvent itself) so the agent package owns the
// trace shape and the run-level stream owns its own. NodeID is stamped
// from the agent node so consumers can route events into the correct
// per-node UI slot.
func traceToRunEvent(t TraceEvent, agentNode Node) RunEvent {
	ev := RunEvent{
		At:       t.At,
		NodeID:   agentNode.ID,
		NodeType: agentNode.Type,
		Iter:     t.Iter,
		Text:     t.Text,
		ToolName: t.ToolName,
		ToolID:   t.ToolID,
		ToolArgs: t.ToolArgs,
		Result:   stringifyResult(t.Result),
		IsError:  t.IsError,
		Provider: t.Provider,
		Model:    t.Model,
	}
	if t.Usage != nil {
		ev.Usage = &Usage{
			InputTokens:  t.Usage.InputTokens,
			OutputTokens: t.Usage.OutputTokens,
			TotalTokens:  t.Usage.TotalTokens,
			CostUSD:      t.Usage.CostUSD,
		}
	}
	switch t.Type {
	case "iter_start":
		ev.Type = EventAgentIter
	case "llm_call":
		ev.Type = EventAgentLLM
	case "tool_call":
		ev.Type = EventAgentToolCall
	case "tool_result":
		ev.Type = EventAgentToolResult
	case "final":
		ev.Type = EventAgentFinal
	}
	return ev
}

// stringifyResult coerces a trace-result value (string OR JSON-shaped any)
// into a single string for streaming consumers. Pre-stringified handlers
// pass through unchanged; structured payloads land as compact JSON so
// the UI can render them verbatim.
func stringifyResult(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

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

func getBoolData(data map[string]any, key string) bool {
	if v, ok := data[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

// waitForApproval blocks until an ApprovalDecision matching toolCallID
// arrives on ch, or the context cancels. Decisions for other tool IDs
// are dropped (defensive — the gate is sequential per agent loop, so
// a mismatch here means a stale frame from a prior call). Returns
// (decision, true) on a hit, (zero, false) on ctx cancel.
func waitForApproval(ctx context.Context, ch chan ApprovalDecision, toolCallID string) (ApprovalDecision, bool) {
	for {
		select {
		case <-ctx.Done():
			return ApprovalDecision{}, false
		case d := <-ch:
			if d.ToolCallID == "" || d.ToolCallID == toolCallID {
				return d, true
			}
			// Stale decision for a prior tool call — drop and keep waiting.
		}
	}
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

