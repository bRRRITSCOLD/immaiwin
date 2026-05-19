package workflow

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/sandbox"
	"github.com/bRRRITSCOLD/burrow/internal/skills"
	"github.com/oklog/ulid/v2"
	"github.com/redis/go-redis/v9"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// defaultMaxResponseBytes caps http_request response body size when the node
// doesn't override it. 10 MiB matches typical document/API responses while
// preventing runaway memory.
const defaultMaxResponseBytes int64 = 10 * 1024 * 1024

// RedisClient executes Redis operations on behalf of redis_request nodes.
// Producer-side only — no Subscribe (the sub side will become a trigger node).
type RedisClient interface {
	Publish(ctx context.Context, channel string, payload []byte) error

	// strings
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string, ttl time.Duration) error
	Del(ctx context.Context, keys ...string) (int64, error)
	Incr(ctx context.Context, key string) (int64, error)
	Decr(ctx context.Context, key string) (int64, error)
	Expire(ctx context.Context, key string, ttl time.Duration) (bool, error)
	TTL(ctx context.Context, key string) (time.Duration, error)
	Exists(ctx context.Context, keys ...string) (int64, error)
	Keys(ctx context.Context, pattern string) ([]string, error)
	MGet(ctx context.Context, keys ...string) ([]any, error)
	MSet(ctx context.Context, pairs map[string]string) error

	// hashes
	HGet(ctx context.Context, key, field string) (string, error)
	HSet(ctx context.Context, key string, fields map[string]string) (int64, error)
	HGetAll(ctx context.Context, key string) (map[string]string, error)
	HDel(ctx context.Context, key string, fields ...string) (int64, error)

	// lists
	LPush(ctx context.Context, key string, values ...any) (int64, error)
	RPush(ctx context.Context, key string, values ...any) (int64, error)
	LPop(ctx context.Context, key string) (string, error)
	RPop(ctx context.Context, key string) (string, error)
	LRange(ctx context.Context, key string, start, stop int64) ([]string, error)
	LLen(ctx context.Context, key string) (int64, error)

	// sets
	SAdd(ctx context.Context, key string, members ...any) (int64, error)
	SRem(ctx context.Context, key string, members ...any) (int64, error)
	SMembers(ctx context.Context, key string) ([]string, error)
	SIsMember(ctx context.Context, key string, member any) (bool, error)

	// sorted sets
	ZAdd(ctx context.Context, key string, members map[string]float64) (int64, error)
	ZRem(ctx context.Context, key string, members ...any) (int64, error)
	ZRange(ctx context.Context, key string, start, stop int64) ([]string, error)
	ZScore(ctx context.Context, key, member string) (float64, error)
	ZIncrBy(ctx context.Context, key string, increment float64, member string) (float64, error)

	// streams (producer + range)
	XAdd(ctx context.Context, stream string, values map[string]any) (string, error)
	XRange(ctx context.Context, stream, start, stop string) ([]redis.XMessage, error)
	XLen(ctx context.Context, stream string) (int64, error)
}

// MongoClient executes generic MongoDB operations against a database.
// Implementations live in internal/mongodb (default db) and inline in
// connection_resolver.go (per-connection resolved clients).
type MongoClient interface {
	Find(ctx context.Context, collection string, filter bson.M, opts *options.FindOptionsBuilder) (*mongo.Cursor, error)
	FindOneAndUpdate(ctx context.Context, collection string, filter, update bson.M, opts *options.FindOneAndUpdateOptionsBuilder) (bson.M, error)
	FindOneAndReplace(ctx context.Context, collection string, filter, replacement bson.M, opts *options.FindOneAndReplaceOptionsBuilder) (bson.M, error)
	InsertOne(ctx context.Context, collection string, doc bson.M) (any, error)
	InsertMany(ctx context.Context, collection string, docs []bson.M, opts *options.InsertManyOptionsBuilder) ([]any, error)
	UpdateMany(ctx context.Context, collection string, filter, update bson.M, opts *options.UpdateManyOptionsBuilder) (matched, modified, upserted int64, err error)
	DeleteOne(ctx context.Context, collection string, filter bson.M) (int64, error)
	DeleteMany(ctx context.Context, collection string, filter bson.M) (int64, error)
	Aggregate(ctx context.Context, collection string, pipeline []bson.M, opts *options.AggregateOptionsBuilder) (*mongo.Cursor, error)
	CountDocuments(ctx context.Context, collection string, filter bson.M, opts *options.CountOptionsBuilder) (int64, error)
	Distinct(ctx context.Context, collection, field string, filter bson.M) ([]any, error)
}

// onErrorPolicy returns the per-node failure policy. Recognised values:
//
//	"stop"     — default; first node error promotes the run to `error`.
//	"continue" — node error is recorded on the StepResult but the run-
//	             status aggregate skips it during promotion. The error
//	             edge (`sourceHandle: "error"`) still routes children
//	             exactly as it does today, so authors can wire diagnostics
//	             / fallback branches without losing the success badge.
//
// Any other value (including missing key) falls back to "stop".
// Empty data → "stop". Lookup is case-insensitive on the value.
func onErrorPolicy(data map[string]any) string {
	if v, ok := data["on_error"].(string); ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "continue":
			return "continue"
		}
	}
	return "stop"
}

// StepResult holds the outcome of a single node execution.
type StepResult struct {
	NodeID   string   `bson:"node_id"            json:"node_id"`
	NodeType NodeType `bson:"node_type"          json:"node_type"`
	Output   any      `bson:"output,omitempty"   json:"output,omitempty"`
	Error    string   `bson:"error,omitempty"    json:"error,omitempty"`
	// ViaAgentTool marks step results produced by an agent-driven
	// `as_tool` dispatch rather than the main BFS. The run-status
	// promotion scan in RunFromCheckpoint ignores Error on these
	// steps: the agent itself decides whether the run aborts
	// (via `stop_on_tool_error` or sandbox-infra classification),
	// so a tool error that the agent fed back to the LLM and
	// recovered from must not promote the run to error
	// retroactively. The canvas + run-detail UI still surface the
	// red badge via the step_done(IsError) event, which is
	// independent of this flag.
	ViaAgentTool bool `bson:"via_agent_tool,omitempty" json:"via_agent_tool,omitempty"`
	// Continued is true when the node errored but its `on_error`
	// policy is `continue`, so the run-status aggregate skips this
	// step's Error during promotion. Run still lands `success` if
	// nothing else failed; the UI surfaces the step distinctly so
	// the suppressed failure is still visible to the operator.
	// Default false → legacy strict behaviour (any non-tool error
	// promotes the run to error).
	Continued bool `bson:"continued,omitempty" json:"continued,omitempty"`
}

// StepContext holds the input, output, and (for for_each) current item of a named step.
//
// For regular nodes:   Input = what the node received; Output = what it produced.
// For for_each nodes:  Input = the full array; Item = the current iteration element.
//                      Output is only populated after all iterations complete (not useful in body).
type StepContext struct {
	Input  any `bson:"input,omitempty"          json:"input"`
	Output any `bson:"output,omitempty"         json:"output"`
	Item   any `bson:"item,omitempty"           json:"item,omitempty"`
}

// runCtx is a per-run map from step name → StepContext.
// JS transforms receive this as the "context" global.
type runCtx map[string]StepContext

// WorkflowExecutor runs a Workflow graph node by node.
type WorkflowExecutor struct {
	HTTPClient   *http.Client
	DB           MongoClient
	Redis        RedisClient
	ConnResolver *ConnectionResolver
	SandboxRT    sandbox.Runtime
	// AI agent dependencies (optional — agent nodes error if unset).
	Memory   AgentMemory      // chat memory backend
	RunRepo  WorkflowRunStore // for run persistence + agent traces
	SkillRes *skills.Resolver // skill bundle resolver (P1.11); nil disables skill loading
	// Default event emitter for runs that don't pass one explicitly.
	// Per-run emitter (passed to RunWithEvents) takes precedence; this
	// fallback is mostly for unit tests + non-streaming code paths.
	Events EventEmitter

	// ApprovalBroker is the cross-process pub/sub backing for OOB
	// approvals. Typically a *rediss.Client; both api + worker wire
	// the same Redis instance so the agent (worker pid) and the
	// approval HTTP endpoint (api pid) bridge via Redis.
	ApprovalBroker ApprovalSubscriber

	// Approvals is the registry, lazy-initialised from ApprovalBroker.
	Approvals     *ApprovalRegistry
	approvalsOnce sync.Once

	// ApprovalNotifier dispatches a magic-link prompt out-of-band when
	// a gate fires. nil = no dispatch (gate still pauses, /runs UI is
	// the only way to resolve it). The dispatcher is best-effort —
	// failures are logged + swallowed so a flaky SMTP relay or
	// unreachable Slack webhook can't strand a workflow.
	ApprovalNotifier ApprovalNotifier
	// ApprovalTokenSecret is the HMAC key for magic-link tokens. Same
	// secret the user-session JWT path uses; reusing avoids a second
	// key in deployments. nil disables magic-link dispatch even when
	// the notifier is configured (the redeem endpoint would 503 anyway
	// without a verifier-side secret).
	ApprovalTokenSecret []byte
	// ApprovalUIBaseURL is the externally-reachable base URL the
	// dispatcher embeds in the magic-link. Empty falls back to a
	// relative `/approve?...` link, useful in dev where the UI proxies
	// the API.
	ApprovalUIBaseURL string

	// cursor cache for find/aggregate pagination. Lazy-initialised on first mongo_request use.
	cursors     *cursorCache
	cursorsOnce sync.Once
}

// dispatchApprovalNotification fires the configured channel notifier
// for a freshly-stamped pending_approval gate. Best-effort: each call
// runs in its own goroutine with a 5s timeout so a slow channel can't
// stall the gate's caller. Failures land in slog.Warn — the /runs UI
// fallback is always available regardless of dispatch outcome.
//
// Skipped when the notifier OR the token secret is unwired: a
// half-configured deploy is treated identically to "feature off."
func (e *WorkflowExecutor) dispatchApprovalNotification(wf Workflow, runID string, pending PendingApprovalState) {
	if e.ApprovalNotifier == nil || len(e.ApprovalTokenSecret) == 0 {
		return
	}
	if pending.TokenID == "" {
		// Defensive — a freshly-fired gate stamps TokenID. Nil means
		// some caller mutated the state mid-flight; skip rather than
		// emit an unverifiable link.
		slog.Warn("approval dispatch: skipped (token_id empty)", "run_id", runID)
		return
	}
	tok, err := SignApprovalToken(e.ApprovalTokenSecret, runID, pending.TokenID, 0)
	if err != nil {
		slog.Warn("approval dispatch: sign token failed", "run_id", runID, "err", err)
		return
	}
	req := ApprovalRequest{
		RunID:     runID,
		Token:     tok,
		Workflow:  wf,
		Pending:   pending,
		UIBaseURL: e.ApprovalUIBaseURL,
	}
	go func(req ApprovalRequest) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := e.ApprovalNotifier.NotifyApprovalRequested(ctx, req); err != nil {
			slog.Warn("approval dispatch: notifier failed (gate still resolvable via /runs)",
				"run_id", req.RunID, "workflow_id", req.Workflow.ID,
				"channel_type", channelType(req.Workflow.ApprovalChannel),
				"err", err)
			// Persist the failure on PendingApproval so /runs/:id can
			// render "<channel> delivery failed: <err>" — the user
			// otherwise has no signal beyond an absent email/Slack.
			// Use a fresh ctx (the dispatch ctx may have just timed
			// out) bounded short so a Mongo blip doesn't strand this
			// goroutine.
			if e.RunRepo != nil {
				upCtx, upCancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer upCancel()
				if rec, gerr := e.RunRepo.Get(upCtx, req.RunID); gerr == nil && rec.PendingApproval != nil {
					rec.PendingApproval.DispatchError = err.Error()
					if uerr := e.RunRepo.Update(upCtx, rec); uerr != nil {
						slog.Warn("approval dispatch: persist dispatch_error failed",
							"run_id", req.RunID, "err", uerr)
					}
				}
			}
		}
	}(req)
}

// channelType is a nil-safe accessor for logging.
func channelType(ch *ApprovalChannel) string {
	if ch == nil {
		return ""
	}
	return ch.Type
}

// approvalRegistryFor returns the executor's lazy-initialised approval
// registry. Centralised so the agent loop and the HTTP handler share
// the same state without each having to nil-check.
func (e *WorkflowExecutor) approvalRegistryFor() *ApprovalRegistry {
	e.approvalsOnce.Do(func() {
		if e.Approvals == nil && e.ApprovalBroker != nil {
			e.Approvals = NewApprovalRegistry(e.ApprovalBroker)
		}
	})
	return e.Approvals
}

// requireNodeApproval reports whether the BFS should halt before
// executing this node and demand a user verdict. Reads
// `require_node_approval` so the flag is unambiguous: it's the pre-
// exec gate for ANY node type. ai_agent has a separate per-tool gate
// (`require_approval`) that fires inside the ReAct loop — both can be
// set independently for layered approval (gate before entering the
// agent + gate every individual tool call).
func (e *WorkflowExecutor) requireNodeApproval(node Node) bool {
	if v, ok := node.Data["require_node_approval"].(bool); ok && v {
		return true
	}
	return false
}

// preExecApproval enforces the pre-exec node-approval gate. Returns
// (approved, err). Two paths:
//   - Live UI (continueCh wired): breakpoint-style pause; Continue =
//     approve, no reject path here (user clicks Cancel to abort).
//   - OOB (no continueCh): Redis-backed approval channel + persisted
//     pending_approval state; Approve/Reject from /runs/:id.
//
// Returns (true, nil) when approved (or auto-approved because no
// channel is wired). Returns (false, nil) for explicit rejection.
// Returns an error only when the gate setup itself failed or ctx
// cancelled — caller should bubble up.
//
// Reused by the BFS pre-exec block AND the agent's tool dispatch
// (where as_tool target nodes bypass main BFS).
func (e *WorkflowExecutor) preExecApproval(ctx context.Context, env *runEnv, node Node, input any) (bool, error) {
	if !e.requireNodeApproval(node) {
		return true, nil
	}
	// Pre-approved (resumed lease run): the resume path consumed an
	// Approved decision from execution_state.pending and stamped this
	// node ID. Skip the gate so the run proceeds. DON'T delete the
	// flag here: in nested-gate scenarios (agent's per-tool gate +
	// this node-level gate on the same call) a subsequent yield
	// might re-enter this code on the next resume; deleting now
	// would cause the second resume to re-fire the gate. Iter-end
	// cleanup in the agent loop clears env.approvedNodeIDs after
	// a successful full tool dispatch.
	if env.approvedNodeIDs != nil && env.approvedNodeIDs[node.ID] {
		return true, nil
	}
	if env.continueCh != nil {
		// Live UI path — breakpoint-style pause. Persist the
		// UI-facing pending mirror + flip status to pending_approval
		// so OOB observers (a manager opening /runs/<id>, the
		// SMTP/Slack approver clicking the magic link) see the run is
		// waiting — the canvas user already sees it via the
		// step_pending WS event, but anyone outside the WS would have
		// no clue otherwise. Cleared after waitAtBreakpoint returns.
		if env.runID != "" && e.RunRepo != nil {
			_ = e.persistPendingApprovalMirror(ctx, env, node, input)
		}
		// OOB tee: register a per-run Redis subscriber so a Slack /
		// email approval click reaches the breakpoint waiter. Without
		// it the redeem handler publishes onto a channel no one in
		// this process is listening on (count=0 → cancelOrphanedApproval
		// writes the misleading "approval orphaned: api process
		// restarted while paused" error even though the WS process is
		// alive). Approved → release continueCh; rejected → release
		// continueCh AND record the rejection so the BFS short-circuits
		// the node. Defer Unregister so the subscription closes when
		// preExecApproval returns.
		var oobRejected bool
		var oobRejectedReason string
		if env.runID != "" {
			if oobReg := e.approvalRegistryFor(); oobReg != nil {
				redisCh := oobReg.Register(ctx, env.runID)
				if redisCh != nil {
					defer oobReg.Unregister(env.runID)
					stopTee := make(chan struct{})
					defer close(stopTee)
					go func(target chan struct{}, src <-chan ApprovalDecision, runID string, stop <-chan struct{}) {
						for {
							select {
							case <-stop:
								return
							case d, ok := <-src:
								if !ok {
									return
								}
								if !d.Approved {
									oobRejected = true
									oobRejectedReason = d.Reason
								}
								select {
								case target <- struct{}{}:
								case <-stop:
									return
								}
							}
						}
					}(env.continueCh, redisCh, env.runID, stopTee)
				}
			}
		}
		env.waitAtBreakpoint(ctx, node)
		if oobRejected {
			if env.runID != "" && e.RunRepo != nil {
				if rec, gerr := e.RunRepo.Get(ctx, env.runID); gerr == nil {
					rec.Status = RunStatusRunning
					rec.PendingApproval = nil
					_ = e.RunRepo.Update(ctx, rec)
				}
			}
			reason := oobRejectedReason
			if reason == "" {
				reason = "rejected by user"
			}
			return false, fmt.Errorf("node approval rejected (OOB): %s", reason)
		}
		if env.runID != "" && e.RunRepo != nil {
			if rec, gerr := e.RunRepo.Get(ctx, env.runID); gerr == nil {
				rec.Status = RunStatusRunning
				rec.PendingApproval = nil
				if uerr := e.RunRepo.Update(ctx, rec); uerr != nil {
					slog.Warn("workflow: clear pending_approval (WS continue) failed", "run_id", env.runID, "err", uerr)
				}
			}
		}
		return true, nil
	}
	// Lease-yield path: persist the gate mirror + dispatch OOB +
	// return the yield sentinel. The BFS catches the sentinel,
	// persists ExecutionState.Pending, releases the lease, and
	// unwinds cleanly so the worker can pick up another run.
	if env.yieldOnApproval && env.runID != "" && e.RunRepo != nil {
		if err := e.persistPendingApprovalMirror(ctx, env, node, input); err != nil {
			return false, err
		}
		env.pendingNodeID = node.ID
		env.pendingInput = input
		return false, errYieldForApproval
	}
	if env.runID != "" && e.RunRepo != nil {
		decision, err := e.waitNodeApproval(ctx, env, node, input)
		if err != nil {
			return false, err
		}
		return decision.Approved, nil
	}
	// No live UI + no broker → degrade to auto-approve so the run
	// isn't deadlocked. Cron / event runs hit this on a misconfigured
	// deploy; the warn surfaces it without breaking flow.
	slog.Warn("workflow: node approval gate skipped (no continueCh or RunRepo)",
		"node", node.ID, "type", node.Type)
	return true, nil
}

// persistPendingApprovalMirror writes the UI-facing PendingApprovalState
// + emits the canvas pending event + dispatches the OOB notification
// for a gated node. Used by the lease-yield branch of preExecApproval
// to set up everything waitNodeApproval would do EXCEPT the in-process
// channel block — the worker yields instead. Idempotent on repeated
// calls (the next run-pass on resume has already cleared Pending).
func (e *WorkflowExecutor) persistPendingApprovalMirror(ctx context.Context, env *runEnv, node Node, input any) error {
	name, _ := node.Data["name"].(string)
	if name == "" {
		name = string(node.Type) + "_" + node.ID
	}
	pending := PendingApprovalState{
		Kind:        "node",
		NodeID:      node.ID,
		NodeType:    node.Type,
		NodeName:    name,
		NodeInput:   input,
		RequestedAt: time.Now().UTC(),
		TokenID:     ulid.Make().String(),
	}
	if rec, gerr := e.RunRepo.Get(ctx, env.runID); gerr == nil {
		rec.Status = RunStatusPendingApproval
		rec.PendingApproval = &pending
		if uerr := e.RunRepo.Update(ctx, rec); uerr != nil {
			slog.Warn("workflow: persist pending_approval mirror (yield) failed", "run_id", env.runID, "node", node.ID, "err", uerr)
		}
	}
	if env.events != nil {
		env.events.Emit(stampNow(RunEvent{
			Type:     EventNodeApprovalPending,
			NodeID:   node.ID,
			NodeType: node.Type,
			RunID:    env.runID,
		}))
	}
	if env.wf != nil {
		e.dispatchApprovalNotification(*env.wf, env.runID, pending)
	}
	return nil
}

// waitNodeApproval blocks until the OOB approval registry receives a
// decision for this run, then returns it. Persists `pending_approval`
// state on the run record so /runs/:id can render the gate.
// Returns the user's decision or an error when the gate can't be set
// up (e.g. no broker, no RunRepo). Context cancellation surfaces as a
// non-nil error AND a zero-value decision.
func (e *WorkflowExecutor) waitNodeApproval(ctx context.Context, env *runEnv, node Node, input any) (ApprovalDecision, error) {
	reg := e.approvalRegistryFor()
	if reg == nil {
		return ApprovalDecision{}, fmt.Errorf("workflow: pre-node approval requested but ApprovalBroker not configured")
	}
	ch := reg.Register(ctx, env.runID)
	defer reg.Unregister(env.runID)

	name, _ := node.Data["name"].(string)
	if name == "" {
		name = string(node.Type) + "_" + node.ID
	}

	// Persist pending state so the run-detail UI surfaces the gate.
	// TokenID is a per-gate nonce that the magic-link redeem endpoint
	// matches against the inbound JWT's `jti` — once cleared (decision
	// landed, run cancelled, ctx died) a stale link can't resurrect
	// the gate.
	pending := PendingApprovalState{
		Kind:        "node",
		NodeID:      node.ID,
		NodeType:    node.Type,
		NodeName:    name,
		NodeInput:   input,
		RequestedAt: time.Now().UTC(),
		TokenID:     ulid.Make().String(),
	}
	if rec, gerr := e.RunRepo.Get(ctx, env.runID); gerr == nil {
		rec.Status = RunStatusPendingApproval
		rec.PendingApproval = &pending
		if uerr := e.RunRepo.Update(ctx, rec); uerr != nil {
			slog.Warn("workflow: persist pending_approval (node) failed", "run_id", env.runID, "node", node.ID, "err", uerr)
		}
	}
	// Stream the gate to live UI clients so the canvas / runs page
	// flips the node from "running" to "awaiting approval" without
	// having to poll. Independent of OOB dispatch outcome.
	if env.events != nil {
		env.events.Emit(stampNow(RunEvent{
			Type:     EventNodeApprovalPending,
			NodeID:   node.ID,
			NodeType: node.Type,
			RunID:    env.runID,
		}))
	}
	// Dispatch OOB channel notification (Stage 2). Async, best-effort;
	// the gate still resolves through the /runs UI if the notifier
	// fails or is unwired. The workflow value already carries the
	// configured `ApprovalChannel` so the dispatcher can read it
	// without a second Mongo lookup.
	if env.wf != nil {
		e.dispatchApprovalNotification(*env.wf, env.runID, pending)
	}

	// Block until decision or ctx cancel.
	select {
	case <-ctx.Done():
		return ApprovalDecision{}, fmt.Errorf("workflow: node approval wait cancelled (node %s) — node not executed", node.ID)
	case decision := <-ch:
		// Clear pending state regardless of approve/reject so the UI
		// stops showing the buttons + the run flips back to running.
		if rec, gerr := e.RunRepo.Get(ctx, env.runID); gerr == nil {
			rec.Status = RunStatusRunning
			rec.PendingApproval = nil
			_ = e.RunRepo.Update(ctx, rec)
		}
		return decision, nil
	}
}

// cursorCache stores open *mongo.Cursor instances for cursor_fetch ops.
// Cursors expire after defaultCursorTTL idle and are closed by the janitor.
type cursorCache struct {
	mu      sync.Mutex
	entries map[string]*storedCursor
}

type storedCursor struct {
	cur      *mongo.Cursor
	lastUsed time.Time
}

const defaultCursorTTL = 10 * time.Minute

func (e *WorkflowExecutor) cursorCache() *cursorCache {
	e.cursorsOnce.Do(func() {
		c := &cursorCache{entries: make(map[string]*storedCursor)}
		e.cursors = c
		go c.janitor()
	})
	return e.cursors
}

func (c *cursorCache) put(cur *mongo.Cursor) string {
	id := newCursorID()
	c.mu.Lock()
	c.entries[id] = &storedCursor{cur: cur, lastUsed: time.Now()}
	c.mu.Unlock()
	return id
}

func (c *cursorCache) take(id string) *mongo.Cursor {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[id]
	if !ok {
		return nil
	}
	entry.lastUsed = time.Now()
	return entry.cur
}

func (c *cursorCache) drop(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.entries[id]; ok {
		_ = entry.cur.Close(context.Background())
		delete(c.entries, id)
	}
}

func (c *cursorCache) janitor() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		now := time.Now()
		c.mu.Lock()
		for id, entry := range c.entries {
			if now.Sub(entry.lastUsed) > defaultCursorTTL {
				_ = entry.cur.Close(context.Background())
				delete(c.entries, id)
			}
		}
		c.mu.Unlock()
	}
}

func newCursorID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// runEnv carries per-run graph context that the agent loop needs but
// other node handlers don't. Stored in ctx via runEnvKey rather than
// expanding every handler's signature.
type runEnv struct {
	wf       *Workflow
	byID     map[string]Node
	adj      map[string][]adjEntry
	runID    string // WorkflowRun.ID for trace persistence; empty if no RunRepo
	tenantID string // "default" until multi-tenant

	// toolSteps collects StepResults for nodes invoked as agent tools
	// (bypassing the main BFS). Drained into the run-level results when
	// the executor's BFS loop finishes so the UI shows success/error
	// status on the tool-bound nodes.
	toolSteps *toolStepCollector

	// skillSystemFragments is a per-agent-run scratch slot for the
	// `prompts/system.md` fragment from each loaded skill. Populated by
	// buildAgentToolCatalog; read by runAIAgent when constructing the
	// final system prompt. Lives on runEnv (not the catalog) so we don't
	// have to thread an extra return through buildAgentToolCatalog.
	skillSystemFragments []string

	// events is the per-run event emitter — fan-out to WS clients,
	// run-repo, etc. Always non-nil after Run() configures it (defaults
	// to the executor's Events field, then to a no-op).
	events EventEmitter

	// stopAtSet is the set of node IDs that are breakpoints for this
	// run. Empty/nil = no breakpoints. Tool-invoked node handlers (which
	// bypass main BFS) read this so a breakpoint on an as_tool target
	// stops the run after the tool finishes — without it, the BFS-side
	// breakpoint check never fires for tool-invoked nodes. Use
	// `env.isBreakpoint(nodeID)` for membership. Guarded by stopAtMu so
	// the live UI can update the set mid-run via a `set_breakpoints` WS
	// frame (deselecting a node should let the run skip past it; adding
	// a node should let the run halt there on its next visit).
	stopAtSet map[string]bool
	stopAtMu  sync.RWMutex
	// stopAtHit is set by a tool handler when it executes a breakpoint
	// node. The BFS loop checks it after every iteration and short-
	// circuits the run. Goroutine-safe via the toolSteps mutex (we reuse
	// it because the handler already takes that lock).
	stopAtHit bool

	// resumeAgentState carries the persisted snapshot from a previous
	// paused run when the current run was started with `resume_run_id`.
	// The agent loop checks `state.AgentNodeID` against the current node
	// and rehydrates messages/iter/usage/trace, skipping fresh prompt
	// construction. nil for a normal run.
	resumeAgentState *AgentPauseState
	// resumeRunID is the WorkflowRun ID we're resuming into so the pause-
	// write targets the same record instead of creating a sibling. Empty
	// for fresh runs.
	resumeRunID string
	// completedNodeIDs is the set of node IDs already executed in the
	// previous (paused) run; the BFS pre-marks these as visited so the
	// resume run jumps straight to the paused agent.
	completedNodeIDs map[string]bool

	// continueCh wakes a paused pre-exec breakpoint when the live UI
	// sends `{type:"continue"}` over the run WS. Nil disables pre-exec
	// pause (falls back to the post-exec stopAt path).
	continueCh chan struct{}

	// approveCh receives per-tool-call user decisions for the
	// `require_approval` gate. Each frame from the live UI
	// (`{type:"approve_tool", tool_call_id, approved, reason}`) lands here.
	// Nil disables the gate (require_approval node config becomes a no-op).
	approveCh chan ApprovalDecision

	// toolNameToNodeID maps the LLM-facing tool name → target node ID for
	// edge-bound `as_tool` targets. Built during buildAgentToolCatalog.
	// Used on approval rejection so the canvas can flip the corresponding
	// node to a red "rejected" badge instead of leaving it as
	// "not executed". Skill tools / built-ins aren't in this map (no
	// canvas node behind them) — those just stay invisible on rejection.
	toolNameToNodeID map[string]string
	// toolNameToOnError maps tool name → the target node's on_error
	// policy (`stop` default, `continue` opt-in). The agent's tool-
	// dispatch loop reads this when a tool errors: `stop` aborts the
	// agent (run flips to error), `continue` feeds the error back to
	// the LLM as a tool_result so the model can pivot — the legacy
	// "agent recovers from flaky upstream" behavior, now opt-in.
	// Built-in tools without an as_tool node default to `stop`.
	toolNameToOnError map[string]string

	// workerID is the lease-holding worker's identifier for runs
	// executing under the Phase 3 lease-based path. Empty for legacy
	// (in-process) runs. When non-empty, BFS checkpoints carry it so
	// CheckpointExecutionState can reject the write if a different
	// worker has reclaimed the lease.
	workerID string
	// checkpoint controls whether BFS persists ExecutionState after
	// every node visit. True only for lease-held runs (worker loop);
	// false for legacy in-process Run / RunResumable so the existing
	// non-durable path stays free of Mongo round-trips.
	checkpoint bool
	// yieldOnApproval routes the approval gate through the persist-
	// then-return sentinel path instead of blocking on the in-process
	// approval channel. True only for lease-held runs. When true, a
	// gate fire persists PendingExecutionGate, releases the lease,
	// and surfaces errYieldForApproval up the call chain. When false
	// (legacy), the gate blocks on an in-process channel exactly as
	// before.
	yieldOnApproval bool
	// priorExecState carries the BFS snapshot loaded from the run
	// record at resume time. RunWithEvents pre-populates the local
	// queue + visited map from this when non-nil so the resumed run
	// dispatches the next node, not the trigger.
	priorExecState *ExecutionState
	// approvedNodeIDs tracks node IDs whose approval gate has already
	// been resolved (Approved=true) on a previous run-pass. Set from
	// priorExecState.Pending when the resuming worker applies the
	// pre-recorded decision; consulted by preExecApproval to skip the
	// gate so the run proceeds without firing the gate a second time.
	approvedNodeIDs map[string]bool
	// pendingNodeID + pendingInput identify the gated node that the
	// BFS must yield on (set by the gate handler before returning the
	// sentinel). RunWithEvents reads these to write the persisted
	// ExecutionState.Pending before unwinding the loop.
	pendingNodeID string
	pendingInput  any
	// pendingKind is "node" (default — pre-exec node-approval gate)
	// or "tool_call" (agent's per-tool approval gate). yieldForApproval
	// reads this to populate PendingExecutionGate.Kind so the resume
	// path knows whether to apply the decision to a node or to the
	// agent's next tool_call by name.
	pendingKind string
	// pendingToolName + pendingToolCallID are populated only when
	// pendingKind="tool_call". ToolName is what resume matches against
	// (model regenerates ToolCallIDs on the re-prompt). ToolCallID is
	// captured for audit.
	pendingToolName   string
	pendingToolCallID string
	// approvedToolCallNames carries pre-recorded per-tool approval
	// decisions hydrated from priorState.Pending on resume. The
	// agent's per-tool gate consults this map by tool name; a hit
	// applies the saved decision and removes the entry (one-shot
	// consumption so a re-fire doesn't reuse an old approval).
	approvedToolCallNames map[string]*ApprovalDecision
}

// errYieldForApproval is the sentinel an executor under a lease
// returns when an approval gate fires: instead of blocking the
// worker on a Redis-backed channel (the legacy path), the gate
// persists PendingExecutionGate, releases the lease, and unwinds
// the BFS loop so the worker can pick up another run. The next
// claim of this run will see ExecutionState.Pending != nil and
// (once the user clicks Approve/Reject) Pending.Decision != nil,
// at which point the worker applies the decision and continues.
//
// Not exported: only the executor package raises and matches it.
var errYieldForApproval = errors.New("workflow: yield for approval gate")

// ApprovalDecision is the user's per-tool-call verdict for the
// require_approval gate. Routed from the live WS handler into
// runEnv.approveCh; the agent loop matches it by ToolCallID before
// firing or short-circuiting the tool dispatch.
type ApprovalDecision struct {
	ToolCallID string `bson:"tool_call_id,omitempty" json:"tool_call_id"`
	Approved   bool   `bson:"approved"               json:"approved"`
	Reason     string `bson:"reason,omitempty"       json:"reason,omitempty"`
}

// isBreakpoint reports whether the given node ID is one of this run's
// configured breakpoints. Centralised so callers don't have to nil-
// check the map themselves on every BFS step. Read-locks stopAtMu so
// concurrent `SetBreakpoints` mutations from the WS reader goroutine
// are race-free.
func (env *runEnv) isBreakpoint(nodeID string) bool {
	env.stopAtMu.RLock()
	defer env.stopAtMu.RUnlock()
	if len(env.stopAtSet) == 0 {
		return false
	}
	return env.stopAtSet[nodeID]
}

// SetBreakpoints replaces the run's breakpoint set with the given IDs.
// Safe to call from any goroutine (typically the WS handler's reader).
// Empty list clears all breakpoints.
func (env *runEnv) SetBreakpoints(ids []string) {
	next := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id != "" {
			next[id] = true
		}
	}
	env.stopAtMu.Lock()
	env.stopAtSet = next
	env.stopAtMu.Unlock()
}

// waitAtBreakpoint blocks until the live UI sends a continue frame OR
// the run context cancels. Emits a `step_pending` event so the UI can
// flip the node state to a "pending — click Continue to fire" indicator.
// No-op when the env has no continueCh — the breakpoint then falls
// through to the post-exec path that persists `PausedAgent`.
func (env *runEnv) waitAtBreakpoint(ctx context.Context, node Node) {
	if env.continueCh == nil || env.events == nil {
		return
	}
	env.events.Emit(stampNow(RunEvent{
		Type:     EventStepPending,
		NodeID:   node.ID,
		NodeType: node.Type,
	}))
	select {
	case <-env.continueCh:
		// Drain extra tokens so a fast double-click doesn't release
		// the next breakpoint as well.
		for {
			select {
			case <-env.continueCh:
			default:
				return
			}
		}
	case <-ctx.Done():
	}
}

// toolStepCollector accumulates StepResults from tool-invoked node runs.
// Goroutine-safe; agent loops fan tool calls out concurrently.
type toolStepCollector struct {
	mu    sync.Mutex
	steps []StepResult
}

func (c *toolStepCollector) add(sr StepResult) {
	c.mu.Lock()
	c.steps = append(c.steps, sr)
	c.mu.Unlock()
}

func (c *toolStepCollector) drain() []StepResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.steps
	c.steps = nil
	return out
}

type runEnvKeyT struct{}

var runEnvKey runEnvKeyT

func envFromCtx(ctx context.Context) (*runEnv, bool) {
	v, ok := ctx.Value(runEnvKey).(*runEnv)
	return v, ok
}

type forEachIterKeyT struct{}

var forEachIterKey forEachIterKeyT

type forEachLoop struct{ iter, total int }

// withForEachLoop stamps the 1-based for_each iteration index AND the
// total iteration count onto ctx. Body steps + an agent run in the
// loop body read it (forEachIterFromCtx / forEachLoopFromCtx) to tag
// every step/trace event with LoopIter+LoopTotal, so the UI can group
// agent traces by iteration and show a uniform "iter K/M" across the
// whole body subgraph.
func withForEachLoop(ctx context.Context, iter, total int) context.Context {
	return context.WithValue(ctx, forEachIterKey, forEachLoop{iter: iter, total: total})
}

func forEachLoopFromCtx(ctx context.Context) (iter, total int) {
	if v, ok := ctx.Value(forEachIterKey).(forEachLoop); ok {
		return v.iter, v.total
	}
	return 0, 0
}

func forEachIterFromCtx(ctx context.Context) int {
	i, _ := forEachLoopFromCtx(ctx)
	return i
}

// adjEntry is one outgoing edge from a node.
type adjEntry struct {
	targetID     string
	sourceHandle string
}

// Run executes a workflow starting from all trigger nodes using BFS.
// for_each nodes iterate input arrays; their "item" edge targets are run
// once per element (not by the main BFS).
// If stopAt is non-empty, execution halts after the node with that ID executes,
// returning partial results (useful for debug/breakpoint runs). Multiple
// breakpoints are supported via RunResumable + RunOpts.StopAtIDs.
// Optional initialInput is injected as trigger node output (used by event-driven triggers like RabbitMQ).
//
// Events are emitted to e.Events (or a no-op when nil). Use RunWithEvents
// when callers need to attach a per-invocation emitter (WS handler, tests).
func (e *WorkflowExecutor) Run(ctx context.Context, wf Workflow, stopAt string, initialInput ...any) ([]StepResult, error) {
	var ids []string
	if stopAt != "" {
		ids = []string{stopAt}
	}
	return e.RunWithEvents(ctx, wf, ids, e.Events, initialInput...)
}

// RunOpts collects per-invocation knobs for RunResumable. Reserved for the
// resumable + emitter combo so callers don't trigger Run signature growth.
type RunOpts struct {
	// StopAt is the legacy single-breakpoint field. Prefer StopAtIDs;
	// kept for callers that haven't migrated. When both are set the
	// union is used.
	StopAt string
	// StopAtIDs is the multi-breakpoint set. Run halts at the first
	// node whose ID matches AND, on continue, halts again at the next
	// match. Empty = no breakpoints.
	StopAtIDs   []string
	Input       any
	ResumeRunID string // when set, resumes a paused WorkflowRun
	// PreallocRunID seeds the WorkflowRun record's ID instead of
	// letting RunResumable mint a fresh ULID. Used by async dispatch
	// callers (webhook handler) that need to return the run ID to the
	// client BEFORE the run finishes. Ignored when ResumeRunID is set.
	PreallocRunID string
	Emitter     EventEmitter
	// ContinueCh receives a tick when the live UI client sends a
	// `{type:"continue"}` frame over the run WS. Used by the pre-exec
	// breakpoint path (`runEnv.waitAtBreakpoint`) to unblock a paused
	// node *in-process* — distinct from the persisted-resume flow that
	// re-launches the run from a saved snapshot. Nil disables pre-exec
	// pause: the breakpoint then falls back to post-exec semantics.
	ContinueCh chan struct{}
	// ApproveCh routes per-tool-call user verdicts from the live UI
	// (`{type:"approve_tool"}`) into the agent's require_approval gate.
	// Nil disables the gate.
	ApproveCh chan ApprovalDecision
	// BreakpointsCh receives live updates to the run's breakpoint set
	// from the UI (`{type:"set_breakpoints", node_ids: [...]}`). The run
	// applies each update via runEnv.SetBreakpoints; deselecting a node
	// then lets the run skip past it. Nil disables live updates — the
	// breakpoint set stays whatever was passed at run start.
	BreakpointsCh chan []string
}

// RunOutcome reports the final state of a Resumable run plus the WorkflowRun
// ID so the caller (HTTP handler) can echo it back to the UI for the
// "Continue" button to target on the next click.
type RunOutcome struct {
	Steps  []StepResult `json:"steps"`
	RunID  string       `json:"run_id,omitempty"`
	Status RunStatus    `json:"status"`
}

// resumeKeyT is the ctx key we tunnel resume state through. Internal —
// callers go through RunResumable, not the ctx.
type resumeKeyT struct{}

var resumeKey = resumeKeyT{}

// checkpointKeyT is the ctx key for the lease-aware checkpoint bundle.
// Set by RunFromCheckpoint (worker-loop entry path) so RunWithEvents
// hydrates BFS state from the persisted ExecutionState + emits a
// checkpoint after every node visit + routes approval gates through
// the yield-and-release path. Distinct from resumeKey: that one
// carries the legacy paused-agent snapshot used by the in-process
// resume flow; this one carries the new durable-run state.
type checkpointKeyT struct{}

var checkpointKey = checkpointKeyT{}

// checkpointBundle is the per-run lease + checkpoint context the
// worker loop hands the executor when invoking RunFromCheckpoint.
// RunWithEvents reads this off ctx and (when present) wires the
// matching fields onto runEnv so the BFS loop knows it's executing
// under a lease.
type checkpointBundle struct {
	runID       string
	workerID    string
	priorState  *ExecutionState
	pausedAgent *AgentPauseState
	// Reserved for the Phase-2 control-channel bridge (canvas
	// Continue + set_breakpoints). Currently no field on the
	// bundle — when the bridge lands it'll add continueCh and
	// breakpointsCh chans here, populated by the worker before
	// invoking RunFromCheckpoint.
}

type resumeBundle struct {
	runID         string
	agent         *AgentPauseState
	completedIDs  map[string]bool
	continueCh    chan struct{}          // for pre-exec breakpoint pause; nil disables
	approveCh     chan ApprovalDecision // for require_approval gate; nil disables
	breakpointsCh chan []string          // for live breakpoint updates; nil disables
}

// RunResumable executes a workflow with optional mid-run resume. When
// `opts.ResumeRunID` is set, the executor loads the persisted
// `WorkflowRun.PausedAgent` snapshot, pre-marks every node already
// executed in that run as visited (so they don't re-run), and the
// matching agent's loop hydrates from the saved messages/iter/usage so
// the LLM picks up exactly where it left off.
//
// On a fresh run (no ResumeRunID), behaves like Run + RunWithEvents but
// also returns the freshly-minted run ID and final status — handy for
// the UI to track "is this run paused or done?" without a follow-up GET.
func (e *WorkflowExecutor) RunResumable(ctx context.Context, wf Workflow, opts RunOpts) (RunOutcome, error) {
	bundle := &resumeBundle{continueCh: opts.ContinueCh, approveCh: opts.ApproveCh, breakpointsCh: opts.BreakpointsCh}

	// Resume path: hydrate from existing run record.
	if opts.ResumeRunID != "" {
		if e.RunRepo == nil {
			return RunOutcome{}, fmt.Errorf("workflow: resume requested but RunRepo not configured")
		}
		prev, err := e.RunRepo.Get(ctx, opts.ResumeRunID)
		if err != nil {
			return RunOutcome{}, fmt.Errorf("workflow: load resume run %q: %w", opts.ResumeRunID, err)
		}
		if prev.Status != RunStatusPaused || prev.PausedAgent == nil {
			return RunOutcome{}, fmt.Errorf("workflow: run %q is not paused (status=%s)", opts.ResumeRunID, prev.Status)
		}
		bundle.runID = prev.ID
		bundle.agent = prev.PausedAgent
		bundle.completedIDs = make(map[string]bool, len(prev.Steps))
		for _, s := range prev.Steps {
			// Skip the agent itself — we want it to re-enter the loop with
			// hydrated state, not get pre-marked visited.
			if s.NodeID == prev.PausedAgent.AgentNodeID {
				continue
			}
			bundle.completedIDs[s.NodeID] = true
		}
		// Pre-flip status back to running so successive observers see the
		// transition. Updated again at the end (success/paused/error).
		prev.Status = RunStatusRunning
		prev.PausedAgent = nil
		_ = e.RunRepo.Update(ctx, prev)
	}

	// Pre-run daily cost cap check. Skipped on resume (already running
	// once; check happens mid-run via the agent loop). Skipped when no
	// cap configured or no run repo. On breach we ALSO persist a tiny
	// run record (status=error, error="cost_exceeded:...") so the /runs
	// table reflects the rejected attempt — without it the user sees
	// silence on the listing and thinks the cap didn't fire.
	if opts.ResumeRunID == "" && wf.CostLimits != nil && wf.CostLimits.MaxDailyUSD > 0 && e.RunRepo != nil {
		spent, sumErr := e.RunRepo.SumCostSince(ctx, wf.ID, startOfUTCDay(time.Now()))
		if sumErr == nil && spent >= wf.CostLimits.MaxDailyUSD {
			ce := &CostExceededError{Axis: "daily", CapUSD: wf.CostLimits.MaxDailyUSD, SpentUSD: spent}
			now := time.Now().UTC()
			rejectedID := ulid.Make().String()
			rec := WorkflowRun{
				ID:           rejectedID,
				WorkflowID:   wf.ID,
				TenantID:     wf.TenantID,
				QueuedAt:     now,
				StartedAt:    &now,
				FinishedAt:   &now,
				Status:       RunStatusError,
				Params:       wf.Params,
				TriggerInput: opts.Input,
				Error:        ce.Error(),
			}
			if _, cerr := e.RunRepo.Create(ctx, rec); cerr == nil {
				if emitter := opts.Emitter; emitter != nil {
					emitter.Emit(stampNow(RunEvent{Type: EventCostExceeded, Error: ce.Error(), RunID: rejectedID}))
					emitter.Emit(stampNow(RunEvent{Type: EventRunDone, Status: string(RunStatusError), RunID: rejectedID}))
				}
				return RunOutcome{Status: RunStatusError, RunID: rejectedID}, ce
			}
			if emitter := opts.Emitter; emitter != nil {
				emitter.Emit(stampNow(RunEvent{Type: EventCostExceeded, Error: ce.Error()}))
			}
			return RunOutcome{Status: RunStatusError}, ce
		}
	}

	// Fresh-run path: create a new WorkflowRun record so the agent loop
	// (and the eventual pause-write, if stopAt fires) has a concrete
	// document to update. RunRepo is optional — when not configured we
	// run without persistence, same behaviour as the old Run().
	if bundle.runID == "" && e.RunRepo != nil {
		id := opts.PreallocRunID
		if id == "" {
			id = ulid.Make().String()
		}
		now := time.Now().UTC()
		runRec := WorkflowRun{
			ID:         id,
			WorkflowID: wf.ID,
			TenantID:   wf.TenantID,
			QueuedAt:   now,
			// RunResumable is the legacy sync path (evals, cron,
			// webhooks, rabbitmq, redis-sub triggers — pre-PR-3.4).
			// It executes immediately in-process so StartedAt is
			// stamped here too. Lease-path runs leave StartedAt nil
			// at dispatch and ClaimLease fills it on first claim.
			StartedAt: &now,
			Status:    RunStatusRunning,
			Params:    wf.Params,
		}
		if opts.Input != nil {
			runRec.TriggerInput = opts.Input
		}
		if created, cerr := e.RunRepo.Create(ctx, runRec); cerr == nil {
			bundle.runID = created.ID
		}
	}

	ctx = context.WithValue(ctx, resumeKey, bundle)

	emitter := opts.Emitter
	if emitter == nil {
		emitter = e.Events
	}

	// Merge legacy single StopAt with the multi StopAtIDs list. Either
	// or both is fine — duplicates collapse via the env.stopAtSet map.
	stopIDs := opts.StopAtIDs
	if opts.StopAt != "" {
		stopIDs = append(stopIDs, opts.StopAt)
	}

	var steps []StepResult
	var err error
	if opts.Input != nil {
		steps, err = e.RunWithEvents(ctx, wf, stopIDs, emitter, opts.Input)
	} else {
		steps, err = e.RunWithEvents(ctx, wf, stopIDs, emitter)
	}
	// Persist final state. The agent loop already wrote PausedAgent + status
	// when stopAtHit fired, so we don't clobber a paused record here.
	//
	// Any step with a non-empty Error promotes the run to status=error.
	// RunWithEvents itself returns nil even on per-node failures (BFS
	// just follows the error edge or drops the queue), so without this
	// scan a cost-cap breach or any handler error showed up as a green
	// "success" badge with an angry red step inside — confusing as hell.
	status := RunStatusSuccess
	stepErr := ""
	if err != nil {
		status = RunStatusError
		stepErr = err.Error()
	}
	if status == RunStatusSuccess {
		for _, s := range steps {
			// Agent-tool dispatch errors are agent-loop concerns, not
			// run-level outcomes. The agent's own StepResult carries
			// the terminal err when it aborts (stop_on_tool_error /
			// sandbox-infra); that's what should promote the run to
			// error. Tool errors the agent recovered from via LLM
			// retry must not retroactively flip the run.
			if s.ViaAgentTool {
				continue
			}
			// `on_error: continue` policy — error recorded on the step
			// but suppressed for run-status promotion. UI distinguishes
			// the suppressed-error step via the Continued flag so the
			// failure stays visible to operators.
			if s.Continued {
				continue
			}
			if s.Error != "" {
				status = RunStatusError
				stepErr = s.Error
				break
			}
		}
	}
	if bundle.runID != "" && e.RunRepo != nil {
		if rec, gerr := e.RunRepo.Get(ctx, bundle.runID); gerr == nil {
			if rec.Status != RunStatusPaused {
				rec.Status = status
				now := time.Now().UTC()
				rec.FinishedAt = &now
				rec.Steps = steps
				rec.Usage = aggregateUsage(rec.AgentTraces)
				if stepErr != "" {
					rec.Error = stepErr
				}
				// Match RunFromCheckpoint terminal cleanup so a legacy
				// resume that lands terminal also drops the in-flight
				// state — otherwise the run-detail UI keeps showing a
				// stale PendingApproval banner on a completed run.
				rec.ExecutionState = nil
				rec.PausedAgent = nil
				rec.PendingApproval = nil
				_ = e.RunRepo.Update(ctx, rec)
			} else {
				status = RunStatusPaused
			}
		}
	}
	return RunOutcome{Steps: steps, RunID: bundle.runID, Status: status}, err
}

// RunFromCheckpoint is the lease-aware entry point used by the
// durable-execution worker loop. The caller has already claimed the
// run's lease via WorkflowRunStore.ClaimLease and (when restarting an
// existing run) handed in the loaded WorkflowRun record so we don't
// double-fetch.
//
// Behaviour:
//   - Stashes a checkpointBundle on ctx so RunWithEvents wires
//     env.workerID + env.checkpoint=true + env.yieldOnApproval=true
//     and hydrates BFS state from rec.ExecutionState (when present).
//   - On normal completion, persists the terminal status (success /
//     error / paused-via-yield) the same way RunResumable does. The
//     worker loop releases the lease on its way back to ClaimLease.
//   - On approval-gate yield, RunWithEvents persists Pending +
//     releases the lease internally; this method returns
//     RunStatusPendingApproval so the worker loop knows not to mark
//     terminal.
//
// Initial input is taken from rec.TriggerInput (already stamped on the
// rec by whoever queued the run). No PreallocRunID etc — the run rec
// is the source of truth.
func (e *WorkflowExecutor) RunFromCheckpoint(ctx context.Context, wf Workflow, rec WorkflowRun, workerID string, emitter EventEmitter) (RunOutcome, error) {
	bundle := &checkpointBundle{
		runID:       rec.ID,
		workerID:    workerID,
		priorState:  rec.ExecutionState,
		pausedAgent: rec.PausedAgent,
	}
	ctx = context.WithValue(ctx, checkpointKey, bundle)
	if emitter == nil {
		emitter = e.Events
	}
	var steps []StepResult
	var err error
	if rec.TriggerInput != nil {
		steps, err = e.RunWithEvents(ctx, wf, nil, emitter, rec.TriggerInput)
	} else {
		steps, err = e.RunWithEvents(ctx, wf, nil, emitter)
	}
	// Determine final status. Lease loss surfaces as ErrLeaseNotHeld
	// — propagate so the worker loop can re-claim or move on.
	if errors.Is(err, ErrLeaseNotHeld) {
		return RunOutcome{Steps: steps, RunID: rec.ID, Status: rec.Status}, err
	}
	// Re-read rec to see whether the executor flipped status to
	// pending_approval (yield path persists status itself).
	cur, gerr := e.RunRepo.Get(ctx, rec.ID)
	if gerr == nil && cur.Status == RunStatusPendingApproval {
		return RunOutcome{Steps: steps, RunID: rec.ID, Status: RunStatusPendingApproval}, nil
	}
	status := RunStatusSuccess
	stepErr := ""
	if err != nil {
		status = RunStatusError
		stepErr = err.Error()
	}
	if status == RunStatusSuccess {
		for _, s := range steps {
			// Agent-tool dispatch errors are agent-loop concerns, not
			// run-level outcomes. The agent's own StepResult carries
			// the terminal err when it aborts (stop_on_tool_error /
			// sandbox-infra); that's what should promote the run to
			// error. Tool errors the agent recovered from via LLM
			// retry must not retroactively flip the run.
			if s.ViaAgentTool {
				continue
			}
			// `on_error: continue` — same suppression rule as the
			// RunResumable terminal scan above. See StepResult.Continued.
			if s.Continued {
				continue
			}
			if s.Error != "" {
				status = RunStatusError
				stepErr = s.Error
				break
			}
		}
	}
	if e.RunRepo != nil {
		rcur, rerr := e.RunRepo.Get(ctx, rec.ID)
		if rerr != nil {
			slog.Warn("workflow: terminal Get failed; skipping cleanup", "run_id", rec.ID, "err", rerr)
		} else if rcur.Status == RunStatusPaused {
			status = RunStatusPaused
		} else {
			rcur.Status = status
			now := time.Now().UTC()
			rcur.FinishedAt = &now
			rcur.Steps = steps
			rcur.Usage = aggregateUsage(rcur.AgentTraces)
			if stepErr != "" {
				rcur.Error = stepErr
			}
			// Clear all in-flight state on terminal — no further resume
			// needed; keeps the doc tidy + prevents stale fields from
			// hydrating a future re-run or the UI's pending-approval
			// banner from sticking on a completed run.
			rcur.ExecutionState = nil
			rcur.PausedAgent = nil
			rcur.PendingApproval = nil
			if uerr := e.RunRepo.Update(ctx, rcur); uerr != nil {
				slog.Warn("workflow: persist terminal state failed", "run_id", rec.ID, "err", uerr)
			}
		}
	}
	return RunOutcome{Steps: steps, RunID: rec.ID, Status: status}, err
}

// RunWithEvents is Run with an explicit per-invocation EventEmitter. Pass
// nil to disable streaming (still records traces via RunRepo as before).
func (e *WorkflowExecutor) RunWithEvents(ctx context.Context, wf Workflow, stopAtIDs []string, emitter EventEmitter, initialInput ...any) ([]StepResult, error) {
	byID := make(map[string]Node, len(wf.Nodes))
	for _, n := range wf.Nodes {
		byID[n.ID] = n
	}

	adj := make(map[string][]adjEntry, len(wf.Edges))
	for _, edge := range wf.Edges {
		h := strings.ToLower(edge.SourceHandle)
		// Prefer paletteType for all edges (not just dh-*); legacy edges
		// without paletteType fall through to sourceHandle for backwards compat.
		if pt, ok := edge.Data["paletteType"].(string); ok && pt != "" {
			h = strings.ToLower(pt)
		}
		adj[edge.Source] = append(adj[edge.Source], adjEntry{
			targetID:     edge.Target,
			sourceHandle: h,
		})
	}

	// body nodes — full subgraph reachable via "item" edges from for_each nodes.
	// These are skipped by main BFS; runForEach executes them per-item instead.
	forEachBodies := buildForEachBodies(wf.Nodes, adj)

	type queueItem struct {
		nodeID string
		input  any
	}
	// When initialInput is provided (e.g. from RabbitMQ message), trigger nodes
	// produce that value as output instead of nil.
	var triggerOutput any
	if len(initialInput) > 0 {
		triggerOutput = initialInput[0]
	}

	var queue []queueItem
	for _, n := range wf.Nodes {
		if n.Type == NodeTypeTrigger {
			queue = append(queue, queueItem{nodeID: n.ID, input: triggerOutput})
		}
	}

	visited := make(map[string]bool)
	var results []StepResult
	wfCtx := make(runCtx)
	params := wf.Params
	if params == nil {
		params = map[string]string{}
	}

	// Stash run env so agent loop (and any future node type that needs
	// graph access) can find tool-edges + node lookup tables without us
	// expanding every handler's signature.
	if emitter == nil {
		emitter = noopEmitter{}
	}

	stopAtSet := make(map[string]bool, len(stopAtIDs))
	for _, id := range stopAtIDs {
		if id != "" {
			stopAtSet[id] = true
		}
	}

	env := &runEnv{
		wf:        &wf,
		byID:      byID,
		adj:       adj,
		tenantID:  wf.TenantID,
		toolSteps: &toolStepCollector{},
		events:    emitter,
		stopAtSet: stopAtSet,
	}
	// Resume hydration: when called via RunResumable a bundle is stashed in
	// ctx with the previous run's ID, paused agent state, and the set of
	// node IDs already executed. We pre-mark visited so the BFS doesn't
	// re-run the http_request etc. that completed before the pause.
	if rb, ok := ctx.Value(resumeKey).(*resumeBundle); ok && rb != nil {
		env.resumeAgentState = rb.agent
		env.resumeRunID = rb.runID
		env.runID = rb.runID
		env.continueCh = rb.continueCh
		env.approveCh = rb.approveCh
		env.completedNodeIDs = rb.completedIDs
		for id := range rb.completedIDs {
			visited[id] = true
		}
		// Live-breakpoint forwarder: when the WS handler wired a
		// breakpointsCh, spawn a goroutine that mirrors each new ID list
		// onto the run's stopAtSet. Lets the user add or remove
		// breakpoints mid-run; the next BFS step picks up the change.
		if rb.breakpointsCh != nil {
			go func(ch chan []string) {
				for {
					select {
					case <-ctx.Done():
						return
					case ids, ok := <-ch:
						if !ok {
							return
						}
						env.SetBreakpoints(ids)
					}
				}
			}(rb.breakpointsCh)
		}
	}
	// Checkpoint hydration: when called via RunFromCheckpoint (worker-
	// loop entry) a checkpointBundle is stashed in ctx. If the run
	// already has a persisted ExecutionState (mid-run resume), we
	// replace the trigger-derived BFS frontier with the persisted
	// queue + visited and apply any landed approval decision before
	// re-entering the loop. Empty priorState (fresh lease-claimed
	// run) falls through to the trigger path.
	if cb, ok := ctx.Value(checkpointKey).(*checkpointBundle); ok && cb != nil {
		env.runID = cb.runID
		env.workerID = cb.workerID
		env.checkpoint = true
		env.yieldOnApproval = true
		env.priorExecState = cb.priorState
		// Hydrate the agent's mid-loop snapshot so the agent at the
		// gated tool call resumes with its iter / messages / usage
		// intact instead of restarting the conversation. The agent
		// itself reads env.resumeAgentState in runAIAgent (line ~209).
		if cb.pausedAgent != nil {
			env.resumeAgentState = cb.pausedAgent
		}
		if cb.priorState != nil {
			// Drop trigger queue; load persisted frontier instead.
			queue = queue[:0]
			for _, q := range cb.priorState.Queue {
				queue = append(queue, queueItem{nodeID: q.NodeID, input: q.Input})
			}
			for _, id := range cb.priorState.Visited {
				visited[id] = true
			}
			if cb.priorState.WfCtx != nil {
				for k, v := range cb.priorState.WfCtx {
					wfCtx[k] = v
				}
			}
			// Apply pre-recorded approval decision (if any) before BFS:
			// Approved → mark the gated node as pre-approved so the
			// pre-exec gate doesn't re-fire. Rejected → emit the
			// rejection step + follow the error edge so the run
			// continues without re-running the gated node. Either
			// way, clear Pending so a second yield doesn't replay
			// the same gate.
			if pg := cb.priorState.Pending; pg != nil && pg.Decision != nil {
				// Per-tool approval (agent gated on a specific tool
				// call). The resumed agent re-prompts the LLM with the
				// trimmed-trailing-assistant message stack and emits a
				// FRESH tool_call (new ID, same name + same intent
				// because input messages are identical). We match on
				// ToolName, not ToolCallID.
				if pg.Kind == "tool_call" && pg.ToolName != "" {
					if env.approvedToolCallNames == nil {
						env.approvedToolCallNames = map[string]*ApprovalDecision{}
					}
					decisionCopy := *pg.Decision
					env.approvedToolCallNames[pg.ToolName] = &decisionCopy
					cb.priorState.Pending = nil
					goto pendingApplied
				}
				if pg.Decision.Approved {
					if env.approvedNodeIDs == nil {
						env.approvedNodeIDs = map[string]bool{}
					}
					env.approvedNodeIDs[pg.NodeID] = true
				} else {
					gatedNode := byID[pg.NodeID]
					rejErr := "rejected by user"
					if pg.Decision.Reason != "" {
						rejErr = "rejected by user: " + pg.Decision.Reason
					}
					emitter.Emit(stampNow(RunEvent{Type: EventStepStart, NodeID: pg.NodeID, NodeType: gatedNode.Type}))
					emitter.Emit(stampNow(RunEvent{Type: EventStepDone, NodeID: pg.NodeID, NodeType: gatedNode.Type, Error: rejErr, IsError: true}))
					// Approval rejection is a SECURITY VETO, not a node
					// fault. `on_error` does NOT apply: a human said no,
					// so the run must land `error` regardless of the
					// gate node's policy — otherwise `on_error: continue`
					// would be a config-level bypass of the gate, which
					// defeats its purpose. Continued stays false; only
					// error edges route (legacy behaviour, no dual-edge).
					rejStep := StepResult{NodeID: pg.NodeID, NodeType: gatedNode.Type, Error: rejErr}
					results = append(results, rejStep)
					// Drop the gated node from the queue front; route
					// any error-edge children in its place.
					var nextQueue []queueItem
					skipped := false
					for _, q := range queue {
						if !skipped && q.nodeID == pg.NodeID {
							skipped = true
							continue
						}
						nextQueue = append(nextQueue, q)
					}
					queue = nextQueue
					for _, et := range adj[pg.NodeID] {
						matches := et.sourceHandle == "error"
						if matches && !visited[et.targetID] {
							queue = append(queue, queueItem{nodeID: et.targetID, input: pg.Input})
						}
					}
					visited[pg.NodeID] = true
				}
				// Clear local mirror so the post-BFS persist doesn't
				// re-write the consumed gate.
				cb.priorState.Pending = nil
			}
		pendingApplied:
		}
	}
	ctx = context.WithValue(ctx, runEnvKey, env)

	// snapshotExecState materialises the current BFS state into an
	// ExecutionState ready for persistence. Visited is sorted-stable
	// for deterministic bson encoding so two consecutive checkpoints
	// without progress write the same document.
	snapshotExecState := func() ExecutionState {
		visIDs := make([]string, 0, len(visited))
		for id := range visited {
			visIDs = append(visIDs, id)
		}
		sort.Strings(visIDs)
		qn := make([]QueuedNode, 0, len(queue))
		for _, q := range queue {
			qn = append(qn, QueuedNode{NodeID: q.nodeID, Input: q.input})
		}
		var ctxCopy map[string]StepContext
		if len(wfCtx) > 0 {
			ctxCopy = make(map[string]StepContext, len(wfCtx))
			for k, v := range wfCtx {
				ctxCopy[k] = v
			}
		}
		return ExecutionState{Visited: visIDs, Queue: qn, WfCtx: ctxCopy}
	}

	// checkpointBFS persists the current BFS snapshot. Called after
	// every node visit when a lease is held; no-op otherwise. Lease
	// loss (ErrLeaseNotHeld) is fatal — we abort the loop, the worker
	// releases its (lost) lease, and the next claim picks up the run.
	checkpointBFS := func() error {
		if !env.checkpoint || env.workerID == "" || e.RunRepo == nil || env.runID == "" {
			return nil
		}
		state := snapshotExecState()
		return e.RunRepo.CheckpointExecutionState(ctx, env.runID, env.workerID, state, results, "")
	}

	// yieldForApproval persists the lease-yield state and returns. Used
	// by both the BFS-side gate (preExecApproval returns errYield) and
	// the agent-side gate (`as_tool` target's `require_node_approval`
	// fires inside the agent's tool dispatch and bubbles errYield up
	// through runAIAgent). Both paths un-mark the just-marked-visited
	// `item` and re-queue it at the head; on resume, BFS re-dispatches
	// `item` and the agent's PausedAgent (persisted by the agent loop
	// before unwinding) hydrates so the model picks up mid-conversation.
	yieldForApproval := func(item queueItem) error {
		delete(visited, item.nodeID)
		queue = append([]queueItem{item}, queue...)
		if env.checkpoint && e.RunRepo != nil && env.workerID != "" && env.runID != "" {
			state := snapshotExecState()
			kind := env.pendingKind
			if kind == "" {
				kind = "node"
			}
			state.Pending = &PendingExecutionGate{
				Kind:       kind,
				NodeID:     env.pendingNodeID,
				Input:      env.pendingInput,
				ToolName:   env.pendingToolName,
				ToolCallID: env.pendingToolCallID,
			}
			if perr := e.RunRepo.CheckpointExecutionState(ctx, env.runID, env.workerID, state, results, RunStatusPendingApproval); perr != nil {
				return perr
			}
			_ = e.RunRepo.ReleaseLease(ctx, env.runID, env.workerID)
		}
		return nil
	}

	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]
		if visited[item.nodeID] || forEachBodies[item.nodeID] {
			continue
		}
		visited[item.nodeID] = true

		node, ok := byID[item.nodeID]
		if !ok {
			continue
		}

		var (
			output       any
			err          error
			extraResults []StepResult
		)

		// Pre-exec breakpoint: when a node is about to run AND it's in
		// the breakpoint set, hold here until the live UI sends a
		// continue frame. Skipped when no continueCh is wired (then the
		// post-exec stopAt path takes over).
		if env.isBreakpoint(node.ID) && env.continueCh != nil {
			env.waitAtBreakpoint(ctx, node)
		}

		// Pre-exec approval gate. Routes through preExecApproval which
		// picks live-UI (continueCh) vs OOB-block-on-channel vs
		// lease-yield automatically based on env wiring.
		if e.requireNodeApproval(node) {
			approved, gateErr := e.preExecApproval(ctx, env, node, item.input)
			if gateErr != nil {
				if errors.Is(gateErr, errYieldForApproval) {
					if perr := yieldForApproval(item); perr != nil {
						return results, perr
					}
					return results, nil
				}
				return results, gateErr
			}
			if !approved {
				rejErr := "rejected by user"
				emitter.Emit(stampNow(RunEvent{
					Type:     EventStepStart,
					NodeID:   node.ID,
					NodeType: node.Type,
				}))
				emitter.Emit(stampNow(RunEvent{
					Type:     EventStepDone,
					NodeID:   node.ID,
					NodeType: node.Type,
					Error:    rejErr,
					IsError:  true,
				}))
				// Approval rejection is a SECURITY VETO, not a node
				// fault. `on_error` does NOT apply (see the durable-
				// resume rejection path above for the rationale): a
				// human veto always flips the run to `error` and routes
				// only error edges, regardless of the gate's policy.
				rejStep := StepResult{
					NodeID:   node.ID,
					NodeType: node.Type,
					Error:    rejErr,
				}
				results = append(results, rejStep)
				for _, et := range adj[item.nodeID] {
					matches := et.sourceHandle == "error"
					if matches && !visited[et.targetID] {
						queue = append(queue, queueItem{nodeID: et.targetID, input: item.input})
					}
				}
				continue
			}
		}

		emitter.Emit(stampNow(RunEvent{
			Type:     EventStepStart,
			NodeID:   node.ID,
			NodeType: node.Type,
		}))

		if node.Type == NodeTypeForEach {
			output, extraResults, err = e.runForEach(ctx, node, item.input, adj, byID, wfCtx, params)
			results = append(results, extraResults...)
		} else {
			output, err = e.runNode(ctx, node, item.input, wfCtx, params)
		}

		// Yield catch: an `as_tool` target's pre-exec gate inside an
		// agent's tool dispatch bubbles errYieldForApproval up through
		// runAIAgent's return. Treat it identically to the BFS-side
		// gate yield — re-queue the agent (so resume re-dispatches it
		// and the agent's persisted PausedAgent hydrates the loop) +
		// persist Pending + release lease. Without this catch, the
		// errYield falls into the generic error-handle branch below
		// and the run terminal-errors mid-step.
		if errors.Is(err, errYieldForApproval) {
			if perr := yieldForApproval(item); perr != nil {
				return results, perr
			}
			return results, nil
		}

		// Populate context for named nodes
		if name, _ := node.Data["name"].(string); name != "" {
			wfCtx[name] = StepContext{Input: item.input, Output: output}
		}

		sr := StepResult{NodeID: node.ID, NodeType: node.Type, Output: output}
		handle := "success"
		if err != nil {
			sr.Error = err.Error()
			handle = "error"
			if onErrorPolicy(node.Data) == "continue" {
				sr.Continued = true
			}
			slog.Warn("workflow: node error", "node", node.ID, "type", node.Type, "err", err, "continued", sr.Continued)
		}
		results = append(results, sr)

		stepDone := RunEvent{
			Type:     EventStepDone,
			NodeID:   node.ID,
			NodeType: node.Type,
			Output:   output,
		}
		if err != nil {
			stepDone.Error = err.Error()
			stepDone.IsError = true
		}
		// When the node is the agent that paused mid-loop (its output
		// carries `paused: true` and `env.stopAtHit` is set), tag the
		// event so the UI renders "paused" instead of green/success.
		if env.stopAtHit && node.Type == NodeTypeAIAgent {
			stepDone.Status = "paused"
		}
		emitter.Emit(stampNow(stepDone))

		// Post-exec breakpoint fallback. Only fires when no continueCh
		// is wired (i.e. the caller didn't enable pre-exec pausing).
		// With continueCh, the pre-exec waitAtBreakpoint already held
		// the BFS before this node ran; firing the post-exec halt
		// AGAIN after continue would emit a spurious run_done(paused)
		// after the run actually finished, which sticks the UI's
		// `pausedRunID` and makes the Continue button persist with no
		// real run to resume. ("workflow run X is not paused" error.)
		if env.isBreakpoint(node.ID) && env.continueCh == nil {
			results = append(results, env.toolSteps.drain()...)
			emitter.Emit(stampNow(RunEvent{Type: EventRunDone, Status: "paused", RunID: env.runID}))
			return results, nil
		}
		// A tool-invoked node (HTTP/Redis as_tool target) bypasses main
		// BFS but may have hit the breakpoint. Short-circuit here so the
		// run halts immediately rather than continuing past the agent.
		if env.stopAtHit {
			results = append(results, env.toolSteps.drain()...)
			emitter.Emit(stampNow(RunEvent{Type: EventRunDone, Status: "paused", RunID: env.runID}))
			return results, nil
		}

		for _, et := range adj[item.nodeID] {
			if et.sourceHandle == "item" {
				continue // for_each body; not traversed by main BFS
			}
			matches := et.sourceHandle == handle || et.sourceHandle == "" || et.sourceHandle == "start"
			// `on_error: continue` + node errored → also fire success
			// edges so the happy path keeps rolling. Error edges
			// already matched above (handle == "error"). Output is nil
			// from a failing handler; downstream nodes see nil input
			// and discriminate explicitly if needed.
			if !matches && sr.Continued && et.sourceHandle == "success" {
				matches = true
			}
			if matches {
				if !visited[et.targetID] {
					queue = append(queue, queueItem{nodeID: et.targetID, input: output})
				}
			}
		}

		// Per-visit checkpoint (lease-held runs only). On lease loss
		// abort the loop without writing further state.
		if cerr := checkpointBFS(); cerr != nil {
			if errors.Is(cerr, ErrLeaseNotHeld) {
				slog.Warn("workflow: lease lost mid-BFS — abandoning run-pass", "run_id", env.runID, "node", node.ID, "worker", env.workerID)
				return results, cerr
			}
			slog.Warn("workflow: BFS checkpoint write failed", "run_id", env.runID, "node", node.ID, "err", cerr)
		}
	}

	// Append step results from tool-invoked nodes so the UI shows them
	// as executed. Order matches tool-call sequence (agent loop adds
	// per-call) — preserved by the mutex-guarded slice.
	results = append(results, env.toolSteps.drain()...)

	emitter.Emit(stampNow(RunEvent{Type: EventRunDone, Status: "success", RunID: env.runID}))

	return results, nil
}

// buildForEachBodies BFS-marks all nodes reachable via "item" edges from for_each nodes.
// These nodes are the loop body and are skipped by the main BFS.
func buildForEachBodies(nodes []Node, adj map[string][]adjEntry) map[string]bool {
	bodies := make(map[string]bool)
	for _, n := range nodes {
		if n.Type != NodeTypeForEach {
			continue
		}
		for _, et := range adj[n.ID] {
			if et.sourceHandle != "item" {
				continue
			}
			// BFS from item target to mark full body subgraph
			q := []string{et.targetID}
			for len(q) > 0 {
				id := q[0]
				q = q[1:]
				if bodies[id] {
					continue
				}
				bodies[id] = true
				for _, next := range adj[id] {
					if !bodies[next.targetID] {
						q = append(q, next.targetID)
					}
				}
			}
		}
	}
	return bodies
}

// runForEach iterates input as an array and runs the full body chain
// (starting from each "item" target) once per element.
// Returns aggregated step results and a slice of per-item final outputs.
func (e *WorkflowExecutor) runForEach(
	ctx context.Context,
	node Node,
	input any,
	adj map[string][]adjEntry,
	byID map[string]Node,
	wfCtx runCtx,
	params map[string]string,
) (any, []StepResult, error) {
	// By default for_each iterates its raw input. An optional `items`
	// selector lets it iterate a field of the input instead — e.g. a
	// preceding mongo_request `find` emits {docs:[…],cursor:{…}}, so
	// `items: "{{input.docs}}"` loops per document rather than once over
	// the whole result envelope. expandSlice widens any slice kind
	// ([]bson.M from mongo, []any from JSON, …); a non-slice falls back
	// to toSlice's single-element-wrap contract.
	items := toSlice(input)
	if sel, ok := node.Data["items"].(string); ok {
		if sel = strings.TrimSpace(sel); sel != "" {
			if v, ok := resolveTemplateValue(sel, input, wfCtx); ok {
				if s, ok := expandSlice(v); ok {
					items = s
				} else {
					items = toSlice(v)
				}
			} else {
				// selector set but unresolved → empty loop, not a
				// silent full-input pass. Authors opted into a field;
				// honour that intent rather than masking a typo.
				items = nil
			}
		}
	}

	var itemTargetIDs []string
	for _, et := range adj[node.ID] {
		if et.sourceHandle == "item" {
			itemTargetIDs = append(itemTargetIDs, et.targetID)
		}
	}

	// Body subgraph (every node reachable from an item target). At the
	// start of each iteration we emit a loop_iter_start for ALL of them
	// so the UI uniformly resets the whole body to "idle · iter K/M" —
	// not just the first node — and the denominator stays M even for a
	// node that isn't traversed every iteration (e.g. an error branch
	// when nothing errored).
	bodySeen := make(map[string]bool)
	{
		q := append([]string{}, itemTargetIDs...)
		for len(q) > 0 {
			bid := q[0]
			q = q[1:]
			if bodySeen[bid] {
				continue
			}
			bodySeen[bid] = true
			for _, et := range adj[bid] {
				if !bodySeen[et.targetID] {
					q = append(q, et.targetID)
				}
			}
		}
	}
	env, _ := envFromCtx(ctx)
	total := len(items)

	var allResults []StepResult
	var outputs []any

	forEachName, _ := node.Data["name"].(string)

	// for_each's own on_error is a LOOP-ABORT policy, distinct from a
	// leaf node's run-status fault:
	//   stop (default) — the first item whose body chain ends with an
	//                    UNSUPPRESSED error (Error != "" && !Continued)
	//                    aborts the loop; remaining items are skipped.
	//   continue       — run every item regardless.
	// Either way the failing body StepResult is already in allResults,
	// so run-status promotion flips the run on its own — this policy
	// only decides whether the REMAINING items still run. A body node
	// with on_error=continue suppresses its error (Continued=true), so
	// it's never "unsuppressed" here: §7's best-effort loop is
	// unchanged regardless of the for_each policy.
	abortOnErr := onErrorPolicy(node.Data) == "stop"
	var abortErr error
	anyFatal := false

	for idx, item := range items {
		// clone parent context per iteration so body steps don't bleed across iterations
		iterCtx := make(runCtx, len(wfCtx))
		for k, v := range wfCtx {
			iterCtx[k] = v
		}
		// expose current iteration element via context if for_each is named
		// body nodes access it as: context.stepName.item.field
		if forEachName != "" {
			iterCtx[forEachName] = StepContext{Input: input, Item: item}
		}
		// Reset every body node to idle for this iteration BEFORE any
		// of them run, so a downstream node still showing the previous
		// iteration's "done" flips to "idle · iter K/M" the moment the
		// loop advances (not only when it personally re-executes).
		if env != nil && env.events != nil {
			for bid := range bodySeen {
				env.events.Emit(stampNow(RunEvent{
					Type:      EventLoopIterStart,
					NodeID:    bid,
					NodeType:  byID[bid].Type,
					LoopIter:  idx + 1,
					LoopTotal: total,
				}))
			}
		}
		// Tag the iteration (1-based) + total onto ctx so body steps
		// (and an agent body) stamp LoopIter+LoopTotal — the UI groups
		// agent traces by iteration and shows a uniform "iter K/M".
		loopCtx := withForEachLoop(ctx, idx+1, total)

		fatal := false
		for _, startID := range itemTargetIDs {
			chainResults, lastOut := e.runBodyChain(loopCtx, startID, item, adj, byID, iterCtx, params)
			allResults = append(allResults, chainResults...)
			if len(chainResults) > 0 && chainResults[len(chainResults)-1].Error == "" {
				outputs = append(outputs, lastOut)
			}
			for _, sr := range chainResults {
				if sr.Error != "" && !sr.Continued {
					fatal = true
				}
			}
		}
		if fatal {
			anyFatal = true
		}
		if abortOnErr && fatal {
			slog.Warn("for_each: unsuppressed body error — aborting loop (on_error=stop)",
				"node", node.ID, "item_index", idx, "remaining", len(items)-idx-1)
			// stop: abort the loop now and surface a node-level error
			// so the main BFS routes ONLY the for_each `error` edge
			// (Continued=false → no dual success edge) + flips the run.
			abortErr = fmt.Errorf("for_each: loop aborted at item %d on unsuppressed body error (on_error=stop)", idx)
			break
		}
	}

	// continue: every item ran (best-effort) but at least one ended
	// with an unsuppressed fault. Still surface a node-level error so
	// the main BFS fires the for_each `error` edge — and because the
	// for_each node's policy is `continue`, the BFS marks it Continued
	// so the `success` edge fires too (dual-edge, same ideology as a
	// per-node continue). Run-status still flips via the unsuppressed
	// body step. (abortOnErr already returned above with abortErr.)
	if abortErr == nil && anyFatal {
		abortErr = fmt.Errorf("for_each: completed with unsuppressed body error(s) (on_error=continue)")
	}

	return outputs, allResults, abortErr
}

// runBodyChain executes a linear chain starting from startID with the given input.
// Follows success/error edges within the body; does not re-enter main BFS nodes.
func (e *WorkflowExecutor) runBodyChain(
	ctx context.Context,
	startID string,
	input any,
	adj map[string][]adjEntry,
	byID map[string]Node,
	wfCtx runCtx,
	params map[string]string,
) ([]StepResult, any) {
	env, _ := envFromCtx(ctx)
	var results []StepResult
	visited := make(map[string]bool)
	type bodyItem struct {
		nodeID string
		input  any
	}
	queue := []bodyItem{{nodeID: startID, input: input}}
	var lastOut any

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur.nodeID == "" || visited[cur.nodeID] {
			continue
		}
		visited[cur.nodeID] = true

		node, ok := byID[cur.nodeID]
		if !ok {
			continue
		}

		// for_each body nodes run outside the main BFS, so they must
		// emit their own step_start/step_done — otherwise the live
		// canvas never lights them and they show "not executed" even
		// though they ran (and produced side effects). LoopIter lets
		// the UI attribute the step to its iteration.
		loopIter, loopTotal := forEachLoopFromCtx(ctx)
		if env != nil && env.events != nil {
			env.events.Emit(stampNow(RunEvent{
				Type:      EventStepStart,
				NodeID:    node.ID,
				NodeType:  node.Type,
				LoopIter:  loopIter,
				LoopTotal: loopTotal,
			}))
		}

		output, err := e.runNode(ctx, node, cur.input, wfCtx, params)

		// Populate context for named body nodes
		if name, _ := node.Data["name"].(string); name != "" {
			wfCtx[name] = StepContext{Input: cur.input, Output: output}
		}

		sr := StepResult{NodeID: node.ID, NodeType: node.Type, Output: output}
		handle := "success"
		if err != nil {
			sr.Error = err.Error()
			handle = "error"
			if onErrorPolicy(node.Data) == "continue" {
				sr.Continued = true
			}
			slog.Warn("for_each body: node error", "node", node.ID, "err", err, "continued", sr.Continued)
		}
		results = append(results, sr)
		lastOut = output

		if env != nil && env.events != nil {
			done := RunEvent{
				Type:      EventStepDone,
				NodeID:    node.ID,
				NodeType:  node.Type,
				Output:    output,
				LoopIter:  loopIter,
				LoopTotal: loopTotal,
			}
			if err != nil {
				done.Error = err.Error()
				done.IsError = true
			}
			env.events.Emit(stampNow(done))
		}
		if env != nil && env.isBreakpoint(node.ID) {
			return results, output
		}

		// Edge routing mirrors the main BFS EXACTLY so the for_each
		// body honours the same `on_error: continue` dual-edge rule:
		// a continued error fires BOTH the error edge AND the success
		// edge (happy path keeps rolling, error edge = optional
		// sidecar). The old single-successor `break` made the success
		// branch unreachable whenever an error edge was also wired.
		for _, et := range adj[node.ID] {
			if et.sourceHandle == "item" {
				continue // nested for_each body marker; not this chain
			}
			matches := et.sourceHandle == handle || et.sourceHandle == ""
			if !matches && sr.Continued && et.sourceHandle == "success" {
				matches = true
			}
			if matches && !visited[et.targetID] {
				queue = append(queue, bodyItem{nodeID: et.targetID, input: output})
			}
		}
	}

	return results, lastOut
}

// runNode dispatches execution to the appropriate handler for node.Type.
// Params are resolved in all string data fields before dispatch (except "script").
func (e *WorkflowExecutor) runNode(ctx context.Context, node Node, input any, wfCtx runCtx, params map[string]string) (any, error) {
	data := applyParamsToData(node.Data, params)
	switch node.Type {
	case NodeTypeTrigger:
		return input, nil // pass-through; initialInput flows via the queue item
	case NodeTypeHTTPRequest:
		return e.runHTTPRequest(ctx, data, input, wfCtx)
	case NodeTypeSandboxScript:
		return e.runSandboxScript(ctx, data, input, wfCtx, params)
	case NodeTypeForEach:
		return nil, fmt.Errorf("for_each dispatched via runNode — use runForEach instead")
	case NodeTypeMongoRequest:
		return e.runMongoRequest(ctx, data, input)
	case NodeTypeRedisRequest:
		return e.runRedisRequest(ctx, data, input)
	case NodeTypeNotify:
		return runNotify(data, input)
	case NodeTypeAIAgent:
		return e.runAIAgent(ctx, node, data, input, wfCtx, params)
	default:
		return nil, fmt.Errorf("unknown node type: %s", node.Type)
	}
}

// runHTTPRequest performs an arbitrary HTTP request with full Go http.Client
// parity. Strings (url, header values, query values, body, bearer_token,
// basic_auth_*) support {{input.FIELD}} / {{context.stepName.*.FIELD}} templates.
//
// Data fields:
//
//	url                       string  required
//	method                    string  default "GET"
//	headers                   map[string]string
//	query                     map[string]string  appended to URL query string
//	body                      string             raw request body
//	body_json                 any                marshalled to JSON; sets Content-Type
//	body_form                 map[string]string  form-urlencoded; sets Content-Type
//	timeout_seconds           number  default 30
//	follow_redirects          bool    default true
//	max_redirects             number  default 10
//	basic_auth_username       string
//	basic_auth_password       string
//	bearer_token              string  sent as "Authorization: Bearer ..."
//	user_agent                string  overrides default User-Agent
//	tls_insecure_skip_verify  bool    default false
//	parse_json                bool    parse response body as JSON into output.json
//	max_response_bytes        number  default 10 MiB; 0 = unlimited
//	accept_any_status         bool    default false; when true, non-2xx is success
//
// Output:
//
//	ok           bool
//	status       int
//	status_text  string
//	headers      map[string][]string
//	body         string
//	json         any (only when parse_json=true and decode succeeds)
func (e *WorkflowExecutor) runHTTPRequest(ctx context.Context, data map[string]any, input any, wfCtx runCtx) (any, error) {
	rawURL, _ := data["url"].(string)
	if rawURL == "" {
		return nil, fmt.Errorf("http_request: url is required")
	}
	rawURL = applyTemplate(rawURL, input, wfCtx)

	method := strings.ToUpper(strings.TrimSpace(getStringData(data, "method")))
	if method == "" {
		method = http.MethodGet
	}

	// Build URL with query merge
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("http_request: parse url: %w", err)
	}
	if qm := stringMap(data["query"]); len(qm) > 0 {
		q := parsedURL.Query()
		for k, v := range qm {
			q.Set(k, applyTemplate(v, input, wfCtx))
		}
		parsedURL.RawQuery = q.Encode()
	}

	// Build body
	var (
		bodyReader  io.Reader
		contentType string
	)
	if bj, ok := data["body_json"]; ok && bj != nil {
		b, err := json.Marshal(bj)
		if err != nil {
			return nil, fmt.Errorf("http_request: marshal body_json: %w", err)
		}
		// Apply templates only to string-valued bodies (body_json is structured)
		bodyReader = bytes.NewReader(b)
		contentType = "application/json"
	} else if bf := stringMap(data["body_form"]); len(bf) > 0 {
		form := url.Values{}
		for k, v := range bf {
			form.Set(k, applyTemplate(v, input, wfCtx))
		}
		bodyReader = strings.NewReader(form.Encode())
		contentType = "application/x-www-form-urlencoded"
	} else if raw, _ := data["body"].(string); raw != "" {
		bodyReader = strings.NewReader(applyTemplate(raw, input, wfCtx))
	}

	req, err := http.NewRequestWithContext(ctx, method, parsedURL.String(), bodyReader)
	if err != nil {
		return nil, fmt.Errorf("http_request: build request: %w", err)
	}

	// Headers
	if hm := stringMap(data["headers"]); len(hm) > 0 {
		for k, v := range hm {
			req.Header.Set(k, applyTemplate(v, input, wfCtx))
		}
	}
	if contentType != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", contentType)
	}
	if ua := getStringData(data, "user_agent"); ua != "" {
		req.Header.Set("User-Agent", applyTemplate(ua, input, wfCtx))
	} else if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; burrow/1.0)")
	}

	// Auth
	if bt := getStringData(data, "bearer_token"); bt != "" {
		req.Header.Set("Authorization", "Bearer "+applyTemplate(bt, input, wfCtx))
	}
	if u := getStringData(data, "basic_auth_username"); u != "" {
		p := getStringData(data, "basic_auth_password")
		req.SetBasicAuth(applyTemplate(u, input, wfCtx), applyTemplate(p, input, wfCtx))
	}

	// Client (per-request to honour timeout/redirects/TLS overrides)
	client := buildHTTPClient(e.HTTPClient, data)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http_request: %w", err)
	}
	defer resp.Body.Close()

	maxBytes := defaultMaxResponseBytes
	if v, ok := numberData(data, "max_response_bytes"); ok {
		maxBytes = int64(v)
	}
	var bodyBytes []byte
	if maxBytes > 0 {
		bodyBytes, err = io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
		if err != nil {
			return nil, fmt.Errorf("http_request: read body: %w", err)
		}
		if int64(len(bodyBytes)) > maxBytes {
			return nil, fmt.Errorf("http_request: response body exceeds max_response_bytes (%d)", maxBytes)
		}
	} else {
		bodyBytes, err = io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("http_request: read body: %w", err)
		}
	}

	ok2xx := resp.StatusCode >= 200 && resp.StatusCode < 300
	acceptAny, _ := data["accept_any_status"].(bool)
	if !ok2xx && !acceptAny {
		return nil, fmt.Errorf("http_request: status %d from %s", resp.StatusCode, parsedURL.String())
	}

	out := map[string]any{
		"ok":          ok2xx,
		"status":      resp.StatusCode,
		"status_text": resp.Status,
		"headers":     map[string][]string(resp.Header),
		"body":        string(bodyBytes),
	}
	if parse, _ := data["parse_json"].(bool); parse && len(bodyBytes) > 0 {
		var parsed any
		if jerr := json.Unmarshal(bodyBytes, &parsed); jerr == nil {
			out["json"] = parsed
		} else {
			out["json_error"] = jerr.Error()
		}
	}
	return out, nil
}

// buildHTTPClient produces an http.Client honouring per-request overrides:
// timeout, redirect policy, TLS skip-verify. Falls back to base when no
// overrides apply (preserving any shared transport).
func buildHTTPClient(base *http.Client, data map[string]any) *http.Client {
	timeout := 30 * time.Second
	if v, ok := numberData(data, "timeout_seconds"); ok && v > 0 {
		timeout = time.Duration(v) * time.Second
	}

	follow := true
	if v, ok := data["follow_redirects"].(bool); ok {
		follow = v
	}
	maxRedirects := 10
	if v, ok := numberData(data, "max_redirects"); ok && v > 0 {
		maxRedirects = int(v)
	}

	insecure, _ := data["tls_insecure_skip_verify"].(bool)

	// If no TLS override and base client present, reuse its transport with our
	// timeout + redirect policy. Otherwise build a fresh transport.
	var transport http.RoundTripper
	if insecure {
		transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // user-opt-in
		}
	} else if base != nil && base.Transport != nil {
		transport = base.Transport
	}

	c := &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
	if !follow {
		c.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}
	} else {
		c.CheckRedirect = func(_ *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errors.New("max redirects exceeded")
			}
			return nil
		}
	}
	return c
}

// stringMap coerces an arbitrary value into map[string]string. Accepts
// map[string]string, map[string]any, bson.M, and bson.D (values stringified).
// Returns nil on type mismatch.
func stringMap(v any) map[string]string {
	if m, ok := v.(map[string]string); ok {
		return m
	}
	if m, ok := mapAny(v); ok {
		out := make(map[string]string, len(m))
		for k, val := range m {
			out[k] = fmt.Sprint(val)
		}
		return out
	}
	return nil
}

// numberData reads a numeric data field (accepts float64/int variants).
func numberData(data map[string]any, key string) (float64, bool) {
	v, ok := data[key]
	if !ok {
		return 0, false
	}
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

// wfCtxToJS converts runCtx to map[string]any with lowercase keys so sandbox scripts
// access context.stepName.input / .output / .item (not .Input / .Output / .Item).
func wfCtxToJS(wfCtx runCtx) map[string]any {
	js := make(map[string]any, len(wfCtx))
	for name, sc := range wfCtx {
		entry := map[string]any{
			"input":  sc.Input,
			"output": sc.Output,
		}
		if sc.Item != nil {
			entry["item"] = sc.Item
		}
		js[name] = entry
	}
	return js
}

// applyParamsToData resolves {{params.key}} placeholders in all string data fields.
// The "script" key is skipped — sandbox scripts access params via the params global instead.
func applyParamsToData(data map[string]any, params map[string]string) map[string]any {
	if len(params) == 0 {
		return data
	}
	resolved := make(map[string]any, len(data))
	for k, v := range data {
		if k == "script" {
			resolved[k] = v // scripts use params global, not template substitution
			continue
		}
		if s, ok := v.(string); ok {
			for pk, pv := range params {
				s = strings.ReplaceAll(s, "{{params."+pk+"}}", pv)
			}
			resolved[k] = s
		} else {
			resolved[k] = v
		}
	}
	return resolved
}

// runMongoRequest dispatches a single MongoDB operation against the resolved
// database. The operation is selected by data["operation"]; the rest of data
// (filter, update, projection, sort, …) carries op-specific options. Unset
// op-specific bson fields fall through to the upstream `input` where it makes
// sense (e.g. insert_one uses input as the document; aggregate uses input as
// the pipeline). Output shape varies per op — see plan doc / README.
func (e *WorkflowExecutor) runMongoRequest(ctx context.Context, data map[string]any, input any) (any, error) {
	op, _ := data["operation"].(string)
	if op == "" {
		op = "find" // sensible default; agents must still pass collection
	}
	op = strings.ToLower(strings.TrimSpace(op))

	// cursor_fetch is the only op that doesn't require a connection lookup —
	// it reads from the in-memory cursor cache.
	if op == "cursor_fetch" {
		return e.mongoCursorFetch(ctx, data)
	}

	collection, _ := data["collection"].(string)
	if collection == "" {
		return nil, fmt.Errorf("mongo_request: collection is required")
	}

	db, err := e.resolveMongoDB(ctx, data)
	if err != nil {
		return nil, err
	}

	switch op {
	case "find":
		return e.mongoFind(ctx, db, collection, data)
	case "find_one_and_update":
		return e.mongoFindOneAndUpdate(ctx, db, collection, data, input)
	case "find_one_and_replace":
		return e.mongoFindOneAndReplace(ctx, db, collection, data, input)
	case "insert_one":
		return e.mongoInsertOne(ctx, db, collection, data, input)
	case "insert_many":
		return e.mongoInsertMany(ctx, db, collection, data, input)
	case "update_many":
		return e.mongoUpdateMany(ctx, db, collection, data, input)
	case "delete_one":
		return e.mongoDeleteOne(ctx, db, collection, data, input)
	case "delete_many":
		return e.mongoDeleteMany(ctx, db, collection, data, input)
	case "aggregate":
		return e.mongoAggregate(ctx, db, collection, data, input)
	case "count_documents":
		return e.mongoCountDocuments(ctx, db, collection, data, input)
	case "distinct":
		return e.mongoDistinct(ctx, db, collection, data, input)
	default:
		return nil, fmt.Errorf("mongo_request: unknown operation %q", op)
	}
}

// resolveMongoDB picks the default DB or, when data.connection_id is set, the
// resolved per-connection client.
func (e *WorkflowExecutor) resolveMongoDB(ctx context.Context, data map[string]any) (MongoClient, error) {
	db := e.DB
	if connID, _ := data["connection_id"].(string); connID != "" && e.ConnResolver != nil {
		resolved, err := e.ConnResolver.ResolveDB(ctx, connID)
		if err != nil {
			return nil, fmt.Errorf("mongo_request: %w", err)
		}
		db = resolved
	}
	if db == nil {
		return nil, fmt.Errorf("mongo_request: no DB configured")
	}
	return db, nil
}

// --- bson coercion helpers ---

// bsonM coerces a generic value (typically map[string]any from JSON) into bson.M.
// Returns nil if v is nil — callers decide whether nil is acceptable.
func bsonM(v any) (bson.M, error) {
	if v == nil {
		return nil, nil
	}
	if m, ok := mapAny(v); ok {
		return bson.M(m), nil
	}
	return nil, fmt.Errorf("expected object/map, got %T", v)
}

// bsonMOrEmpty returns an empty bson.M instead of nil when v is missing.
func bsonMOrEmpty(v any) (bson.M, error) {
	if v == nil {
		return bson.M{}, nil
	}
	return bsonM(v)
}

// bsonPipeline coerces a generic value into []bson.M.
func bsonPipeline(v any) ([]bson.M, error) {
	if v == nil {
		return nil, fmt.Errorf("pipeline is required")
	}
	if p, ok := v.([]bson.M); ok {
		return p, nil
	}
	if p, ok := sliceAny(v); ok {
		out := make([]bson.M, len(p))
		for i, stage := range p {
			m, err := bsonM(stage)
			if err != nil {
				return nil, fmt.Errorf("pipeline[%d]: %w", i, err)
			}
			out[i] = m
		}
		return out, nil
	}
	return nil, fmt.Errorf("expected array of objects, got %T", v)
}

// bsonDocs coerces a generic value into []bson.M (for insert_many).
func bsonDocs(v any) ([]bson.M, error) {
	if v == nil {
		return nil, fmt.Errorf("documents is required")
	}
	if d, ok := v.([]bson.M); ok {
		return d, nil
	}
	if d, ok := sliceAny(v); ok {
		out := make([]bson.M, len(d))
		for i, doc := range d {
			m, err := bsonM(doc)
			if err != nil {
				return nil, fmt.Errorf("documents[%d]: %w", i, err)
			}
			out[i] = m
		}
		return out, nil
	}
	return nil, fmt.Errorf("expected array of objects, got %T", v)
}

// firstNonNil returns the first non-nil value among the inputs.
func firstNonNil(vs ...any) any {
	for _, v := range vs {
		if v != nil {
			return v
		}
	}
	return nil
}

// --- per-op handlers ---

func (e *WorkflowExecutor) mongoFind(ctx context.Context, db MongoClient, coll string, data map[string]any) (any, error) {
	filter, err := bsonMOrEmpty(data["filter"])
	if err != nil {
		return nil, fmt.Errorf("mongo_request find: filter: %w", err)
	}

	opts := options.Find()
	if v, err := bsonM(data["projection"]); err != nil {
		return nil, fmt.Errorf("mongo_request find: projection: %w", err)
	} else if v != nil {
		opts.SetProjection(v)
	}
	if v, err := bsonM(data["sort"]); err != nil {
		return nil, fmt.Errorf("mongo_request find: sort: %w", err)
	} else if v != nil {
		opts.SetSort(v)
	}
	if v, ok := numberData(data, "skip"); ok {
		opts.SetSkip(int64(v))
	}
	if v, ok := numberData(data, "limit"); ok {
		opts.SetLimit(int64(v))
	}

	cur, err := db.Find(ctx, coll, filter, opts)
	if err != nil {
		return nil, fmt.Errorf("mongo_request find: %w", err)
	}
	return e.serializeCursor(ctx, cur, data)
}

func (e *WorkflowExecutor) mongoFindOneAndUpdate(ctx context.Context, db MongoClient, coll string, data map[string]any, input any) (any, error) {
	filter, err := bsonMOrEmpty(data["filter"])
	if err != nil {
		return nil, fmt.Errorf("mongo_request find_one_and_update: filter: %w", err)
	}
	update, err := bsonM(firstNonNil(data["update"], input))
	if err != nil {
		return nil, fmt.Errorf("mongo_request find_one_and_update: update: %w", err)
	}
	if update == nil {
		return nil, fmt.Errorf("mongo_request find_one_and_update: update is required")
	}

	opts := options.FindOneAndUpdate()
	if upsert, _ := data["upsert"].(bool); upsert {
		opts.SetUpsert(true)
	}
	if v, err := bsonM(data["projection"]); err != nil {
		return nil, fmt.Errorf("mongo_request find_one_and_update: projection: %w", err)
	} else if v != nil {
		opts.SetProjection(v)
	}
	if v, err := bsonM(data["sort"]); err != nil {
		return nil, fmt.Errorf("mongo_request find_one_and_update: sort: %w", err)
	} else if v != nil {
		opts.SetSort(v)
	}
	if rd, _ := data["return_document"].(string); rd != "" {
		switch strings.ToLower(rd) {
		case "after":
			opts.SetReturnDocument(options.After)
		case "before":
			opts.SetReturnDocument(options.Before)
		}
	}
	if af, ok := sliceAny(data["array_filters"]); ok {
		filters := make([]any, 0, len(af))
		for _, f := range af {
			m, err := bsonM(f)
			if err != nil {
				return nil, fmt.Errorf("mongo_request find_one_and_update: array_filters: %w", err)
			}
			filters = append(filters, m)
		}
		opts.SetArrayFilters(filters)
	}

	doc, err := db.FindOneAndUpdate(ctx, coll, filter, update, opts)
	if err != nil {
		return nil, fmt.Errorf("mongo_request find_one_and_update: %w", err)
	}
	return map[string]any{"doc": doc}, nil
}

func (e *WorkflowExecutor) mongoFindOneAndReplace(ctx context.Context, db MongoClient, coll string, data map[string]any, input any) (any, error) {
	filter, err := bsonMOrEmpty(data["filter"])
	if err != nil {
		return nil, fmt.Errorf("mongo_request find_one_and_replace: filter: %w", err)
	}
	repl, err := bsonM(firstNonNil(data["replacement"], input))
	if err != nil {
		return nil, fmt.Errorf("mongo_request find_one_and_replace: replacement: %w", err)
	}
	if repl == nil {
		return nil, fmt.Errorf("mongo_request find_one_and_replace: replacement is required")
	}

	opts := options.FindOneAndReplace()
	if upsert, _ := data["upsert"].(bool); upsert {
		opts.SetUpsert(true)
	}
	if v, err := bsonM(data["projection"]); err != nil {
		return nil, fmt.Errorf("mongo_request find_one_and_replace: projection: %w", err)
	} else if v != nil {
		opts.SetProjection(v)
	}
	if v, err := bsonM(data["sort"]); err != nil {
		return nil, fmt.Errorf("mongo_request find_one_and_replace: sort: %w", err)
	} else if v != nil {
		opts.SetSort(v)
	}
	if rd, _ := data["return_document"].(string); rd != "" {
		switch strings.ToLower(rd) {
		case "after":
			opts.SetReturnDocument(options.After)
		case "before":
			opts.SetReturnDocument(options.Before)
		}
	}

	doc, err := db.FindOneAndReplace(ctx, coll, filter, repl, opts)
	if err != nil {
		return nil, fmt.Errorf("mongo_request find_one_and_replace: %w", err)
	}
	return map[string]any{"doc": doc}, nil
}

func (e *WorkflowExecutor) mongoInsertOne(ctx context.Context, db MongoClient, coll string, data map[string]any, input any) (any, error) {
	doc, err := bsonM(firstNonNil(data["document"], input))
	if err != nil {
		return nil, fmt.Errorf("mongo_request insert_one: document: %w", err)
	}
	if doc == nil {
		return nil, fmt.Errorf("mongo_request insert_one: document is required")
	}
	id, err := db.InsertOne(ctx, coll, doc)
	if err != nil {
		return nil, fmt.Errorf("mongo_request insert_one: %w", err)
	}
	return map[string]any{"inserted_id": id}, nil
}

func (e *WorkflowExecutor) mongoInsertMany(ctx context.Context, db MongoClient, coll string, data map[string]any, input any) (any, error) {
	docs, err := bsonDocs(firstNonNil(data["documents"], input))
	if err != nil {
		return nil, fmt.Errorf("mongo_request insert_many: documents: %w", err)
	}

	opts := options.InsertMany()
	if ordered, ok := data["ordered"].(bool); ok {
		opts.SetOrdered(ordered)
	}

	ids, err := db.InsertMany(ctx, coll, docs, opts)
	if err != nil {
		return nil, fmt.Errorf("mongo_request insert_many: %w", err)
	}
	return map[string]any{"inserted_ids": ids, "inserted_count": len(ids)}, nil
}

func (e *WorkflowExecutor) mongoUpdateMany(ctx context.Context, db MongoClient, coll string, data map[string]any, input any) (any, error) {
	filter, err := bsonMOrEmpty(data["filter"])
	if err != nil {
		return nil, fmt.Errorf("mongo_request update_many: filter: %w", err)
	}
	update, err := bsonM(firstNonNil(data["update"], input))
	if err != nil {
		return nil, fmt.Errorf("mongo_request update_many: update: %w", err)
	}
	if update == nil {
		return nil, fmt.Errorf("mongo_request update_many: update is required")
	}

	opts := options.UpdateMany()
	if upsert, _ := data["upsert"].(bool); upsert {
		opts.SetUpsert(true)
	}
	if af, ok := sliceAny(data["array_filters"]); ok {
		filters := make([]any, 0, len(af))
		for _, f := range af {
			m, err := bsonM(f)
			if err != nil {
				return nil, fmt.Errorf("mongo_request update_many: array_filters: %w", err)
			}
			filters = append(filters, m)
		}
		opts.SetArrayFilters(filters)
	}

	matched, modified, upserted, err := db.UpdateMany(ctx, coll, filter, update, opts)
	if err != nil {
		return nil, fmt.Errorf("mongo_request update_many: %w", err)
	}
	return map[string]any{"matched": matched, "modified": modified, "upserted": upserted}, nil
}

func (e *WorkflowExecutor) mongoDeleteOne(ctx context.Context, db MongoClient, coll string, data map[string]any, input any) (any, error) {
	filter, err := bsonM(firstNonNil(data["filter"], input))
	if err != nil {
		return nil, fmt.Errorf("mongo_request delete_one: filter: %w", err)
	}
	if filter == nil {
		return nil, fmt.Errorf("mongo_request delete_one: filter is required")
	}
	n, err := db.DeleteOne(ctx, coll, filter)
	if err != nil {
		return nil, fmt.Errorf("mongo_request delete_one: %w", err)
	}
	return map[string]any{"deleted_count": n}, nil
}

func (e *WorkflowExecutor) mongoDeleteMany(ctx context.Context, db MongoClient, coll string, data map[string]any, input any) (any, error) {
	filter, err := bsonM(firstNonNil(data["filter"], input))
	if err != nil {
		return nil, fmt.Errorf("mongo_request delete_many: filter: %w", err)
	}
	if filter == nil {
		return nil, fmt.Errorf("mongo_request delete_many: filter is required")
	}
	n, err := db.DeleteMany(ctx, coll, filter)
	if err != nil {
		return nil, fmt.Errorf("mongo_request delete_many: %w", err)
	}
	return map[string]any{"deleted_count": n}, nil
}

func (e *WorkflowExecutor) mongoAggregate(ctx context.Context, db MongoClient, coll string, data map[string]any, input any) (any, error) {
	pipeline, err := bsonPipeline(firstNonNil(data["pipeline"], input))
	if err != nil {
		return nil, fmt.Errorf("mongo_request aggregate: pipeline: %w", err)
	}

	opts := options.Aggregate()
	if v, ok := data["allow_disk_use"].(bool); ok {
		opts.SetAllowDiskUse(v)
	}
	if v, ok := numberData(data, "batch_size"); ok {
		opts.SetBatchSize(int32(v))
	}

	cur, err := db.Aggregate(ctx, coll, pipeline, opts)
	if err != nil {
		return nil, fmt.Errorf("mongo_request aggregate: %w", err)
	}
	return e.serializeCursor(ctx, cur, data)
}

func (e *WorkflowExecutor) mongoCountDocuments(ctx context.Context, db MongoClient, coll string, data map[string]any, input any) (any, error) {
	filter, err := bsonMOrEmpty(firstNonNil(data["filter"], input))
	if err != nil {
		return nil, fmt.Errorf("mongo_request count_documents: filter: %w", err)
	}

	opts := options.Count()
	if v, ok := numberData(data, "limit"); ok {
		opts.SetLimit(int64(v))
	}
	if v, ok := numberData(data, "skip"); ok {
		opts.SetSkip(int64(v))
	}

	n, err := db.CountDocuments(ctx, coll, filter, opts)
	if err != nil {
		return nil, fmt.Errorf("mongo_request count_documents: %w", err)
	}
	return map[string]any{"count": n}, nil
}

func (e *WorkflowExecutor) mongoDistinct(ctx context.Context, db MongoClient, coll string, data map[string]any, input any) (any, error) {
	field, _ := data["field"].(string)
	if field == "" {
		return nil, fmt.Errorf("mongo_request distinct: field is required")
	}
	filter, err := bsonMOrEmpty(firstNonNil(data["filter"], input))
	if err != nil {
		return nil, fmt.Errorf("mongo_request distinct: filter: %w", err)
	}
	values, err := db.Distinct(ctx, coll, field, filter)
	if err != nil {
		return nil, fmt.Errorf("mongo_request distinct: %w", err)
	}
	return map[string]any{"values": values}, nil
}

func (e *WorkflowExecutor) mongoCursorFetch(ctx context.Context, data map[string]any) (any, error) {
	id, _ := data["cursor_id"].(string)
	if id == "" {
		return nil, fmt.Errorf("mongo_request cursor_fetch: cursor_id is required")
	}
	cur := e.cursorCache().take(id)
	if cur == nil {
		return nil, fmt.Errorf("mongo_request cursor_fetch: cursor %q not found or expired", id)
	}
	return e.readCursorBatch(ctx, cur, id, data)
}

// serializeCursor reads the first batch from a freshly opened cursor. If more
// docs remain, the cursor is stashed in the cache and its id returned for
// follow-up cursor_fetch calls. Otherwise the cursor is closed immediately.
func (e *WorkflowExecutor) serializeCursor(ctx context.Context, cur *mongo.Cursor, data map[string]any) (any, error) {
	id := e.cursorCache().put(cur)
	out, err := e.readCursorBatch(ctx, cur, id, data)
	if err != nil {
		// drop ensures the cursor is closed even on read errors.
		e.cursorCache().drop(id)
	}
	return out, err
}

// readCursorBatch reads up to batch_size docs from cur. When the cursor is
// exhausted, it is closed and removed from the cache; the returned cursor.id
// is empty and has_more=false.
func (e *WorkflowExecutor) readCursorBatch(ctx context.Context, cur *mongo.Cursor, id string, data map[string]any) (any, error) {
	batchSize := 100
	if v, ok := numberData(data, "batch_size"); ok && v > 0 {
		batchSize = int(v)
	}

	docs := make([]bson.M, 0, batchSize)
	for len(docs) < batchSize && cur.Next(ctx) {
		var doc bson.M
		if err := cur.Decode(&doc); err != nil {
			return nil, fmt.Errorf("decode cursor doc: %w", err)
		}
		docs = append(docs, doc)
	}
	if err := cur.Err(); err != nil {
		return nil, fmt.Errorf("cursor error: %w", err)
	}

	hasMore := cur.RemainingBatchLength() > 0 || cur.ID() != 0
	cursorOut := map[string]any{
		"has_more":   hasMore,
		"batch_size": batchSize,
		"returned":   len(docs),
	}
	if hasMore {
		cursorOut["id"] = id
	} else {
		// drain — cursor exhausted, close and remove from cache.
		e.cursorCache().drop(id)
		cursorOut["id"] = ""
	}

	return map[string]any{
		"docs":   docs,
		"cursor": cursorOut,
	}, nil
}

// runRedisRequest dispatches a Redis op against the resolved client.
// Operation is selected by data["operation"]; the rest of data carries
// op-specific fields. Output shape varies per op.
func (e *WorkflowExecutor) runRedisRequest(ctx context.Context, data map[string]any, input any) (any, error) {
	op := strings.ToLower(strings.TrimSpace(getStringData(data, "operation")))
	if op == "" {
		op = "publish" // backward-friendly default — original node was publish-only
	}

	rc, err := e.resolveRedis(ctx, data)
	if err != nil {
		return nil, err
	}

	switch op {
	case "publish":
		return e.redisPublish(ctx, rc, data, input)
	case "get":
		return e.redisGet(ctx, rc, data)
	case "set":
		return e.redisSet(ctx, rc, data, input)
	case "del":
		return e.redisDel(ctx, rc, data)
	case "incr":
		return e.redisIncr(ctx, rc, data)
	case "decr":
		return e.redisDecr(ctx, rc, data)
	case "expire":
		return e.redisExpire(ctx, rc, data)
	case "ttl":
		return e.redisTTL(ctx, rc, data)
	case "exists":
		return e.redisExists(ctx, rc, data)
	case "keys":
		return e.redisKeys(ctx, rc, data)
	case "mget":
		return e.redisMGet(ctx, rc, data)
	case "mset":
		return e.redisMSet(ctx, rc, data, input)
	case "hget":
		return e.redisHGet(ctx, rc, data)
	case "hset":
		return e.redisHSet(ctx, rc, data, input)
	case "hgetall":
		return e.redisHGetAll(ctx, rc, data)
	case "hdel":
		return e.redisHDel(ctx, rc, data)
	case "lpush":
		return e.redisLPush(ctx, rc, data, input)
	case "rpush":
		return e.redisRPush(ctx, rc, data, input)
	case "lpop":
		return e.redisLPop(ctx, rc, data)
	case "rpop":
		return e.redisRPop(ctx, rc, data)
	case "lrange":
		return e.redisLRange(ctx, rc, data)
	case "llen":
		return e.redisLLen(ctx, rc, data)
	case "sadd":
		return e.redisSAdd(ctx, rc, data, input)
	case "srem":
		return e.redisSRem(ctx, rc, data, input)
	case "smembers":
		return e.redisSMembers(ctx, rc, data)
	case "sismember":
		return e.redisSIsMember(ctx, rc, data)
	case "zadd":
		return e.redisZAdd(ctx, rc, data, input)
	case "zrem":
		return e.redisZRem(ctx, rc, data, input)
	case "zrange":
		return e.redisZRange(ctx, rc, data)
	case "zscore":
		return e.redisZScore(ctx, rc, data)
	case "zincrby":
		return e.redisZIncrBy(ctx, rc, data)
	case "xadd":
		return e.redisXAdd(ctx, rc, data, input)
	case "xrange":
		return e.redisXRange(ctx, rc, data)
	case "xlen":
		return e.redisXLen(ctx, rc, data)
	default:
		return nil, fmt.Errorf("redis_request: unknown operation %q", op)
	}
}

func (e *WorkflowExecutor) resolveRedis(ctx context.Context, data map[string]any) (RedisClient, error) {
	rc := e.Redis
	if connID, _ := data["connection_id"].(string); connID != "" && e.ConnResolver != nil {
		resolved, err := e.ConnResolver.ResolveRedis(ctx, connID)
		if err != nil {
			return nil, fmt.Errorf("redis_request: %w", err)
		}
		rc = resolved
	}
	if rc == nil {
		return nil, fmt.Errorf("redis_request: no redis client configured")
	}
	return rc, nil
}

// --- shared helpers ---

func requireString(data map[string]any, key, op string) (string, error) {
	v, _ := data[key].(string)
	if v == "" {
		return "", fmt.Errorf("redis_request %s: %s is required", op, key)
	}
	return v, nil
}

// stringSlice coerces a value to []string (accepts []string or []any of strings).
func stringSlice(v any) []string {
	switch s := v.(type) {
	case []string:
		return s
	case []any:
		out := make([]string, 0, len(s))
		for _, x := range s {
			out = append(out, fmt.Sprint(x))
		}
		return out
	case string:
		return []string{s}
	}
	return nil
}

// anySlice coerces a value to []any.
func anySlice(v any) []any {
	switch s := v.(type) {
	case []any:
		return s
	case []string:
		out := make([]any, len(s))
		for i, x := range s {
			out[i] = x
		}
		return out
	case nil:
		return nil
	default:
		return []any{v}
	}
}

// durationFromSeconds reads a numeric data field as time.Duration in seconds.
// Zero / missing returns 0 (Redis semantic: no expiry).
func durationFromSeconds(data map[string]any, key string) time.Duration {
	if v, ok := numberData(data, key); ok && v > 0 {
		return time.Duration(v) * time.Second
	}
	return 0
}

// --- op handlers ---

func (e *WorkflowExecutor) redisPublish(ctx context.Context, rc RedisClient, data map[string]any, input any) (any, error) {
	channel, err := requireString(data, "channel", "publish")
	if err != nil {
		return nil, err
	}
	var payload []byte
	if raw, ok := data["payload"].(string); ok && raw != "" {
		payload = []byte(raw)
	} else {
		b, err := json.Marshal(input)
		if err != nil {
			return nil, fmt.Errorf("redis_request publish: marshal input: %w", err)
		}
		payload = b
	}
	if err := rc.Publish(ctx, channel, payload); err != nil {
		return nil, fmt.Errorf("redis_request publish: %w", err)
	}
	return map[string]any{"channel": channel, "published": true}, nil
}

func (e *WorkflowExecutor) redisGet(ctx context.Context, rc RedisClient, data map[string]any) (any, error) {
	key, err := requireString(data, "key", "get")
	if err != nil {
		return nil, err
	}
	v, err := rc.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("redis_request get: %w", err)
	}
	return map[string]any{"value": v}, nil
}

func (e *WorkflowExecutor) redisSet(ctx context.Context, rc RedisClient, data map[string]any, input any) (any, error) {
	key, err := requireString(data, "key", "set")
	if err != nil {
		return nil, err
	}
	value, _ := data["value"].(string)
	if value == "" {
		// fall back to input — JSON-stringify if needed
		switch v := input.(type) {
		case string:
			value = v
		case nil:
			return nil, fmt.Errorf("redis_request set: value is required")
		default:
			b, err := json.Marshal(v)
			if err != nil {
				return nil, fmt.Errorf("redis_request set: marshal input: %w", err)
			}
			value = string(b)
		}
	}
	ttl := durationFromSeconds(data, "ttl_seconds")
	if err := rc.Set(ctx, key, value, ttl); err != nil {
		return nil, fmt.Errorf("redis_request set: %w", err)
	}
	return map[string]any{"ok": true}, nil
}

func (e *WorkflowExecutor) redisDel(ctx context.Context, rc RedisClient, data map[string]any) (any, error) {
	keys := stringSlice(firstNonNil(data["keys"], data["key"]))
	if len(keys) == 0 {
		return nil, fmt.Errorf("redis_request del: key/keys is required")
	}
	n, err := rc.Del(ctx, keys...)
	if err != nil {
		return nil, fmt.Errorf("redis_request del: %w", err)
	}
	return map[string]any{"deleted": n}, nil
}

func (e *WorkflowExecutor) redisIncr(ctx context.Context, rc RedisClient, data map[string]any) (any, error) {
	key, err := requireString(data, "key", "incr")
	if err != nil {
		return nil, err
	}
	v, err := rc.Incr(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("redis_request incr: %w", err)
	}
	return map[string]any{"value": v}, nil
}

func (e *WorkflowExecutor) redisDecr(ctx context.Context, rc RedisClient, data map[string]any) (any, error) {
	key, err := requireString(data, "key", "decr")
	if err != nil {
		return nil, err
	}
	v, err := rc.Decr(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("redis_request decr: %w", err)
	}
	return map[string]any{"value": v}, nil
}

func (e *WorkflowExecutor) redisExpire(ctx context.Context, rc RedisClient, data map[string]any) (any, error) {
	key, err := requireString(data, "key", "expire")
	if err != nil {
		return nil, err
	}
	ttl := durationFromSeconds(data, "ttl_seconds")
	if ttl <= 0 {
		return nil, fmt.Errorf("redis_request expire: ttl_seconds is required")
	}
	ok, err := rc.Expire(ctx, key, ttl)
	if err != nil {
		return nil, fmt.Errorf("redis_request expire: %w", err)
	}
	return map[string]any{"ok": ok}, nil
}

func (e *WorkflowExecutor) redisTTL(ctx context.Context, rc RedisClient, data map[string]any) (any, error) {
	key, err := requireString(data, "key", "ttl")
	if err != nil {
		return nil, err
	}
	d, err := rc.TTL(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("redis_request ttl: %w", err)
	}
	return map[string]any{"ttl_seconds": int64(d.Seconds())}, nil
}

func (e *WorkflowExecutor) redisExists(ctx context.Context, rc RedisClient, data map[string]any) (any, error) {
	keys := stringSlice(firstNonNil(data["keys"], data["key"]))
	if len(keys) == 0 {
		return nil, fmt.Errorf("redis_request exists: key/keys is required")
	}
	n, err := rc.Exists(ctx, keys...)
	if err != nil {
		return nil, fmt.Errorf("redis_request exists: %w", err)
	}
	return map[string]any{"count": n}, nil
}

func (e *WorkflowExecutor) redisKeys(ctx context.Context, rc RedisClient, data map[string]any) (any, error) {
	pattern, _ := data["pattern"].(string)
	if pattern == "" {
		pattern = "*"
	}
	keys, err := rc.Keys(ctx, pattern)
	if err != nil {
		return nil, fmt.Errorf("redis_request keys: %w", err)
	}
	return map[string]any{"keys": keys}, nil
}

func (e *WorkflowExecutor) redisMGet(ctx context.Context, rc RedisClient, data map[string]any) (any, error) {
	keys := stringSlice(data["keys"])
	if len(keys) == 0 {
		return nil, fmt.Errorf("redis_request mget: keys is required")
	}
	vals, err := rc.MGet(ctx, keys...)
	if err != nil {
		return nil, fmt.Errorf("redis_request mget: %w", err)
	}
	return map[string]any{"values": vals}, nil
}

func (e *WorkflowExecutor) redisMSet(ctx context.Context, rc RedisClient, data map[string]any, input any) (any, error) {
	pairs := stringMap(firstNonNil(data["pairs"], input))
	if len(pairs) == 0 {
		return nil, fmt.Errorf("redis_request mset: pairs is required")
	}
	if err := rc.MSet(ctx, pairs); err != nil {
		return nil, fmt.Errorf("redis_request mset: %w", err)
	}
	return map[string]any{"ok": true}, nil
}

func (e *WorkflowExecutor) redisHGet(ctx context.Context, rc RedisClient, data map[string]any) (any, error) {
	key, err := requireString(data, "key", "hget")
	if err != nil {
		return nil, err
	}
	field, err := requireString(data, "field", "hget")
	if err != nil {
		return nil, err
	}
	v, err := rc.HGet(ctx, key, field)
	if err != nil {
		return nil, fmt.Errorf("redis_request hget: %w", err)
	}
	return map[string]any{"value": v}, nil
}

func (e *WorkflowExecutor) redisHSet(ctx context.Context, rc RedisClient, data map[string]any, input any) (any, error) {
	key, err := requireString(data, "key", "hset")
	if err != nil {
		return nil, err
	}
	fields := stringMap(firstNonNil(data["fields"], input))
	if len(fields) == 0 {
		return nil, fmt.Errorf("redis_request hset: fields is required")
	}
	n, err := rc.HSet(ctx, key, fields)
	if err != nil {
		return nil, fmt.Errorf("redis_request hset: %w", err)
	}
	return map[string]any{"added": n}, nil
}

func (e *WorkflowExecutor) redisHGetAll(ctx context.Context, rc RedisClient, data map[string]any) (any, error) {
	key, err := requireString(data, "key", "hgetall")
	if err != nil {
		return nil, err
	}
	m, err := rc.HGetAll(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("redis_request hgetall: %w", err)
	}
	return map[string]any{"fields": m}, nil
}

func (e *WorkflowExecutor) redisHDel(ctx context.Context, rc RedisClient, data map[string]any) (any, error) {
	key, err := requireString(data, "key", "hdel")
	if err != nil {
		return nil, err
	}
	fields := stringSlice(firstNonNil(data["fields"], data["field"]))
	if len(fields) == 0 {
		return nil, fmt.Errorf("redis_request hdel: field/fields is required")
	}
	n, err := rc.HDel(ctx, key, fields...)
	if err != nil {
		return nil, fmt.Errorf("redis_request hdel: %w", err)
	}
	return map[string]any{"deleted": n}, nil
}

func (e *WorkflowExecutor) redisLPush(ctx context.Context, rc RedisClient, data map[string]any, input any) (any, error) {
	key, err := requireString(data, "key", "lpush")
	if err != nil {
		return nil, err
	}
	values := anySlice(firstNonNil(data["values"], data["value"], input))
	if len(values) == 0 {
		return nil, fmt.Errorf("redis_request lpush: value/values is required")
	}
	n, err := rc.LPush(ctx, key, values...)
	if err != nil {
		return nil, fmt.Errorf("redis_request lpush: %w", err)
	}
	return map[string]any{"length": n}, nil
}

func (e *WorkflowExecutor) redisRPush(ctx context.Context, rc RedisClient, data map[string]any, input any) (any, error) {
	key, err := requireString(data, "key", "rpush")
	if err != nil {
		return nil, err
	}
	values := anySlice(firstNonNil(data["values"], data["value"], input))
	if len(values) == 0 {
		return nil, fmt.Errorf("redis_request rpush: value/values is required")
	}
	n, err := rc.RPush(ctx, key, values...)
	if err != nil {
		return nil, fmt.Errorf("redis_request rpush: %w", err)
	}
	return map[string]any{"length": n}, nil
}

func (e *WorkflowExecutor) redisLPop(ctx context.Context, rc RedisClient, data map[string]any) (any, error) {
	key, err := requireString(data, "key", "lpop")
	if err != nil {
		return nil, err
	}
	v, err := rc.LPop(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("redis_request lpop: %w", err)
	}
	return map[string]any{"value": v}, nil
}

func (e *WorkflowExecutor) redisRPop(ctx context.Context, rc RedisClient, data map[string]any) (any, error) {
	key, err := requireString(data, "key", "rpop")
	if err != nil {
		return nil, err
	}
	v, err := rc.RPop(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("redis_request rpop: %w", err)
	}
	return map[string]any{"value": v}, nil
}

func (e *WorkflowExecutor) redisLRange(ctx context.Context, rc RedisClient, data map[string]any) (any, error) {
	key, err := requireString(data, "key", "lrange")
	if err != nil {
		return nil, err
	}
	start := int64(0)
	stop := int64(-1)
	if v, ok := numberData(data, "start"); ok {
		start = int64(v)
	}
	if v, ok := numberData(data, "stop"); ok {
		stop = int64(v)
	}
	out, err := rc.LRange(ctx, key, start, stop)
	if err != nil {
		return nil, fmt.Errorf("redis_request lrange: %w", err)
	}
	return map[string]any{"values": out}, nil
}

func (e *WorkflowExecutor) redisLLen(ctx context.Context, rc RedisClient, data map[string]any) (any, error) {
	key, err := requireString(data, "key", "llen")
	if err != nil {
		return nil, err
	}
	n, err := rc.LLen(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("redis_request llen: %w", err)
	}
	return map[string]any{"length": n}, nil
}

func (e *WorkflowExecutor) redisSAdd(ctx context.Context, rc RedisClient, data map[string]any, input any) (any, error) {
	key, err := requireString(data, "key", "sadd")
	if err != nil {
		return nil, err
	}
	members := anySlice(firstNonNil(data["members"], data["member"], input))
	if len(members) == 0 {
		return nil, fmt.Errorf("redis_request sadd: member/members is required")
	}
	n, err := rc.SAdd(ctx, key, members...)
	if err != nil {
		return nil, fmt.Errorf("redis_request sadd: %w", err)
	}
	return map[string]any{"added": n}, nil
}

func (e *WorkflowExecutor) redisSRem(ctx context.Context, rc RedisClient, data map[string]any, input any) (any, error) {
	key, err := requireString(data, "key", "srem")
	if err != nil {
		return nil, err
	}
	members := anySlice(firstNonNil(data["members"], data["member"], input))
	if len(members) == 0 {
		return nil, fmt.Errorf("redis_request srem: member/members is required")
	}
	n, err := rc.SRem(ctx, key, members...)
	if err != nil {
		return nil, fmt.Errorf("redis_request srem: %w", err)
	}
	return map[string]any{"removed": n}, nil
}

func (e *WorkflowExecutor) redisSMembers(ctx context.Context, rc RedisClient, data map[string]any) (any, error) {
	key, err := requireString(data, "key", "smembers")
	if err != nil {
		return nil, err
	}
	members, err := rc.SMembers(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("redis_request smembers: %w", err)
	}
	return map[string]any{"members": members}, nil
}

func (e *WorkflowExecutor) redisSIsMember(ctx context.Context, rc RedisClient, data map[string]any) (any, error) {
	key, err := requireString(data, "key", "sismember")
	if err != nil {
		return nil, err
	}
	member, ok := data["member"]
	if !ok {
		return nil, fmt.Errorf("redis_request sismember: member is required")
	}
	is, err := rc.SIsMember(ctx, key, member)
	if err != nil {
		return nil, fmt.Errorf("redis_request sismember: %w", err)
	}
	return map[string]any{"is_member": is}, nil
}

func (e *WorkflowExecutor) redisZAdd(ctx context.Context, rc RedisClient, data map[string]any, input any) (any, error) {
	key, err := requireString(data, "key", "zadd")
	if err != nil {
		return nil, err
	}
	src := firstNonNil(data["members"], input)
	members, err := scoreMap(src)
	if err != nil {
		return nil, fmt.Errorf("redis_request zadd: members: %w", err)
	}
	if len(members) == 0 {
		return nil, fmt.Errorf("redis_request zadd: members is required (map[member]score)")
	}
	n, err := rc.ZAdd(ctx, key, members)
	if err != nil {
		return nil, fmt.Errorf("redis_request zadd: %w", err)
	}
	return map[string]any{"added": n}, nil
}

func (e *WorkflowExecutor) redisZRem(ctx context.Context, rc RedisClient, data map[string]any, input any) (any, error) {
	key, err := requireString(data, "key", "zrem")
	if err != nil {
		return nil, err
	}
	members := anySlice(firstNonNil(data["members"], data["member"], input))
	if len(members) == 0 {
		return nil, fmt.Errorf("redis_request zrem: member/members is required")
	}
	n, err := rc.ZRem(ctx, key, members...)
	if err != nil {
		return nil, fmt.Errorf("redis_request zrem: %w", err)
	}
	return map[string]any{"removed": n}, nil
}

func (e *WorkflowExecutor) redisZRange(ctx context.Context, rc RedisClient, data map[string]any) (any, error) {
	key, err := requireString(data, "key", "zrange")
	if err != nil {
		return nil, err
	}
	start := int64(0)
	stop := int64(-1)
	if v, ok := numberData(data, "start"); ok {
		start = int64(v)
	}
	if v, ok := numberData(data, "stop"); ok {
		stop = int64(v)
	}
	out, err := rc.ZRange(ctx, key, start, stop)
	if err != nil {
		return nil, fmt.Errorf("redis_request zrange: %w", err)
	}
	return map[string]any{"members": out}, nil
}

func (e *WorkflowExecutor) redisZScore(ctx context.Context, rc RedisClient, data map[string]any) (any, error) {
	key, err := requireString(data, "key", "zscore")
	if err != nil {
		return nil, err
	}
	member, err := requireString(data, "member", "zscore")
	if err != nil {
		return nil, err
	}
	v, err := rc.ZScore(ctx, key, member)
	if err != nil {
		return nil, fmt.Errorf("redis_request zscore: %w", err)
	}
	return map[string]any{"score": v}, nil
}

func (e *WorkflowExecutor) redisZIncrBy(ctx context.Context, rc RedisClient, data map[string]any) (any, error) {
	key, err := requireString(data, "key", "zincrby")
	if err != nil {
		return nil, err
	}
	member, err := requireString(data, "member", "zincrby")
	if err != nil {
		return nil, err
	}
	inc, ok := numberData(data, "increment")
	if !ok {
		return nil, fmt.Errorf("redis_request zincrby: increment is required")
	}
	v, err := rc.ZIncrBy(ctx, key, inc, member)
	if err != nil {
		return nil, fmt.Errorf("redis_request zincrby: %w", err)
	}
	return map[string]any{"score": v}, nil
}

func (e *WorkflowExecutor) redisXAdd(ctx context.Context, rc RedisClient, data map[string]any, input any) (any, error) {
	stream, err := requireString(data, "stream", "xadd")
	if err != nil {
		return nil, err
	}
	src := firstNonNil(data["values"], input)
	values, err := anyMap(src)
	if err != nil {
		return nil, fmt.Errorf("redis_request xadd: values: %w", err)
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("redis_request xadd: values is required (map[field]value)")
	}
	id, err := rc.XAdd(ctx, stream, values)
	if err != nil {
		return nil, fmt.Errorf("redis_request xadd: %w", err)
	}
	return map[string]any{"id": id}, nil
}

func (e *WorkflowExecutor) redisXRange(ctx context.Context, rc RedisClient, data map[string]any) (any, error) {
	stream, err := requireString(data, "stream", "xrange")
	if err != nil {
		return nil, err
	}
	start, _ := data["start"].(string)
	if start == "" {
		start = "-"
	}
	stop, _ := data["stop"].(string)
	if stop == "" {
		stop = "+"
	}
	msgs, err := rc.XRange(ctx, stream, start, stop)
	if err != nil {
		return nil, fmt.Errorf("redis_request xrange: %w", err)
	}
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, map[string]any{"id": m.ID, "values": m.Values})
	}
	return map[string]any{"messages": out}, nil
}

func (e *WorkflowExecutor) redisXLen(ctx context.Context, rc RedisClient, data map[string]any) (any, error) {
	stream, err := requireString(data, "stream", "xlen")
	if err != nil {
		return nil, err
	}
	n, err := rc.XLen(ctx, stream)
	if err != nil {
		return nil, fmt.Errorf("redis_request xlen: %w", err)
	}
	return map[string]any{"length": n}, nil
}

// scoreMap coerces a value to map[member]score for ZAdd.
func scoreMap(v any) (map[string]float64, error) {
	if v == nil {
		return nil, nil
	}
	if m, ok := v.(map[string]float64); ok {
		return m, nil
	}
	if m, ok := mapAny(v); ok {
		out := make(map[string]float64, len(m))
		for k, val := range m {
			f, ok := toFloat64(val)
			if !ok {
				return nil, fmt.Errorf("score for %q is not numeric (%T)", k, val)
			}
			out[k] = f
		}
		return out, nil
	}
	return nil, fmt.Errorf("expected map[member]score, got %T", v)
}

// anyMap coerces a value to map[string]any for XAdd values.
func anyMap(v any) (map[string]any, error) {
	if v == nil {
		return nil, nil
	}
	if m, ok := mapAny(v); ok {
		return m, nil
	}
	if m, ok := v.(map[string]string); ok {
		out := make(map[string]any, len(m))
		for k, val := range m {
			out[k] = val
		}
		return out, nil
	}
	return nil, fmt.Errorf("expected object/map, got %T", v)
}

func toFloat64(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

// runNotify logs input and returns a message.
func runNotify(data map[string]any, input any) (any, error) {
	msg, _ := data["message"].(string)
	if msg == "" {
		msg = fmt.Sprint(input)
	}
	slog.Info("workflow notify", "message", msg, "input", input)
	return map[string]any{"message": msg}, nil
}

// applyTemplate replaces template placeholders in s.
//
// Supported patterns:
//
//	{{input.FIELD}}                   — field from immediate input
//	{{context.stepName.input.FIELD}}  — field from named step's input
//	{{context.stepName.output.FIELD}} — field from named step's output
//	{{context.stepName.item.FIELD}}   — current iteration element (for_each body only)
func applyTemplate(s string, input any, wfCtx runCtx) string {
	if m, ok := mapAny(input); ok {
		for k, v := range m {
			s = strings.ReplaceAll(s, "{{input."+k+"}}", fmt.Sprint(v))
		}
	}
	for name, sc := range wfCtx {
		if m, ok := mapAny(sc.Input); ok {
			for k, v := range m {
				s = strings.ReplaceAll(s, "{{context."+name+".input."+k+"}}", fmt.Sprint(v))
			}
		}
		if m, ok := mapAny(sc.Output); ok {
			for k, v := range m {
				s = strings.ReplaceAll(s, "{{context."+name+".output."+k+"}}", fmt.Sprint(v))
			}
		}
		if m, ok := mapAny(sc.Item); ok {
			for k, v := range m {
				s = strings.ReplaceAll(s, "{{context."+name+".item."+k+"}}", fmt.Sprint(v))
			}
		}
	}
	return s
}

// resolveTemplateValue resolves a string that is EXACTLY a single
// `{{…}}` token to the raw underlying value (slice, map, number — not
// stringified). applyTemplate is for embedding scalars into a larger
// string; this is for selectors that must preserve type, currently the
// for_each `items` field where `{{input.docs}}` has to stay a slice.
//
// Supported paths (mirrors applyTemplate's surface):
//
//	{{input.<key>}}
//	{{context.<name>.input.<key>}}
//	{{context.<name>.output.<key>}}
//	{{context.<name>.item.<key>}}
//
// Returns ok=false when s is not a lone token or the path doesn't
// resolve, so the caller keeps its existing fallback behaviour.
func resolveTemplateValue(s string, input any, wfCtx runCtx) (any, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{{") || !strings.HasSuffix(s, "}}") {
		return nil, false
	}
	expr := strings.TrimSpace(s[2 : len(s)-2])
	if strings.Contains(expr, "{{") {
		return nil, false // not a lone token
	}
	parts := strings.Split(expr, ".")

	get := func(container any, key string) (any, bool) {
		m, ok := mapAny(container)
		if !ok {
			return nil, false
		}
		v, ok := m[key]
		return v, ok
	}

	switch {
	case len(parts) == 2 && parts[0] == "input":
		return get(input, parts[1])
	case len(parts) == 4 && parts[0] == "context":
		sc, ok := wfCtx[parts[1]]
		if !ok {
			return nil, false
		}
		switch parts[2] {
		case "input":
			return get(sc.Input, parts[3])
		case "output":
			return get(sc.Output, parts[3])
		case "item":
			return get(sc.Item, parts[3])
		}
	}
	return nil, false
}

// runSandboxScript executes user code in an isolated Docker container.
//
// Node data fields:
//
//	data.language   string  "javascript" | "python"
//	data.script     string  user code
//	data.timeout    float64 seconds (default 30)
//	data.mem_limit  float64 MB (default 128)
//	data.cpu_limit  float64 cores (default 0.5)
//	data.network    bool    allow outbound network (default false)
func (e *WorkflowExecutor) runSandboxScript(ctx context.Context, data map[string]any, input any, wfCtx runCtx, params map[string]string) (any, error) {
	if e.SandboxRT == nil {
		return nil, fmt.Errorf("sandbox_script: sandbox manager not configured")
	}

	lang, _ := data["language"].(string)
	if lang == "" {
		lang = "javascript"
	}
	script, _ := data["script"].(string)
	if script == "" {
		return input, nil // pass-through
	}

	timeout := 30 * time.Second
	if t, ok := data["timeout"].(float64); ok && t > 0 {
		timeout = time.Duration(t) * time.Second
	}
	memLimit := int64(sandbox.DefaultMemLimit)
	if m, ok := data["mem_limit"].(float64); ok && m > 0 {
		memLimit = int64(m) * 1024 * 1024
	}
	cpuLimit := sandbox.DefaultCPULimit
	if c, ok := data["cpu_limit"].(float64); ok && c > 0 {
		cpuLimit = c
	}
	network, _ := data["network"].(bool)
	customImage, _ := data["custom_image"].(string)

	var packages []string
	if pkgStr, ok := data["packages"].(string); ok && pkgStr != "" {
		for _, p := range strings.Split(pkgStr, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				packages = append(packages, p)
			}
		}
	}

	result, err := e.SandboxRT.Run(ctx, sandbox.RunRequest{
		Language: sandbox.Language(lang),
		Code:     script,
		Input:    input,
		Context:  wfCtxToJS(wfCtx),
		Params:   params,
		Timeout:  timeout,
		MemLimit: memLimit,
		CPULimit: cpuLimit,
		Network:  network,
		Image:    customImage,
		Packages: packages,
	})
	if err != nil {
		return nil, fmt.Errorf("sandbox_script: %w", err)
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("sandbox_script: exit code %d: %s", result.ExitCode, result.Stderr)
	}
	return result.Output, nil
}

// toSlice normalises input to []any. Single values become a one-element slice.
func toSlice(input any) []any {
	switch v := input.(type) {
	case []any:
		return v
	case nil:
		return nil
	default:
		return []any{v}
	}
}

// startOfUTCDay returns midnight UTC for the given time.
func startOfUTCDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// CostExceededError is the typed error returned when a workflow.CostLimits
// cap blocks a run (pre-start daily cap) or aborts one mid-run (per-run or
// daily cap during agent loop). The Axis field tells the UI which cap
// fired so it can label the error appropriately ("daily cap" vs "run cap").
type CostExceededError struct {
	Axis    string  // "run" or "daily"
	CapUSD  float64 // configured cap
	SpentUSD float64 // observed spend at breach time
}

func (e *CostExceededError) Error() string {
	return fmt.Sprintf("cost_exceeded: %s cap $%.4f reached (spent $%.4f)", e.Axis, e.CapUSD, e.SpentUSD)
}

// IsCostExceeded reports whether err (or anything it wraps) is a
// *CostExceededError. UI / handlers use this to render a typed badge
// rather than the raw error string.
func IsCostExceeded(err error) bool {
	var ce *CostExceededError
	return errors.As(err, &ce)
}

// aggregateUsage walks every llm_call event across every agent trace and
// sums input/output tokens + cost into one WorkflowRun.Usage. Same source
// the live AgentCostBadge reads, so the persisted total matches the UI.
// CostUSD stays full-precision float64 — UI decides rounding.
func aggregateUsage(traces map[string][]TraceEvent) UsageTotal {
	var total UsageTotal
	for _, evs := range traces {
		for _, ev := range evs {
			if ev.Type != "llm_call" || ev.Usage == nil {
				continue
			}
			total.Add(ev.Usage.InputTokens, ev.Usage.OutputTokens, ev.Usage.CostUSD)
		}
	}
	return total
}

