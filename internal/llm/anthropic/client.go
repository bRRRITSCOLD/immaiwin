// Package anthropic implements llm.Provider against Anthropic's Messages API.
//
// Endpoint: https://api.anthropic.com/v1/messages
// Auth:     x-api-key + anthropic-version headers
// Reference: https://docs.anthropic.com/en/api/messages
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/llm"
)

const (
	defaultEndpoint = "https://api.anthropic.com"
	defaultVersion  = "2023-06-01"
	defaultModel    = "claude-sonnet-4-6" // sweet spot for agent loops; opus-4-7 available via override
	defaultMaxTok   = 4096
)

// Client is the Anthropic Provider implementation.
type Client struct {
	apiKey       string
	endpoint     string
	version      string
	defaultModel string
	http         *http.Client
}

// New constructs an Anthropic client from a connection's config map.
//
// Required config keys:
//   - api_key:       Anthropic API key
//
// Optional:
//   - endpoint:      override base URL (gateways, proxies)
//   - version:       override anthropic-version header
//   - default_model: override default Claude model
//   - timeout:       override HTTP client timeout (e.g. "60s")
func New(config map[string]string) (llm.Provider, error) {
	apiKey := config["api_key"]
	if apiKey == "" {
		return nil, fmt.Errorf("anthropic: api_key required")
	}

	endpoint := config["endpoint"]
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	endpoint = strings.TrimRight(endpoint, "/")

	version := config["version"]
	if version == "" {
		version = defaultVersion
	}

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
		version:      version,
		defaultModel: model,
		http:         &http.Client{Timeout: timeout},
	}, nil
}

// Name returns "anthropic".
func (c *Client) Name() string { return "anthropic" }

// ChatStream is not yet implemented; agent loop uses Chat in P1.
func (c *Client) ChatStream(ctx context.Context, req llm.ChatRequest) (<-chan llm.ChatEvent, error) {
	return nil, llm.ErrStreamingNotImplemented
}

// --- Anthropic wire types (only what we use) ---

type apiContent struct {
	Type string `json:"type"`

	// type=text
	Text string `json:"text,omitempty"`

	// type=tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// type=tool_result (request payload)
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

type apiMessage struct {
	Role    string       `json:"role"`
	Content []apiContent `json:"content"`
}

type apiTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type apiToolChoice struct {
	Type string `json:"type"` // auto | any | tool
	Name string `json:"name,omitempty"`
}

type apiRequest struct {
	Model         string         `json:"model"`
	System        string         `json:"system,omitempty"`
	Messages      []apiMessage   `json:"messages"`
	Tools         []apiTool      `json:"tools,omitempty"`
	ToolChoice    *apiToolChoice `json:"tool_choice,omitempty"`
	MaxTokens     int            `json:"max_tokens"`
	Temperature   *float64       `json:"temperature,omitempty"`
	StopSequences []string       `json:"stop_sequences,omitempty"`
}

type apiUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type apiResponse struct {
	ID         string       `json:"id"`
	Type       string       `json:"type"`
	Role       string       `json:"role"`
	Model      string       `json:"model"`
	Content    []apiContent `json:"content"`
	StopReason string       `json:"stop_reason"`
	Usage      apiUsage     `json:"usage"`
}

type apiError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type apiErrorEnvelope struct {
	Type  string   `json:"type"`
	Error apiError `json:"error"`
}

// --- Conversion: llm <-> anthropic ---

func toAPIMessages(msgs []llm.Message) []apiMessage {
	out := make([]apiMessage, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, apiMessage{
			Role:    string(m.Role),
			Content: toAPIContent(m.Content),
		})
	}
	return out
}

func toAPIContent(blocks []llm.Content) []apiContent {
	out := make([]apiContent, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case llm.ContentTypeText:
			out = append(out, apiContent{Type: "text", Text: b.Text})
		case llm.ContentTypeToolUse:
			out = append(out, apiContent{
				Type:  "tool_use",
				ID:    b.ID,
				Name:  b.Name,
				Input: b.Input,
			})
		case llm.ContentTypeToolResult:
			out = append(out, apiContent{
				Type:      "tool_result",
				ToolUseID: b.ToolUseID,
				Content:   b.ResultText,
				IsError:   b.IsError,
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
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}
	return out
}

func toAPIToolChoice(s string) *apiToolChoice {
	switch s {
	case "", "auto":
		return nil // auto is the default; omit to save bytes
	case "any":
		return &apiToolChoice{Type: "any"}
	case "none":
		// Anthropic represents "none" by omitting tools entirely; the
		// caller should set Tools=nil in that case. If we get here we
		// simply return nil and let the empty Tools array do the work.
		return nil
	default:
		return &apiToolChoice{Type: "tool", Name: s}
	}
}

func fromAPIContent(blocks []apiContent) []llm.Content {
	out := make([]llm.Content, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "text":
			out = append(out, llm.TextBlock(b.Text))
		case "tool_use":
			out = append(out, llm.ToolUseBlock(b.ID, b.Name, b.Input))
		}
	}
	return out
}

func fromAPIStopReason(r string) llm.StopReason {
	switch r {
	case "end_turn":
		return llm.StopReasonEndTurn
	case "tool_use":
		return llm.StopReasonToolUse
	case "max_tokens":
		return llm.StopReasonMaxTokens
	case "stop_sequence":
		return llm.StopReasonStopSeq
	default:
		return llm.StopReason(r)
	}
}

// --- Pricing table (USD per 1M tokens). Best-effort, refresh as needed.
// Used purely for cost estimates surfaced in Usage.CostUSD.

var pricing = map[string]struct{ inUSD, outUSD float64 }{
	"claude-opus-4-7":          {15.0, 75.0},
	"claude-sonnet-4-6":        {3.0, 15.0},
	"claude-haiku-4-5-20251001": {0.80, 4.0},
}

func estimateCost(model string, in, out int) float64 {
	p, ok := pricing[model]
	if !ok {
		return 0
	}
	return float64(in)/1_000_000*p.inUSD + float64(out)/1_000_000*p.outUSD
}

// --- Chat ---

// Chat sends a single non-streaming Messages request.
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
		Model:         model,
		System:        req.System,
		Messages:      toAPIMessages(req.Messages),
		Tools:         toAPITools(req.Tools),
		ToolChoice:    toAPIToolChoice(req.ToolChoice),
		MaxTokens:     maxTok,
		Temperature:   req.Temperature,
		StopSequences: req.StopSeqs,
	}

	body, err := json.Marshal(apiReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", c.version)
	httpReq.Header.Set("x-api-key", c.apiKey)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("anthropic: read body: %w", err)
	}

	if resp.StatusCode >= 400 {
		var env apiErrorEnvelope
		if jsonErr := json.Unmarshal(raw, &env); jsonErr == nil && env.Error.Message != "" {
			return nil, fmt.Errorf("anthropic: %d %s: %s", resp.StatusCode, env.Error.Type, env.Error.Message)
		}
		return nil, fmt.Errorf("anthropic: %d: %s", resp.StatusCode, truncate(string(raw), 500))
	}

	var apiResp apiResponse
	if err := json.Unmarshal(raw, &apiResp); err != nil {
		return nil, fmt.Errorf("anthropic: unmarshal response: %w", err)
	}

	usage := llm.Usage{
		InputTokens:  apiResp.Usage.InputTokens,
		OutputTokens: apiResp.Usage.OutputTokens,
		TotalTokens:  apiResp.Usage.InputTokens + apiResp.Usage.OutputTokens,
		CostUSD:      estimateCost(apiResp.Model, apiResp.Usage.InputTokens, apiResp.Usage.OutputTokens),
	}

	return &llm.ChatResponse{
		StopReason: fromAPIStopReason(apiResp.StopReason),
		Content:    fromAPIContent(apiResp.Content),
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
