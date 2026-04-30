// Package ollama implements llm.Provider against a local (or remote) Ollama
// server's /api/chat endpoint.
//
// Endpoint default: http://localhost:11434
// Auth:             none (Ollama is unauthenticated by design; secure with
//                   reverse-proxy / VPN if exposed)
// Reference: https://github.com/ollama/ollama/blob/main/docs/api.md
//
// Cost is always 0 — local inference.
//
// Tool calling is supported for compatible models (llama3.1+, qwen2.5,
// mistral-nemo, etc.). Models without tool support will silently ignore
// the `tools` array — the agent loop's `max_iterations` cap protects
// against runaway non-tool runs.
//
// Differences vs OpenAI:
//   - tool_calls have no `id` from the server; we synthesise one
//     (`<sanitised-name>_<idx>`) so the agent loop has a stable correlation
//     key for its own bookkeeping (approval gate, trace events).
//   - tool result messages don't carry tool_call_id; Ollama relies purely
//     on positional / textual context to associate the result with its
//     prior call.
//   - arguments come back as a JSON OBJECT (not a string like OpenAI).
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/llm"
)

const (
	defaultEndpoint = "http://localhost:11434"
	defaultModel    = "llama3.1" // tool-capable; override per-connection or per-agent
	defaultMaxTok   = 4096
)

// Client is the Ollama Provider implementation.
type Client struct {
	endpoint     string
	defaultModel string
	keepAlive    string
	http         *http.Client
}

// New constructs an Ollama client from a connection's config map.
//
// All fields optional:
//   - endpoint:      override base URL (default http://localhost:11434)
//   - default_model: override default chat model (e.g. llama3.1, qwen2.5:7b)
//   - keep_alive:    e.g. "5m" — how long Ollama keeps the model warm
//   - timeout:       Go duration string for HTTP client (default 10m for
//                    cold-start tolerance — first call may need to load
//                    the model into VRAM).
func New(config map[string]string) (llm.Provider, error) {
	endpoint := config["endpoint"]
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	endpoint = strings.TrimRight(endpoint, "/")

	model := config["default_model"]
	if model == "" {
		model = defaultModel
	}

	timeout := 10 * time.Minute
	if t, ok := config["timeout"]; ok && t != "" {
		if d, err := time.ParseDuration(t); err == nil {
			timeout = d
		}
	}

	return &Client{
		endpoint:     endpoint,
		defaultModel: model,
		keepAlive:    config["keep_alive"],
		http:         &http.Client{Timeout: timeout},
	}, nil
}

// Name returns "ollama".
func (c *Client) Name() string { return "ollama" }

// ChatStream is not yet implemented.
func (c *Client) ChatStream(ctx context.Context, req llm.ChatRequest) (<-chan llm.ChatEvent, error) {
	return nil, llm.ErrStreamingNotImplemented
}

// --- Ollama wire types (only what we use) ---

type apiToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type apiTool struct {
	Type     string          `json:"type"` // always "function"
	Function apiToolFunction `json:"function"`
}

type apiToolCallFunc struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"` // OBJECT, not string (vs OpenAI)
}

type apiToolCall struct {
	Function apiToolCallFunc `json:"function"`
}

type apiMessage struct {
	Role      string        `json:"role"` // "system" | "user" | "assistant" | "tool"
	Content   string        `json:"content"`
	ToolCalls []apiToolCall `json:"tool_calls,omitempty"`
	// Ollama doesn't require a tool_call_id on role=tool messages, but
	// pass-through if present (newer builds may use it).
	ToolName string `json:"name,omitempty"`
}

type apiOptions struct {
	Temperature *float64 `json:"temperature,omitempty"`
	NumPredict  int      `json:"num_predict,omitempty"`
	Stop        []string `json:"stop,omitempty"`
}

type apiRequest struct {
	Model     string       `json:"model"`
	Messages  []apiMessage `json:"messages"`
	Tools     []apiTool    `json:"tools,omitempty"`
	Stream    bool         `json:"stream"`
	KeepAlive string       `json:"keep_alive,omitempty"`
	Options   *apiOptions  `json:"options,omitempty"`
}

type apiResponse struct {
	Model            string     `json:"model"`
	Message          apiMessage `json:"message"`
	Done             bool       `json:"done"`
	DoneReason       string     `json:"done_reason"`
	PromptEvalCount  int        `json:"prompt_eval_count"`
	EvalCount        int        `json:"eval_count"`
}

type apiError struct {
	Error string `json:"error"`
}

// --- Conversion: llm <-> ollama ---

// toAPIMessages translates llm.Message slices, unfolding tool_result blocks
// into separate role=tool messages and folding assistant tool_use blocks
// into the assistant message's tool_calls field.
func toAPIMessages(system string, msgs []llm.Message) []apiMessage {
	out := make([]apiMessage, 0, len(msgs)+1)
	if system != "" {
		out = append(out, apiMessage{Role: "system", Content: system})
	}
	for _, m := range msgs {
		switch m.Role {
		case llm.RoleUser:
			var text strings.Builder
			var toolResults []llm.Content
			for _, b := range m.Content {
				switch b.Type {
				case llm.ContentTypeText:
					if text.Len() > 0 {
						text.WriteString("\n")
					}
					text.WriteString(b.Text)
				case llm.ContentTypeToolResult:
					toolResults = append(toolResults, b)
				}
			}
			if text.Len() > 0 {
				out = append(out, apiMessage{Role: "user", Content: text.String()})
			}
			for _, tr := range toolResults {
				content := tr.ResultText
				if tr.IsError {
					content = "ERROR: " + content
				}
				out = append(out, apiMessage{
					Role:    "tool",
					Content: content,
				})
			}

		case llm.RoleAssistant:
			var text strings.Builder
			var calls []apiToolCall
			for _, b := range m.Content {
				switch b.Type {
				case llm.ContentTypeText:
					if text.Len() > 0 {
						text.WriteString("\n")
					}
					text.WriteString(b.Text)
				case llm.ContentTypeToolUse:
					args := b.Input
					if len(args) == 0 {
						args = json.RawMessage("{}")
					}
					calls = append(calls, apiToolCall{
						Function: apiToolCallFunc{
							Name:      b.Name,
							Arguments: args,
						},
					})
				}
			}
			out = append(out, apiMessage{
				Role:      "assistant",
				Content:   text.String(),
				ToolCalls: calls,
			})
		}
	}
	return out
}

func toAPITools(tools []llm.ToolDef) []apiTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]apiTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, apiTool{
			Type: "function",
			Function: apiToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}
	return out
}

// fromAPIMessage converts an Ollama assistant turn into llm.Content blocks.
// Synthesises stable IDs for tool calls (Ollama doesn't emit them) so the
// agent loop's bookkeeping (approval gate, trace events) has a correlation
// key.
func fromAPIMessage(m apiMessage) []llm.Content {
	var blocks []llm.Content
	if m.Content != "" {
		blocks = append(blocks, llm.TextBlock(m.Content))
	}
	for i, tc := range m.ToolCalls {
		id := fmt.Sprintf("%s_%d", sanitiseID(tc.Function.Name), i)
		args := tc.Function.Arguments
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		blocks = append(blocks, llm.ToolUseBlock(id, tc.Function.Name, args))
	}
	return blocks
}

// fromAPIDoneReason maps Ollama's done_reason onto our StopReason. Ollama
// tool calls don't emit a dedicated done_reason — when tool_calls is
// non-empty the caller decides on StopReasonToolUse regardless of this.
func fromAPIDoneReason(r string) llm.StopReason {
	switch r {
	case "stop", "":
		return llm.StopReasonEndTurn
	case "length":
		return llm.StopReasonMaxTokens
	default:
		return llm.StopReason(r)
	}
}

// sanitiseID replaces non-alphanumeric chars with `_` so synthesised tool
// IDs stay valid in any downstream consumer that cares (e.g. our trace
// emit + UI tool-call key).
func sanitiseID(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "tool"
	}
	return b.String()
}

// --- Chat ---

func (c *Client) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	model := req.Model
	if model == "" {
		model = c.defaultModel
	}
	maxTok := req.MaxTokens
	if maxTok <= 0 {
		maxTok = defaultMaxTok
	}

	apiReq := apiRequest{
		Model:    model,
		Messages: toAPIMessages(req.System, req.Messages),
		Tools:    toAPITools(req.Tools),
		Stream:   false,
		KeepAlive: c.keepAlive,
		Options: &apiOptions{
			Temperature: req.Temperature,
			NumPredict:  maxTok,
			Stop:        req.StopSeqs,
		},
	}

	body, err := json.Marshal(apiReq)
	if err != nil {
		return nil, fmt.Errorf("ollama: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ollama: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	// Transient-error retry loop. Ollama is local so 429 is unusual.
	// Retry 429/503 with exponential backoff. 500 omitted — those are
	// usually hard model errors (OOM, missing weights) that won't
	// recover on retry. No Retry-After header from Ollama.
	const maxRetries = 3
	var resp *http.Response
	var raw []byte
	for attempt := 0; ; attempt++ {
		retryReq, rErr := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/api/chat", bytes.NewReader(body))
		if rErr != nil {
			return nil, fmt.Errorf("ollama: build request: %w", rErr)
		}
		retryReq.Header = httpReq.Header.Clone()

		var doErr error
		resp, doErr = c.http.Do(retryReq)
		if doErr != nil {
			return nil, fmt.Errorf("ollama: do: %w", doErr)
		}
		raw, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("ollama: read body: %w", err)
		}

		retryable := resp.StatusCode == http.StatusTooManyRequests ||
			resp.StatusCode == http.StatusServiceUnavailable
		if !retryable || attempt >= maxRetries {
			break
		}

		wait := time.Duration(1<<attempt) * time.Second
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	if resp.StatusCode >= 400 {
		var env apiError
		if jsonErr := json.Unmarshal(raw, &env); jsonErr == nil && env.Error != "" {
			return nil, fmt.Errorf("ollama: %d: %s", resp.StatusCode, env.Error)
		}
		return nil, fmt.Errorf("ollama: %d: %s", resp.StatusCode, truncate(string(raw), 500))
	}

	var apiResp apiResponse
	if err := json.Unmarshal(raw, &apiResp); err != nil {
		return nil, fmt.Errorf("ollama: unmarshal response: %w", err)
	}

	blocks := fromAPIMessage(apiResp.Message)
	stop := fromAPIDoneReason(apiResp.DoneReason)
	if len(apiResp.Message.ToolCalls) > 0 {
		stop = llm.StopReasonToolUse
	}

	usage := llm.Usage{
		InputTokens:  apiResp.PromptEvalCount,
		OutputTokens: apiResp.EvalCount,
		TotalTokens:  apiResp.PromptEvalCount + apiResp.EvalCount,
		// Cost = 0 (local inference).
	}

	return &llm.ChatResponse{
		StopReason: stop,
		Content:    blocks,
		Usage:      usage,
		Model:      apiResp.Model,
		Raw:        raw,
	}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
