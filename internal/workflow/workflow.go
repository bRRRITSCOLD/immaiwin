package workflow

import "time"

// NodeType identifies the role of a node in a workflow.
type NodeType string

const (
	NodeTypeTrigger      NodeType = "trigger"
	NodeTypeHTTPFetch    NodeType = "http_fetch"
	NodeTypeJSTransform  NodeType = "js_transform"
	NodeTypeForEach      NodeType = "for_each"
	NodeTypeMongoUpsert  NodeType = "mongo_upsert"
	NodeTypeRedisPublish NodeType = "redis_publish"
	NodeTypeNotify       NodeType = "notify"
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
//   - http_fetch:    {"url": "https://...", "name": "fetchArticle"}
//   - js_transform:  {"script": "return input.items.map(...)"}
//   - mongo_upsert:  {"collection": "news_articles", "filter_field": "url"}
//   - redis_publish: {"channel": "immaiwin:news:articles"}
//   - notify:        {"message": "optional template"}
type Node struct {
	ID       string         `bson:"id"       json:"id"`
	Type     NodeType       `bson:"type"     json:"type"`
	Position Position       `bson:"position" json:"position"`
	Data     map[string]any `bson:"data"     json:"data"`
}

// Edge connects two nodes.
// SourceHandle "success" or "error" controls which branch is followed.
type Edge struct {
	ID           string `bson:"id"                       json:"id"`
	Source       string `bson:"source"                   json:"source"`
	Target       string `bson:"target"                   json:"target"`
	SourceHandle string `bson:"source_handle,omitempty"  json:"sourceHandle,omitempty"`
	TargetHandle string `bson:"target_handle,omitempty"  json:"targetHandle,omitempty"`
	Label        string `bson:"label,omitempty"          json:"label,omitempty"`
}

// Workflow is a named node-edge graph that describes a pipeline.
// ID is a client-supplied string (e.g. UUID) to support idempotent PUT.
//
// Params holds workflow-level key-value constants accessible in node fields and JS transforms:
//   - In any string data field: {{params.key}}
//   - In JS transform scripts:  params.key  (available as a global)
type Workflow struct {
	ID        string            `bson:"_id,omitempty" json:"id"`
	Name      string            `bson:"name"          json:"name"`
	Params    map[string]string `bson:"params"        json:"params"`
	Nodes     []Node            `bson:"nodes"         json:"nodes"`
	Edges     []Edge            `bson:"edges"         json:"edges"`
	CreatedAt time.Time         `bson:"created_at"    json:"created_at"`
	UpdatedAt time.Time         `bson:"updated_at"    json:"updated_at"`
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
