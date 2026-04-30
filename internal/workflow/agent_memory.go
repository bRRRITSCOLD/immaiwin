package workflow

import (
	"context"

	"github.com/bRRRITSCOLD/burrow/internal/llm"
)

// AgentMemory persists agent conversations across runs.
//
// Memory is keyed by sessionID. The session is assigned per agent run,
// typically derived from a workflow param or trigger event (e.g.
// session_id = "user_42_chat_7"). Empty session = no persistence.
//
// The Mongo implementation lives in internal/mongodb/chat_memory.go.
type AgentMemory interface {
	// Load returns the most recent up-to-maxMessages messages, oldest first.
	// Returns an empty slice (not nil error) when sessionID has no history.
	Load(ctx context.Context, sessionID string, maxMessages int) ([]llm.Message, error)

	// Append adds messages to the session in order.
	Append(ctx context.Context, sessionID string, msgs []llm.Message) error

	// Trim removes oldest messages so the session has at most maxMessages.
	// Cheap to call after every Append; idempotent.
	Trim(ctx context.Context, sessionID string, maxMessages int) error
}
