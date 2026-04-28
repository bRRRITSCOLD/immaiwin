package llm

import (
	"context"
	"encoding/json"
	"errors"
)

// Provider is the minimal contract that every LLM backend must implement.
// Implementations live in subpackages (anthropic, openai, ollama, ...).
//
// The shape is intentionally small: a provider-agnostic abstraction means
// fewer leaks of vendor specifics into the agent loop. Provider-specific
// behaviors (e.g. Anthropic's prompt-caching directives) can be passed
// through the request via Extra fields ignored by other providers.
type Provider interface {
	// Name returns the provider identifier (e.g. "anthropic", "openai").
	Name() string

	// Chat sends a turn-based message list with optional tool defs and
	// returns either text (StopReason=end_turn) or tool calls
	// (StopReason=tool_use).
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)

	// ChatStream streams events for live UI. Optional in v1; agent loop
	// uses Chat until streaming UI is required (Phase 3 of agent plan).
	ChatStream(ctx context.Context, req ChatRequest) (<-chan ChatEvent, error)
}

// Role identifies who produced a message in the conversation.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	// Note: system is conveyed via ChatRequest.System, not as a Role here.
	// Tool results are conveyed as a User message containing ToolResult content blocks.
)

// ContentType is the kind of content block within a Message.
// We adopt Anthropic's content-block model because it's more expressive
// than OpenAI's flat content+tool_calls split. Other providers translate
// in their adapter.
type ContentType string

const (
	ContentTypeText       ContentType = "text"
	ContentTypeToolUse    ContentType = "tool_use"
	ContentTypeToolResult ContentType = "tool_result"
)

// Content is a single block within a Message. Use the helpers (TextBlock,
// ToolUseBlock, ToolResultBlock) to construct rather than building structs
// by hand.
type Content struct {
	Type ContentType `json:"type"`

	// Text — for ContentTypeText
	Text string `json:"text,omitempty"`

	// Tool use — for ContentTypeToolUse (model-emitted call)
	ID    string          `json:"id,omitempty"`     // tool_use_id; correlates with ToolResult
	Name  string          `json:"name,omitempty"`   // tool name
	Input json.RawMessage `json:"input,omitempty"`  // tool args (raw JSON; caller decodes)

	// Tool result — for ContentTypeToolResult (caller-produced observation)
	ToolUseID string `json:"tool_use_id,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
	// ToolResult content can be string OR structured; keep as raw text for v1.
	ResultText string `json:"result_text,omitempty"`
}

// Message is one turn in a chat history.
type Message struct {
	Role    Role      `json:"role"`
	Content []Content `json:"content"`
}

// ToolDef describes a callable surface to the LLM. The InputSchema is a
// JSON-Schema object that the LLM's structured-output mechanism uses to
// emit valid tool args.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ChatRequest is the universal chat input.
type ChatRequest struct {
	Model       string    `json:"model"`
	System      string    `json:"system,omitempty"`
	Messages    []Message `json:"messages"`
	Tools       []ToolDef `json:"tools,omitempty"`
	ToolChoice  string    `json:"tool_choice,omitempty"`  // "auto" | "any" | "none" | "<tool name>"
	Temperature *float64  `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	StopSeqs    []string  `json:"stop_sequences,omitempty"`

	// Extra is a provider-specific passthrough bag. Providers ignore keys
	// they don't recognize. Use sparingly; favors at the abstraction layer.
	Extra map[string]any `json:"-"`
}

// StopReason explains why the model stopped generating. Provider-agnostic;
// adapters map vendor-specific reasons onto this enum.
type StopReason string

const (
	StopReasonEndTurn   StopReason = "end_turn"   // model produced a final message
	StopReasonToolUse   StopReason = "tool_use"   // model wants to call one or more tools
	StopReasonMaxTokens StopReason = "max_tokens" // hit the response cap
	StopReasonStopSeq   StopReason = "stop_sequence"
	StopReasonError     StopReason = "error"
)

// ChatResponse is the universal chat output.
type ChatResponse struct {
	StopReason StopReason `json:"stop_reason"`
	Content    []Content  `json:"content"` // text + tool_use blocks
	Usage      Usage      `json:"usage"`
	Model      string     `json:"model"` // echo of the model that responded (helps when an alias was used)
	Raw        []byte     `json:"-"`     // raw provider response for audit/replay
}

// Usage tracks tokens and estimated cost for one turn. Cost in USD.
// Cost may be zero when the provider doesn't report pricing.
type Usage struct {
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	TotalTokens  int     `json:"total_tokens"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
}

// ChatEvent is one item from a streaming chat. Used by ChatStream.
// Event types intentionally narrow; expand as streaming UI grows.
type ChatEvent struct {
	Type ChatEventType `json:"type"`

	// Delta — for ChatEventTypeText
	Delta string `json:"delta,omitempty"`

	// ToolUse — for ChatEventTypeToolUseStart / ToolUseDelta / ToolUseStop
	ToolUse *Content `json:"tool_use,omitempty"`

	// Message — for ChatEventTypeMessageStop
	StopReason StopReason `json:"stop_reason,omitempty"`
	Usage      *Usage     `json:"usage,omitempty"`

	// Err — for ChatEventTypeError
	Err error `json:"-"`
}

type ChatEventType string

const (
	ChatEventTypeMessageStart  ChatEventType = "message_start"
	ChatEventTypeText          ChatEventType = "text"
	ChatEventTypeToolUseStart  ChatEventType = "tool_use_start"
	ChatEventTypeToolUseDelta  ChatEventType = "tool_use_delta"
	ChatEventTypeToolUseStop   ChatEventType = "tool_use_stop"
	ChatEventTypeMessageStop   ChatEventType = "message_stop"
	ChatEventTypeError         ChatEventType = "error"
)

// Block constructors — preferred way to build Content.
// ----------------------------------------------------

func TextBlock(s string) Content {
	return Content{Type: ContentTypeText, Text: s}
}

func ToolUseBlock(id, name string, input json.RawMessage) Content {
	return Content{Type: ContentTypeToolUse, ID: id, Name: name, Input: input}
}

func ToolResultBlock(toolUseID, resultText string, isError bool) Content {
	return Content{
		Type:       ContentTypeToolResult,
		ToolUseID:  toolUseID,
		ResultText: resultText,
		IsError:    isError,
	}
}

// Message constructors — common shapes.
// -------------------------------------

func UserText(s string) Message {
	return Message{Role: RoleUser, Content: []Content{TextBlock(s)}}
}

func AssistantText(s string) Message {
	return Message{Role: RoleAssistant, Content: []Content{TextBlock(s)}}
}

// ToolResultMessage wraps tool results in a single user-role message,
// matching Anthropic's expected layout. One message can carry multiple
// tool-result blocks if the model emitted parallel tool calls.
func ToolResultMessage(blocks []Content) Message {
	return Message{Role: RoleUser, Content: blocks}
}

// Errors
// ------

// ErrToolNotImplemented is returned when a request's Tools field is non-empty
// but the provider does not support tool use (e.g. some local Ollama models).
var ErrToolNotImplemented = errors.New("llm: provider does not support tool use")

// ErrStreamingNotImplemented is returned when ChatStream is called on a
// provider that has not implemented streaming yet. Callers can fall back
// to Chat in that case.
var ErrStreamingNotImplemented = errors.New("llm: provider does not support streaming")
