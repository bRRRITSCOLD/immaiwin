package workflow

import (
	"encoding/json"
	"time"
)

// NodeType identifies the role of a node in a workflow.
type NodeType string

const (
	NodeTypeTrigger       NodeType = "trigger"
	NodeTypeHTTPRequest   NodeType = "http_request"
	NodeTypeForEach       NodeType = "for_each"
	NodeTypeMongoRequest  NodeType = "mongo_request"
	NodeTypeRedisRequest  NodeType = "redis_request"
	NodeTypeNotify        NodeType = "notify"
	NodeTypeSandboxScript NodeType = "sandbox_script"
	NodeTypeAIAgent       NodeType = "ai_agent"
	// NodeTypeSubWorkflow is an agent tool target whose handler
	// dispatches a sub-workflow run (identified by data.workflow_id)
	// and returns its final output as the tool result. The node is
	// only reachable through an agent's `tool` edge — BFS never
	// visits it directly, same as other as_tool targets.
	NodeTypeSubWorkflow NodeType = "sub_workflow"
	// NodeTypeReturn declares the workflow's return value when run
	// as a sub_workflow tool (or surfaced from a manual run).
	// `data.payload` is a JSON-shaped value with template
	// substitution against `context` / `config` / `input`; the
	// resolved value becomes the sub_workflow tool result. Workflows
	// without a return node return null — fire-and-forget pattern.
	NodeTypeReturn NodeType = "return"
)

// Edge source-handle values used by the executor.
const (
	EdgeHandleSuccess = "success" // default outgoing edge after a node succeeds
	EdgeHandleError   = "error"   // alternative path on node failure
	EdgeHandleItem    = "item"    // for_each body trigger
	EdgeHandleTool    = "tool"    // AI Agent → tool node binding (NEW for ai_agent)
)

// Position holds the canvas (x, y) coordinates for a node.
type Position struct {
	X float64 `bson:"x" json:"x"`
	Y float64 `bson:"y" json:"y"`
}

// Node is a single step in a workflow graph.
//
// All nodes support an optional "name" field in data:
//
//	data.name: step name — accessible via context.stepName in JS transforms and URL templates:
//	  context.stepName.input.field  — what the named step received
//	  context.stepName.output.field — what the named step produced
//	  context.stepName.item.field   — current iteration element (for_each body only)
//
// Node data fields by type:
//   - http_request:  {"url": "https://...", "method": "GET", "headers": {...}, "query": {...}, "body": "...", "body_json": {...}, "body_form": {...}, "timeout_seconds": 30, "follow_redirects": true, "max_redirects": 10, "basic_auth_username": "...", "basic_auth_password": "...", "bearer_token": "...", "user_agent": "...", "tls_insecure_skip_verify": false, "parse_json": false, "max_response_bytes": 10485760, "accept_any_status": false, "name": "fetchArticle"}
//   - mongo_request: {"collection": "<name>", "operation": "find|find_one_and_update|find_one_and_replace|insert_one|insert_many|update_many|delete_one|delete_many|aggregate|count_documents|distinct|cursor_fetch", ...op-specific fields (filter, update, projection, sort, limit, batch_size, pipeline, etc.)}
//   - redis_request: {"operation": "publish|get|set|del|incr|decr|expire|ttl|exists|keys|mget|mset|hget|hset|hgetall|hdel|lpush|rpush|lpop|rpop|lrange|llen|sadd|srem|smembers|sismember|zadd|zrem|zrange|zscore|zincrby|xadd|xrange|xlen", ...op-specific fields (key, value, channel, payload, members, etc.)}
//   - notify:        {"message": "optional template"}
type Node struct {
	ID       string         `bson:"id"       json:"id"`
	Type     NodeType       `bson:"type"     json:"type"`
	Position Position       `bson:"position" json:"position"`
	Data     map[string]any `bson:"data"     json:"data"`
	Width    *float64       `bson:"width,omitempty"  json:"width,omitempty"`
	Height   *float64       `bson:"height,omitempty" json:"height,omitempty"`
}

// Edge connects two nodes.
// SourceHandle "success" or "error" controls which branch is followed.
type Edge struct {
	ID           string         `bson:"id"                       json:"id"`
	Source       string         `bson:"source"                   json:"source"`
	Target       string         `bson:"target"                   json:"target"`
	SourceHandle string         `bson:"source_handle,omitempty"  json:"sourceHandle,omitempty"`
	TargetHandle string         `bson:"target_handle,omitempty"  json:"targetHandle,omitempty"`
	Label        string         `bson:"label,omitempty"          json:"label,omitempty"`
	Data         map[string]any `bson:"data,omitempty"           json:"data,omitempty"`
}

// Workflow is a named node-edge graph that describes a pipeline.
// ID is a client-supplied string (e.g. UUID) to support idempotent PUT.
//
// Config holds workflow-level key-value constants accessible via
// `{{config.key}}` interpolation in any string data field on a node.
type Workflow struct {
	ID        string            `bson:"_id,omitempty" json:"id"`
	TenantID  string            `bson:"tenant_id"     json:"tenant_id"` // multi-tenant scoping
	Name      string            `bson:"name"          json:"name"`
	// Config is the workflow's persistent configuration map —
	// constants the workflow author bakes in (API base URLs,
	// channel names, default formatting strings). Per-run dynamic
	// input goes through Workflow.InputSchema + the operator's Run
	// dialog, not here. Renamed from `Config` everywhere — existing
	// Mongo docs need to be re-saved (pre-launch, so acceptable).
	Config map[string]string `bson:"config"        json:"config"`
	Nodes     []Node            `bson:"nodes"         json:"nodes"`
	Edges     []Edge            `bson:"edges"         json:"edges"`
	// CostLimits enforces per-workflow USD caps on agent LLM spend.
	// Zero (or omitted) on either field means "no cap on that axis."
	// Caps are checked BEFORE a run starts (daily) and AFTER every
	// llm_call inside the agent loop (per-run + daily). Breaches stop
	// the run with status=error and error="cost_exceeded: <axis>".
	CostLimits *CostLimits `bson:"cost_limits,omitempty" json:"cost_limits,omitempty"`
	// ConfigSchema is the optional typed declaration for Config. When
	// set, the UI renders typed inputs (select for enum, NumberField,
	// Switch, etc.) and the API validates Config on save. Empty falls
	// back to the legacy untyped key/value editor — fully back-compat.
	ConfigSchema []SchemaEntry `bson:"config_schema,omitempty" json:"config_schema,omitempty"`
	// InputSchema is the optional typed declaration for the
	// workflow's RUN INPUT — distinct from Config (which are
	// persistent across runs). Same `SchemaEntry` shape as
	// ConfigSchema so the editor + form-renderer can be shared.
	// When set, sub_workflow tool nodes auto-derive their JSON
	// Schema from this field instead of forcing each consumer
	// to hand-write a schema; the canvas Run dialog renders
	// typed inputs; the engine can validate input at dispatch
	// (future phase). Empty = back-compat free-form input.
	InputSchema []SchemaEntry `bson:"input_schema,omitempty" json:"input_schema,omitempty"`
	// InputSchemaJSON is the optional raw-JSON-Schema escape hatch
	// for nested / array / advanced input contracts that SchemaEntry's
	// flat shape can't express. Stored as a string (Mongo doesn't
	// have a native JSON type and we want to round-trip the
	// authored form). When set, it WINS over InputSchema for engine
	// validation and sub_workflow auto-derive; InputSchema still
	// drives the typed Run dialog when present, so workflows can
	// expose a friendly form OR a raw JSON box (or both — the form
	// covers happy-path entry, raw is the contract for consumers).
	InputSchemaJSON string `bson:"input_schema_json,omitempty" json:"input_schema_json,omitempty"`
	// OutputSchema declares the typed return contract for the
	// workflow. Validated when the workflow finishes; sub_workflow
	// consumers surface this in agent tool descriptions so the LLM
	// knows what it'll get back. Same shape priority as InputSchema:
	// OutputSchemaJSON wins, OutputSchema (typed flat) is the
	// simpler editor surface. Empty = no contract enforcement.
	OutputSchema     []SchemaEntry `bson:"output_schema,omitempty"      json:"output_schema,omitempty"`
	OutputSchemaJSON string       `bson:"output_schema_json,omitempty" json:"output_schema_json,omitempty"`
	// ApprovalChannel routes pending_approval events out-of-band when
	// the gate fires (Stage 2 of OOB approvals). Nil = no channel; the
	// run still pauses and is resolvable from /runs/:id, but no email
	// or webhook is sent. The dispatcher (PR-b) reads this field on
	// gate fire and posts the magic-link to the configured target.
	ApprovalChannel *ApprovalChannel `bson:"approval_channel,omitempty" json:"approval_channel,omitempty"`
	// Version increments on every server-side Upsert via Mongo `$inc`.
	// Server-controlled — clients do not set it. Value is 1 after the
	// first save; "Save as new" / Duplicate resets it to 1 on the new
	// doc. Foundation for optimistic concurrency / git-style diff later.
	Version   int       `bson:"version,omitempty" json:"version,omitempty"`
	CreatedAt time.Time `bson:"created_at"    json:"created_at"`
	UpdatedAt time.Time `bson:"updated_at"    json:"updated_at"`

	// Enabled gates trigger-driven runs. Disabled workflows stay
	// fully editable + manually runnable (canvas Run button), but
	// every trigger worker (cron / RabbitMQ / Redis-subscribe /
	// future websocket) skips them at sync-tick time. Default = true
	// (UnmarshalJSON below normalises an absent field to true so an
	// older API client + freshly-decoded Mongo docs missing the
	// field don't pause every workflow on upgrade). A boot-time
	// migration backfills existing rows so List/Get also defaults
	// cleanly without per-call gymnastics.
	Enabled        bool       `bson:"enabled"                  json:"enabled"`
	DisabledAt     *time.Time `bson:"disabled_at,omitempty"    json:"disabled_at,omitempty"`
	DisabledReason string     `bson:"disabled_reason,omitempty" json:"disabled_reason,omitempty"`
}

// UnmarshalJSON normalises an absent `enabled` field to true. The
// alternative — setting Enabled=false on missing — would pause every
// workflow saved by an older client that doesn't include the field.
// Mongo-loaded docs without the field are handled by the one-time
// boot-time BackfillEnabled migration; this handler covers the API
// surface (UpsertWorkflow body parsing).
func (w *Workflow) UnmarshalJSON(data []byte) error {
	type alias Workflow
	aux := &struct {
		Enabled *bool `json:"enabled"`
		*alias
	}{alias: (*alias)(w)}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	if aux.Enabled == nil {
		w.Enabled = true
	} else {
		w.Enabled = *aux.Enabled
	}
	return nil
}

// SchemaEntry is the typed declaration for one workflow Config key.
// Mirrors the skill manifest's `config[]` shape so author UX stays
// consistent across skill-author and workflow-author surfaces; kept as
// its own type to avoid cross-package import for a frozen subset.
type SchemaEntry struct {
	Name        string   `bson:"name"                  json:"name"`
	Type        string   `bson:"type"                  json:"type"` // "string" | "number" | "boolean" | "enum"
	Description string   `bson:"description,omitempty" json:"description,omitempty"`
	Default     string   `bson:"default,omitempty"     json:"default,omitempty"`
	Required    bool     `bson:"required,omitempty"    json:"required,omitempty"`
	Enum        []string `bson:"enum,omitempty"        json:"enum,omitempty"`
}

// ApprovalChannel describes where the OOB-approval dispatcher should
// post the magic-link prompt when this workflow's `require_node_approval`
// or `require_approval` gates fire. Per-workflow rather than per-node
// or per-tenant — workflow author owns the routing decision. PR-a stores
// the field; PR-b adds the dispatcher.
type ApprovalChannel struct {
	// Type selects the transport. Supported: "smtp" (templated
	// email), "slack_webhook" (Incoming Webhook URL POST), "slack_bot"
	// (chat.postMessage via a `slack`-typed Connection). "none" stores
	// intent-to-disable without removing the field; the dispatcher
	// treats it as a no-op.
	Type string `bson:"type"           json:"type"`
	// Target is the destination keyed by Type:
	//   smtp           → recipient email address
	//   slack_webhook  → incoming-webhook URL
	//   slack_bot      → connection_id (refs a `slack`-typed Connection
	//                    whose Config.bot_token holds the xoxb-* token)
	//   none           → empty
	Target string `bson:"target,omitempty" json:"target,omitempty"`
	// From is the optional sender override for the "smtp" transport;
	// empty falls back to the SMTP-config-level default `From:` header.
	From string `bson:"from,omitempty"   json:"from,omitempty"`
	// Channel is the destination Slack channel ID or DM user ID
	// (e.g. "C01234ABCDE" or "U01234ABCDE"). Required for "slack_bot"
	// when the referenced connection has no `default_channel`. Falls
	// back to that connection's default when empty. Ignored by other
	// transports.
	Channel string `bson:"channel,omitempty" json:"channel,omitempty"`
}

// CostLimits is the per-workflow cap config.
type CostLimits struct {
	// MaxRunUSD aborts a single run once cumulative LLM cost exceeds
	// this dollar amount. 0 = unlimited.
	MaxRunUSD float64 `bson:"max_run_usd"   json:"max_run_usd"`
	// MaxDailyUSD aborts (and blocks new starts of) any run for this
	// workflow once today's UTC-day cost-sum (across all runs of this
	// workflow) exceeds this dollar amount. 0 = unlimited.
	MaxDailyUSD float64 `bson:"max_daily_usd" json:"max_daily_usd"`
}

// OrderedNodes returns all nodes reachable from trigger nodes in BFS order.
func (w *Workflow) OrderedNodes() []Node {
	adj := make(map[string][]string, len(w.Edges))
	for _, e := range w.Edges {
		adj[e.Source] = append(adj[e.Source], e.Target)
	}
	byID := make(map[string]Node, len(w.Nodes))
	for _, n := range w.Nodes {
		byID[n.ID] = n
	}

	var queue []string
	for _, n := range w.Nodes {
		if n.Type == NodeTypeTrigger {
			queue = append(queue, n.ID)
		}
	}

	visited := make(map[string]bool)
	var ordered []Node

	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if visited[id] {
			continue
		}
		visited[id] = true

		if n, ok := byID[id]; ok {
			ordered = append(ordered, n)
		}
		for _, next := range adj[id] {
			if !visited[next] {
				queue = append(queue, next)
			}
		}
	}
	return ordered
}
