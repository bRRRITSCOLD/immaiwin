package mongodb

import (
	"context"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/llm"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/workflow"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// ChatMemory persists agent conversation turns indexed by sessionID.
//
// Storage shape: one document per turn, indexed by (session_id, seq).
// Append increments seq; Trim removes oldest turns. This is simpler than
// a single document with an embedded array — easier to paginate, no 16MB
// document cap to worry about.
type ChatMemory struct {
	col *mongo.Collection
}

// chatTurn is the persisted shape. We store llm.Message verbatim plus
// bookkeeping fields. The Message field handles JSON marshaling fine via
// BSON's json-tag fallthrough.
type chatTurn struct {
	ID        string      `bson:"_id"        json:"id"`
	SessionID string      `bson:"session_id" json:"session_id"`
	Seq       int64       `bson:"seq"        json:"seq"`
	CreatedAt time.Time   `bson:"created_at" json:"created_at"`
	Message   llm.Message `bson:"message"    json:"message"`
}

// NewChatMemory constructs the repo + ensures indexes.
func NewChatMemory(ctx context.Context, db *mongo.Database) (*ChatMemory, error) {
	col := db.Collection("agent_chat_memory")
	_, err := col.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "session_id", Value: 1}, {Key: "seq", Value: 1}}, Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "session_id", Value: 1}, {Key: "created_at", Value: -1}}},
	})
	if err != nil {
		return nil, err
	}
	return &ChatMemory{col: col}, nil
}

// Compile-time check.
var _ workflow.AgentMemory = (*ChatMemory)(nil)

// Load returns the most-recent up-to-maxMessages messages for the session,
// oldest first. Empty session = empty slice + nil error.
func (m *ChatMemory) Load(ctx context.Context, sessionID string, maxMessages int) ([]llm.Message, error) {
	if sessionID == "" {
		return nil, nil
	}
	if maxMessages <= 0 {
		maxMessages = 30
	}

	// Sort by seq DESC, limit, then reverse for chronological output.
	opts := options.Find().
		SetSort(bson.D{{Key: "seq", Value: -1}}).
		SetLimit(int64(maxMessages))

	cur, err := m.col.Find(ctx, bson.M{"session_id": sessionID}, opts)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cur.Close(ctx) }()

	var turns []chatTurn
	if err := cur.All(ctx, &turns); err != nil {
		return nil, err
	}
	// Reverse to chronological order.
	out := make([]llm.Message, len(turns))
	for i, t := range turns {
		out[len(turns)-1-i] = t.Message
	}
	return out, nil
}

// Append adds messages in order, assigning monotonic seq numbers.
func (m *ChatMemory) Append(ctx context.Context, sessionID string, msgs []llm.Message) error {
	if sessionID == "" || len(msgs) == 0 {
		return nil
	}

	// Get current max seq for the session.
	var last chatTurn
	err := m.col.FindOne(ctx,
		bson.M{"session_id": sessionID},
		options.FindOne().SetSort(bson.D{{Key: "seq", Value: -1}}),
	).Decode(&last)
	if err != nil && err != mongo.ErrNoDocuments {
		return err
	}
	nextSeq := last.Seq + 1
	if last.Seq == 0 && err == mongo.ErrNoDocuments {
		nextSeq = 1
	}

	docs := make([]any, len(msgs))
	now := time.Now().UTC()
	for i, msg := range msgs {
		seq := nextSeq + int64(i)
		docs[i] = chatTurn{
			ID:        sessionID + ":" + formatSeq(seq),
			SessionID: sessionID,
			Seq:       seq,
			CreatedAt: now,
			Message:   msg,
		}
	}
	_, err = m.col.InsertMany(ctx, docs)
	return err
}

// Trim removes oldest messages so the session has at most maxMessages.
// Idempotent.
func (m *ChatMemory) Trim(ctx context.Context, sessionID string, maxMessages int) error {
	if sessionID == "" || maxMessages <= 0 {
		return nil
	}

	count, err := m.col.CountDocuments(ctx, bson.M{"session_id": sessionID})
	if err != nil {
		return err
	}
	if count <= int64(maxMessages) {
		return nil
	}

	// Find the cutoff seq — the (count - maxMessages)th oldest.
	excess := count - int64(maxMessages)
	cur, err := m.col.Find(ctx,
		bson.M{"session_id": sessionID},
		options.Find().
			SetSort(bson.D{{Key: "seq", Value: 1}}).
			SetLimit(excess).
			SetProjection(bson.M{"seq": 1}),
	)
	if err != nil {
		return err
	}
	defer func() { _ = cur.Close(ctx) }()

	var oldest []chatTurn
	if err := cur.All(ctx, &oldest); err != nil {
		return err
	}
	if len(oldest) == 0 {
		return nil
	}
	cutoffSeq := oldest[len(oldest)-1].Seq

	_, err = m.col.DeleteMany(ctx, bson.M{
		"session_id": sessionID,
		"seq":        bson.M{"$lte": cutoffSeq},
	})
	return err
}

// formatSeq pads seq to 12 digits so document IDs sort lexicographically.
func formatSeq(n int64) string {
	const pad = "000000000000"
	s := ""
	if n < 0 {
		s = "-"
		n = -n
	}
	num := ""
	if n == 0 {
		num = "0"
	} else {
		for n > 0 {
			num = string(rune('0'+(n%10))) + num
			n /= 10
		}
	}
	if len(num) < 12 {
		num = pad[:12-len(num)] + num
	}
	return s + num
}
