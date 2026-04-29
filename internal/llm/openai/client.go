// Package openai implements llm.Provider against OpenAI's Chat Completions API.
//
// Endpoint: https://api.openai.com/v1/chat/completions
// Auth:     Authorization: Bearer <api_key>
// Reference: https://platform.openai.com/docs/api-reference/chat/create
//
// Tool-use shape differs from Anthropic:
//   - assistant turn carries `tool_calls: [{id, type:"function", function:{name, arguments}}]`
//   - tool results live as separate messages with role=tool + tool_call_id + content
//
// Our llm.Content model follows Anthropic's block layout, so this adapter
// unfolds tool_result blocks (carried inside a role=user llm.Message) into
// N role=tool messages and assembles assistant tool_calls + a text content
// string from the assistant's mixed text/tool_use blocks.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/llm"
)

const (
	defaultEndpoint = "https://api.openai.com"
	defaultModel    = "gpt-4o-mini" // cheap default for agent loops; gpt-4o / gpt-4-turbo via override
	defaultMaxTok   = 4096
)

// Client is the OpenAI Provider implementation.
type Client struct {
	apiKey       string
	endpoint     string
	defaultModel string
	organization string
	project      string
	http         *http.Client
}

// New constructs an OpenAI client from a connection's config map.
//
// Required:
//   - api_key:       OpenAI API key
//
// Optional:
//   - endpoint:      override base URL (Azure / proxies)
//   - default_model: override default chat model (e.g. gpt-4o)
//   - organization:  org id (sent as OpenAI-Organization header)
//   - project:       project id (sent as OpenAI-Project header)
//   - timeout:       Go duration string (e.g. "60s")
func New(config map[string]string) (llm.Provider, error) {
	apiKey := config["api_key"]
	if apiKey == "" {
		return nil, fmt.Errorf("openai: api_key required")
	}

	endpoint := config["endpoint"]
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	endpoint = strings.TrimRight(endpoint, "/")

	model := config["default_model"]
	if model == "" {
		model = defaultModel
	}

	timeout := 90 * time.Second
	if t, ok := config["timeout"]; ok && t != "" {
		if d, err := time.ParseDuration(t); err == nil {
			timeout = d
		}
	}

	return &Client{
		apiKey:       apiKey,
		endpoint:     endpoint,
		defaultModel: model,
		organization: config["organization"],
		project:      config["project"],
		http:         &http.Client{Timeout: timeout},
	}, nil
}

// Name returns "openai".
func (c *Client) Name() string { return "openai" }

// ChatStream is not yet implemented.
func (c *Client) ChatStream(ctx context.Context, req llm.ChatRequest) (<-chan llm.ChatEvent, error) {
	return nil, llm.ErrStreamingNotImplemented
}

// --- OpenAI wire types (only what we use) ---

type apiToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type apiTool struct {
	Type     string          `json:"type"` // "function"
	Function apiToolFunction `json:"function"`
}

// apiToolCall is what the assistant emits AND what we send back in subsequent
// turns. `Arguments` is a JSON-encoded string per OpenAI spec.
type apiToolCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"` // "function"
	Function apiToolCallFunc `json:"function"`
}

type apiToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type apiMessage struct {
	Role       string         `json:"role"` // "system" | "user" | "assistant" | "tool"
	Content    any            `json:"content,omitempty"` // string | null
	ToolCalls  []apiToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"` // role=tool only
	Name       string         `json:"name,omitempty"`
}

type apiToolChoiceObj struct {
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}

type apiRequest struct {
	Model       string       `json:"model"`
	Messages    []apiMessage `json:"messages"`
	Tools       []apiTool    `json:"tools,omitempty"`
	ToolChoice  any          `json:"tool_choice,omitempty"` // "auto"|"none"|"required"|object
	MaxTokens   int          `json:"max_tokens,omitempty"`
	Temperature *float64     `json:"temperature,omitempty"`
	Stop        []string     `json:"stop,omitempty"`
	Stream      bool         `json:"stream,omitempty"`
}

type apiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type apiChoice struct {
	Index        int        `json:"index"`
	Message      apiMessage `json:"message"`
	FinishReason string     `json:"finish_reason"`
}

type apiResponse struct {
	ID      string      `json:"id"`
	Model   string      `json:"model"`
	Choices []apiChoice `json:"choices"`
	Usage   apiUsage    `json:"usage"`
}

type apiError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Code    string `json:"code"`
}

type apiErrorEnvelope struct {
	Error apiError `json:"error"`
}

// --- Conversion: llm <-> openai ---

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
			// Tool results land here in our model. Detect: if every block is
			// ContentTypeToolResult, unfold into N role=tool messages. Mixed
			// text + tool_result is unusual; we emit the text as a separate
			// user message followed by tool messages.
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
				// OpenAI doesn't have an `is_error` field on tool messages —
				// surface the failure inline so the model can see it.
				if tr.IsError {
					content = "ERROR: " + content
				}
				out = append(out, apiMessage{
					Role:       "tool",
					ToolCallID: tr.ToolUseID,
					Content:    content,
				})
			}
			// Empty user message (no text, no tool_results) — skip; OpenAI
			// rejects messages with no content.
			if text.Len() == 0 && len(toolResults) == 0 {
				continue
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
					// OpenAI requires arguments as a JSON-encoded STRING, not
					// a nested object. Our wire shape stores the model's raw
					// JSON; pass it through verbatim.
					argsStr := string(b.Input)
					if argsStr == "" {
						argsStr = "{}"
					}
					calls = append(calls, apiToolCall{
						ID:   b.ID,
						Type: "function",
						Function: apiToolCallFunc{
							Name:      b.Name,
							Arguments: argsStr,
						},
					})
				}
			}
			msg := apiMessage{Role: "assistant"}
			if text.Len() > 0 {
				msg.Content = text.String()
			} else {
				// OpenAI requires either content or tool_calls; null content
				// is allowed when tool_calls is non-empty.
				msg.Content = nil
			}
			if len(calls) > 0 {
				msg.ToolCalls = calls
			}
			out = append(out, msg)
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

// toAPIToolChoice maps our string knob onto OpenAI's tool_choice field.
//   - "" / "auto" → omit (default is auto when tools present)
//   - "any"        → "required"
//   - "none"       → "none"
//   - <tool name>  → object form {type:"function", function:{name:"…"}}
func toAPIToolChoice(s string) any {
	switch s {
	case "", "auto":
		return nil
	case "any":
		return "required"
	case "none":
		return "none"
	default:
		obj := apiToolChoiceObj{Type: "function"}
		obj.Function.Name = s
		return obj
	}
}

// fromAPIChoice converts an OpenAI assistant message into our content blocks.
// finish_reason maps onto our StopReason enum.
func fromAPIChoice(ch apiChoice) ([]llm.Content, llm.StopReason) {
	var blocks []llm.Content
	if s, ok := ch.Message.Content.(string); ok && s != "" {
		blocks = append(blocks, llm.TextBlock(s))
	}
	for _, tc := range ch.Message.ToolCalls {
		blocks = append(blocks, llm.ToolUseBlock(tc.ID, tc.Function.Name, json.RawMessage(tc.Function.Arguments)))
	}
	stop := fromAPIFinishReason(ch.FinishReason)
	return blocks, stop
}

func fromAPIFinishReason(r string) llm.StopReason {
	switch r {
	case "stop":
		return llm.StopReasonEndTurn
	case "tool_calls":
		return llm.StopReasonToolUse
	case "length":
		return llm.StopReasonMaxTokens
	default:
		return llm.StopReason(r)
	}
}

// --- Pricing table (USD per 1M tokens). Best-effort, refresh as needed.
// Source: https://openai.com/api/pricing — as of 2026-04 lineup. Keys
// match the model id OpenAI echoes back in the response.

var pricing = map[string]struct{ inUSD, outUSD float64 }{
	// 4o family
	"gpt-4o":             {2.50, 10.00},
	"gpt-4o-2024-08-06":  {2.50, 10.00},
	"gpt-4o-mini":        {0.15, 0.60},
	"gpt-4o-mini-2024-07-18": {0.15, 0.60},
	// 4-turbo / 4
	"gpt-4-turbo":        {10.00, 30.00},
	"gpt-4-turbo-2024-04-09": {10.00, 30.00},
	"gpt-4":              {30.00, 60.00},
	// 3.5
	"gpt-3.5-turbo":      {0.50, 1.50},
	"gpt-3.5-turbo-0125": {0.50, 1.50},
}

func estimateCost(model string, in, out int) float64 {
	p, ok := pricing[model]
	if !ok {
		// Try a longest-prefix match so dated revisions still get billed.
		for k, v := range pricing {
			if strings.HasPrefix(model, k) {
				p = v
				ok = true
				break
			}
		}
	}
	if !ok {
		return 0
	}
	return float64(in)/1_000_000*p.inUSD + float64(out)/1_000_000*p.outUSD
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
		Model:       model,
		Messages:    toAPIMessages(req.System, req.Messages),
		Tools:       toAPITools(req.Tools),
		ToolChoice:  toAPIToolChoice(req.ToolChoice),
		MaxTokens:   maxTok,
		Temperature: req.Temperature,
		Stop:        req.StopSeqs,
	}

	body, err := json.Marshal(apiReq)
	if err != nil {
		return nil, fmt.Errorf("openai: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	if c.organization != "" {
		httpReq.Header.Set("OpenAI-Organization", c.organization)
	}
	if c.project != "" {
		httpReq.Header.Set("OpenAI-Project", c.project)
	}

	// 429 retry loop. OpenAI's 429 covers BOTH true rate-limit bursts
	// (TPM/RPM exceeded for the minute) AND `insufficient_quota` (account
	// tier cap or monthly spend ceiling). Bursts respond well to short
	// backoff; the quota case never recovers on retry, so we cap retries
	// at 3 and let the caller surface a friendlier error.
	const maxRetries = 3
	var resp *http.Response
	var raw []byte
	for attempt := 0; ; attempt++ {
		// Re-build the request body each attempt — http.NewRequestWithContext
		// captures the Reader, and a re-Do without resetting would send empty.
		retryReq, rErr := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/v1/chat/completions", bytes.NewReader(body))
		if rErr != nil {
			return nil, fmt.Errorf("openai: build request: %w", rErr)
		}
		retryReq.Header = httpReq.Header.Clone()

		var doErr error
		resp, doErr = c.http.Do(retryReq)
		if doErr != nil {
			return nil, fmt.Errorf("openai: do: %w", doErr)
		}

		raw, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("openai: read body: %w", err)
		}

		if resp.StatusCode != http.StatusTooManyRequests || attempt >= maxRetries {
			break
		}

		// Honour Retry-After when present; cap at 30s. Otherwise exponential
		// backoff (1s, 2s, 4s). RFC 7231 says Retry-After is either an
		// integer (seconds) or HTTP-date — OpenAI uses integer seconds.
		wait := time.Duration(1<<attempt) * time.Second
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, perr := strconv.Atoi(strings.TrimSpace(ra)); perr == nil && secs > 0 && secs < 30 {
				wait = time.Duration(secs) * time.Second
			}
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	if resp.StatusCode >= 400 {
		var env apiErrorEnvelope
		if jsonErr := json.Unmarshal(raw, &env); jsonErr == nil && env.Error.Message != "" {
			// Friendlier error for insufficient_quota — users almost always
			// confuse it with "I'm out of credits" when in reality it's the
			// account tier or monthly spend cap.
			if env.Error.Code == "insufficient_quota" || strings.Contains(env.Error.Message, "exceeded your current quota") {
				return nil, fmt.Errorf("openai: %d insufficient_quota: account tier cap or monthly spend limit hit (NOT account credits — check tier at platform.openai.com/account/limits and usage at platform.openai.com/usage). raw: %s", resp.StatusCode, env.Error.Message)
			}
			return nil, fmt.Errorf("openai: %d %s: %s", resp.StatusCode, env.Error.Type, env.Error.Message)
		}
		return nil, fmt.Errorf("openai: %d: %s", resp.StatusCode, truncate(string(raw), 500))
	}

	var apiResp apiResponse
	if err := json.Unmarshal(raw, &apiResp); err != nil {
		return nil, fmt.Errorf("openai: unmarshal response: %w", err)
	}
	if len(apiResp.Choices) == 0 {
		return nil, fmt.Errorf("openai: no choices in response")
	}
	choice := apiResp.Choices[0]
	blocks, stopReason := fromAPIChoice(choice)

	usage := llm.Usage{
		InputTokens:  apiResp.Usage.PromptTokens,
		OutputTokens: apiResp.Usage.CompletionTokens,
		TotalTokens:  apiResp.Usage.TotalTokens,
		CostUSD:      estimateCost(apiResp.Model, apiResp.Usage.PromptTokens, apiResp.Usage.CompletionTokens),
	}

	return &llm.ChatResponse{
		StopReason: stopReason,
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
