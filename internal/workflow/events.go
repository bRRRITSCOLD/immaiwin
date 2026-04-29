package workflow

import (
	"sync"
	"time"
)

// EventType identifies a workflow run event. The set is intentionally small —
// each one corresponds to one logical UI affordance:
//
//	step_start         a node about to execute (BFS visit)
//	step_done          a node finished (success or error captured in payload)
//	agent_iter         start of one ReAct iteration inside an ai_agent node
//	agent_llm          the agent issued an LLM call (text + usage attached)
//	agent_tool_call    the agent invoked a tool
//	agent_tool_result  the tool returned (observation + is_error attached)
//	agent_final        the agent emitted its final message
//	run_done           the workflow run finished (success or error)
//
// New types should map to new UI events; reuse existing ones whenever
// possible so the React side doesn't grow a switch-statement per change.
type EventType string

const (
	EventStepStart       EventType = "step_start"
	EventStepDone        EventType = "step_done"
	EventStepPending     EventType = "step_pending" // pre-exec breakpoint pause
	EventCostExceeded    EventType = "cost_exceeded" // workflow CostLimits cap breached
	EventAgentIter       EventType = "agent_iter"
	EventAgentLLM        EventType = "agent_llm"
	EventAgentToolCall   EventType = "agent_tool_call"
	EventAgentToolResult EventType = "agent_tool_result"
	EventAgentFinal      EventType = "agent_final"
	EventRunDone         EventType = "run_done"
)

// RunEvent is the wire shape pushed onto the event channel. Same struct
// goes to WS clients (JSON-encoded), the agent run repo (best-effort
// persistence), and any future transport (gRPC streaming, SSE, etc.).
//
// Fields are intentionally union-like — only those relevant to a given
// EventType are populated. Keep this stable; UI clients pick fields by
// type.
type RunEvent struct {
	Type       EventType `json:"type"`
	At         time.Time `json:"at"`
	NodeID     string    `json:"node_id,omitempty"`
	NodeType   NodeType  `json:"node_type,omitempty"`
	Iter       int       `json:"iter,omitempty"`
	Text       string    `json:"text,omitempty"`
	ToolName   string    `json:"tool_name,omitempty"`
	ToolID     string    `json:"tool_id,omitempty"`
	ToolArgs   any       `json:"tool_args,omitempty"`
	Result     string    `json:"result,omitempty"`
	IsError    bool      `json:"is_error,omitempty"`
	Output     any       `json:"output,omitempty"`
	Error      string    `json:"error,omitempty"`
	Usage      *Usage    `json:"usage,omitempty"`
	// Populated on the terminal "run_done" event so clients can flip Run ↔
	// Continue based on whether the run paused mid-way.
	RunID  string `json:"run_id,omitempty"`
	Status string `json:"status,omitempty"`
}

// Usage mirrors the per-call LLM accounting on the wire. We use a separate
// type so RunEvent doesn't depend on llm package shapes (which leaks
// vendor-specific fields).
type Usage struct {
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	TotalTokens  int     `json:"total_tokens"`
	CostUSD      float64 `json:"cost_usd"`
}

// EventEmitter is the abstraction the executor + agent loop hand events to.
// Implementations live outside this package: the WS handler wraps a
// `*websocket.Conn`; tests use a slice-backed recorder; future SSE or gRPC
// transports plug in here too. Keeping this on an interface (not a
// channel) lets emitters apply backpressure / filtering / fan-out without
// the executor caring.
//
// Emit MUST be safe to call concurrently — the agent loop fans out tool
// calls in goroutines (see runEnv.toolSteps for precedent).
type EventEmitter interface {
	Emit(event RunEvent)
}

// noopEmitter is the default when callers don't supply one. Skips work
// without nil-checking on every call site.
type noopEmitter struct{}

// Emit implements EventEmitter (no-op).
func (noopEmitter) Emit(RunEvent) {}

// fanoutEmitter forwards to multiple downstream emitters in registration
// order. Used internally so the trace path can simultaneously persist via
// RunRepo and stream via the WS handler.
type fanoutEmitter struct {
	mu       sync.RWMutex
	children []EventEmitter
}

func newFanoutEmitter(children ...EventEmitter) *fanoutEmitter {
	out := &fanoutEmitter{children: make([]EventEmitter, 0, len(children))}
	for _, c := range children {
		if c != nil {
			out.children = append(out.children, c)
		}
	}
	return out
}

// Emit fans out to every registered child. A slow child cannot block
// others — implementations are expected to do their own buffering.
func (f *fanoutEmitter) Emit(ev RunEvent) {
	f.mu.RLock()
	children := f.children
	f.mu.RUnlock()
	for _, c := range children {
		c.Emit(ev)
	}
}

// stamp ensures ev.At is populated; called by every emit site so the UI
// always has a sortable timestamp without each callsite remembering.
func stampNow(ev RunEvent) RunEvent {
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	return ev
}
