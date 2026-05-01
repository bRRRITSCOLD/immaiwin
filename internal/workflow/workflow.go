package workflow

import "time"

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
// Params holds workflow-level key-value constants accessible in node fields and JS transforms:
//   - In any string data field: {{params.key}}
//   - In JS transform scripts:  params.key  (available as a global)
type Workflow struct {
	ID        string            `bson:"_id,omitempty" json:"id"`
	TenantID  string            `bson:"tenant_id"     json:"tenant_id"` // multi-tenant scoping
	Name      string            `bson:"name"          json:"name"`
	Params    map[string]string `bson:"params"        json:"params"`
	Nodes     []Node            `bson:"nodes"         json:"nodes"`
	Edges     []Edge            `bson:"edges"         json:"edges"`
	// CostLimits enforces per-workflow USD caps on agent LLM spend.
	// Zero (or omitted) on either field means "no cap on that axis."
	// Caps are checked BEFORE a run starts (daily) and AFTER every
	// llm_call inside the agent loop (per-run + daily). Breaches stop
	// the run with status=error and error="cost_exceeded: <axis>".
	CostLimits *CostLimits `bson:"cost_limits,omitempty" json:"cost_limits,omitempty"`
	// ParamsSchema is the optional typed declaration for Params. When
	// set, the UI renders typed inputs (select for enum, NumberField,
	// Switch, etc.) and the API validates Params on save. Empty falls
	// back to the legacy untyped key/value editor — fully back-compat.
	ParamsSchema []ParamEntry `bson:"params_schema,omitempty" json:"params_schema,omitempty"`
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
}

// ParamEntry is the typed declaration for one workflow Params key.
// Mirrors the skill manifest's `config[]` shape so author UX stays
// consistent across skill-author and workflow-author surfaces; kept as
// its own type to avoid cross-package import for a frozen subset.
type ParamEntry struct {
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
