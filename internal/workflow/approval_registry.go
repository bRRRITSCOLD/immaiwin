package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/redis/go-redis/v9"
)

// ApprovalSubscriber is the slim subset of *rediss.Client that the
// approval registry uses for cross-process messaging. Defined here so
// the workflow package doesn't import the rediss package directly
// (avoids a cycle when the test harness wants to fake it).
//
// PublishWithCount returns Redis' delivery count (number of clients
// that received the message). Submit uses this to detect abandoned
// pending_approval runs — the in-process waiter dies on API restart,
// so a publish that lands with zero receivers means the Mongo record
// is stale and the run can never resume. Caller (HTTP handler) takes
// remediation from there (cancel the run, return 410).
type ApprovalSubscriber interface {
	Publish(ctx context.Context, channel string, payload []byte) error
	PublishWithCount(ctx context.Context, channel string, payload []byte) (int64, error)
	Subscribe(ctx context.Context, channels ...string) *redis.PubSub
}

// ApprovalRegistry tracks in-flight workflow runs that are blocked on
// an out-of-band tool-approval decision (i.e. a server-side run with
// no live WS connection). Cross-process: the agent loop runs in the
// `worker` process; the HTTP `POST /runs/:id/approval` endpoint runs
// in the `api` process. Pure in-memory channels can't bridge that, so
// this registry uses Redis pub/sub: Register opens a subscription on
// `burrow:approval:<run_id>`; Submit publishes a JSON-encoded
// ApprovalDecision to the same channel; the registered subscriber
// goroutine parses + forwards to the agent's local channel.
//
// Distinct from the WS-side approveCh which lives on a single run's
// connection — that one's ephemeral and dies with the socket. This
// registry survives restarts in the sense that any process subscribed
// during the wait window will pick up a published decision.
type ApprovalRegistry struct {
	subscriber ApprovalSubscriber

	mu      sync.Mutex
	pending map[string]*pendingEntry
}

type pendingEntry struct {
	ch  chan ApprovalDecision
	sub *redis.PubSub
}

// NewApprovalRegistry constructs a registry backed by the given Redis
// client. Both api + worker processes should construct one each; they
// stay in sync via the shared Redis instance.
func NewApprovalRegistry(sub ApprovalSubscriber) *ApprovalRegistry {
	return &ApprovalRegistry{
		subscriber: sub,
		pending:    make(map[string]*pendingEntry),
	}
}

// approvalChannel is the per-run pub/sub channel name. Centralised so
// publish + subscribe stay in lockstep.
func approvalChannel(runID string) string {
	return "burrow:approval:" + runID
}

// Register opens a subscription on the run's approval channel and
// returns a buffered Go channel the caller (the agent loop) reads from.
// Caller MUST defer Unregister so the subscription closes cleanly.
// Returns nil when the registry has no Redis subscriber wired (legacy
// in-memory fallback for tests).
func (r *ApprovalRegistry) Register(ctx context.Context, runID string) chan ApprovalDecision {
	if r == nil || r.subscriber == nil {
		return nil
	}
	ch := make(chan ApprovalDecision, 4)
	sub := r.subscriber.Subscribe(ctx, approvalChannel(runID))

	r.mu.Lock()
	r.pending[runID] = &pendingEntry{ch: ch, sub: sub}
	r.mu.Unlock()

	// Pump messages from Redis into the local channel until the sub
	// closes (Unregister or context cancel). Goroutine exits cleanly
	// when subCh closes — go-redis closes it on PubSub.Close().
	go func() {
		subCh := sub.Channel()
		for msg := range subCh {
			var d ApprovalDecision
			if err := json.Unmarshal([]byte(msg.Payload), &d); err != nil {
				slog.Warn("approval registry: bad payload", "run_id", runID, "err", err)
				continue
			}
			select {
			case ch <- d:
			default:
				slog.Warn("approval registry: ch full; dropping decision", "run_id", runID)
			}
		}
	}()

	return ch
}

// Unregister closes the run's subscription. Safe to call once even
// when the entry was already cleaned up. Should be deferred by the
// agent loop so abnormal exits don't leak Redis subscriptions.
func (r *ApprovalRegistry) Unregister(runID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	entry := r.pending[runID]
	delete(r.pending, runID)
	r.mu.Unlock()
	if entry != nil && entry.sub != nil {
		_ = entry.sub.Close()
	}
}

// Submit publishes a decision to the run's approval channel. Returns
// the Redis delivery count (number of subscribers that received it)
// alongside any publish error. Caller can use a zero count + a Mongo
// record still in `pending_approval` to detect an abandoned run (the
// API process holding the wait restarted; the in-memory goroutine +
// its Redis subscription died with it). Cancelling those runs is the
// right move — they're unrecoverable.
func (r *ApprovalRegistry) Submit(ctx context.Context, runID string, decision ApprovalDecision) (int64, error) {
	if r == nil || r.subscriber == nil {
		return 0, fmt.Errorf("approval registry: no broker configured")
	}
	payload, err := json.Marshal(decision)
	if err != nil {
		return 0, fmt.Errorf("approval registry: marshal: %w", err)
	}
	return r.subscriber.PublishWithCount(ctx, approvalChannel(runID), payload)
}
