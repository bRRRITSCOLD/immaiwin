package dap

// DAP (Debug Adapter Protocol) message types.
// See: https://microsoft.github.io/debug-adapter-protocol/specification

// Message is the base DAP message envelope.
type Message struct {
	Seq  int    `json:"seq"`
	Type string `json:"type"` // "request", "response", "event"
}

// Request is a DAP request from client to adapter.
type Request struct {
	Message
	Command   string `json:"command"`
	Arguments any    `json:"arguments,omitempty"`
}

// Response is a DAP response from adapter to client.
type Response struct {
	Message
	RequestSeq int    `json:"request_seq"`
	Success    bool   `json:"success"`
	Command    string `json:"command"`
	Message_   string `json:"message,omitempty"` // error message when !Success
	Body       any    `json:"body,omitempty"`
}

// Event is a DAP event from adapter to client.
type Event struct {
	Message
	Event string `json:"event"`
	Body  any    `json:"body,omitempty"`
}

// --- Request arguments ---

type InitializeRequestArgs struct {
	ClientID                     string `json:"clientID,omitempty"`
	ClientName                   string `json:"clientName,omitempty"`
	AdapterID                    string `json:"adapterID"`
	LinesStartAt1               bool   `json:"linesStartAt1"`
	ColumnsStartAt1              bool   `json:"columnsStartAt1"`
	PathFormat                   string `json:"pathFormat,omitempty"`
	SupportsVariableType         bool   `json:"supportsVariableType,omitempty"`
	SupportsRunInTerminalRequest bool   `json:"supportsRunInTerminalRequest,omitempty"`
}

type LaunchRequestArgs struct {
	Program     string `json:"program"`
	StopOnEntry bool   `json:"stopOnEntry,omitempty"`
	NoDebug     bool   `json:"noDebug,omitempty"`
	// debugpy-specific
	JustMyCode bool `json:"justMyCode,omitempty"`
}

type AttachRequestArgs struct {
	JustMyCode     bool `json:"justMyCode,omitempty"`
	RedirectOutput bool `json:"redirectOutput,omitempty"`
}

type SetBreakpointsArgs struct {
	Source      Source             `json:"source"`
	Breakpoints []SourceBreakpoint `json:"breakpoints"`
}

type SourceBreakpoint struct {
	Line      int    `json:"line"`
	Condition string `json:"condition,omitempty"`
}

type Source struct {
	Name string `json:"name,omitempty"`
	Path string `json:"path"`
}

type ContinueArgs struct {
	ThreadId int `json:"threadId"`
}

type NextArgs struct {
	ThreadId int `json:"threadId"`
}

type StepInArgs struct {
	ThreadId int `json:"threadId"`
}

type StepOutArgs struct {
	ThreadId int `json:"threadId"`
}

type ScopesArgs struct {
	FrameId int `json:"frameId"`
}

type VariablesArgs struct {
	VariablesReference int `json:"variablesReference"`
}

type StackTraceArgs struct {
	ThreadId   int `json:"threadId"`
	StartFrame int `json:"startFrame,omitempty"`
	Levels     int `json:"levels,omitempty"`
}

type EvaluateArgs struct {
	Expression string `json:"expression"`
	FrameId    int    `json:"frameId,omitempty"`
	Context    string `json:"context,omitempty"` // "repl", "watch", "hover"
}

type DisconnectArgs struct {
	TerminateDebuggee bool `json:"terminateDebuggee,omitempty"`
}

type ConfigurationDoneArgs struct{}

type ThreadsArgs struct{}

// --- Response bodies ---

type InitializeResponseBody struct {
	SupportsConfigurationDoneRequest bool `json:"supportsConfigurationDoneRequest,omitempty"`
	SupportsFunctionBreakpoints      bool `json:"supportsFunctionBreakpoints,omitempty"`
	SupportsConditionalBreakpoints   bool `json:"supportsConditionalBreakpoints,omitempty"`
	SupportsEvaluateForHovers        bool `json:"supportsEvaluateForHovers,omitempty"`
}

type SetBreakpointsResponseBody struct {
	Breakpoints []BreakpointResult `json:"breakpoints"`
}

type BreakpointResult struct {
	Id       int    `json:"id,omitempty"`
	Verified bool   `json:"verified"`
	Line     int    `json:"line,omitempty"`
	Message  string `json:"message,omitempty"`
}

type StackTraceResponseBody struct {
	StackFrames []StackFrame `json:"stackFrames"`
	TotalFrames int          `json:"totalFrames,omitempty"`
}

type StackFrame struct {
	Id     int    `json:"id"`
	Name   string `json:"name"`
	Source *Source `json:"source,omitempty"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
}

type ScopesResponseBody struct {
	Scopes []Scope `json:"scopes"`
}

type Scope struct {
	Name               string `json:"name"`
	VariablesReference int    `json:"variablesReference"`
	Expensive          bool   `json:"expensive,omitempty"`
}

type VariablesResponseBody struct {
	Variables []Variable `json:"variables"`
}

type Variable struct {
	Name               string `json:"name"`
	Value              string `json:"value"`
	Type               string `json:"type,omitempty"`
	VariablesReference int    `json:"variablesReference,omitempty"`
	ObjectId           string `json:"objectId,omitempty"` // CDP object reference for expanding
}

type EvaluateResponseBody struct {
	Result             string `json:"result"`
	Type               string `json:"type,omitempty"`
	VariablesReference int    `json:"variablesReference,omitempty"`
}

type ThreadsResponseBody struct {
	Threads []Thread `json:"threads"`
}

type Thread struct {
	Id   int    `json:"id"`
	Name string `json:"name"`
}

// --- Event bodies ---

type StoppedEventBody struct {
	Reason   string `json:"reason"` // "breakpoint", "step", "pause", "exception"
	ThreadId int    `json:"threadId"`
	Text     string `json:"text,omitempty"`
}

type OutputEventBody struct {
	Category string `json:"category,omitempty"` // "console", "stdout", "stderr"
	Output   string `json:"output"`
}

type TerminatedEventBody struct {
	Restart bool `json:"restart,omitempty"`
}

// DebugEvent is a unified internal event type used by both DAP and CDP clients
// to communicate debug events back to the handler.
type DebugEvent struct {
	Type        string       `json:"type"`      // "stopped", "output", "terminated", "continued"
	Reason      string       `json:"reason,omitempty"`
	Line        int          `json:"line,omitempty"`
	ThreadId    int          `json:"threadId,omitempty"`
	Variables   []Variable   `json:"variables,omitempty"`
	CallStack   []StackFrame `json:"callStack,omitempty"`
	Stream      string       `json:"stream,omitempty"`
	Data        string       `json:"data,omitempty"`
	CallFrameId string       `json:"-"` // CDP-internal, not serialized to browser
}
