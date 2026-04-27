package sandbox

import "time"

// Language identifies a supported sandbox runtime.
type Language string

const (
	LangJavaScript Language = "javascript"
	LangPython     Language = "python"
	LangGolang     Language = "golang"
	LangRust       Language = "rust"
	LangPHP        Language = "php"
)

// RunRequest describes a sandbox execution.
type RunRequest struct {
	Language Language          `json:"language"`
	Code     string            `json:"code"`
	Input    any               `json:"input"`
	Context  map[string]any    `json:"context,omitempty"`
	Params   map[string]string `json:"params,omitempty"`
	Timeout  time.Duration     `json:"timeout"`
	MemLimit int64             `json:"mem_limit"` // bytes
	CPULimit float64           `json:"cpu_limit"` // cores (e.g. 0.5)
	Network  bool              `json:"network"`
	Image    string            `json:"image,omitempty"`    // custom Docker image; overrides ImageForLanguage when set
	Packages []string          `json:"packages,omitempty"` // extra packages to install (auto-builds image)
}

// RunResult holds the outcome of a sandbox execution.
type RunResult struct {
	Output   any           `json:"output"`
	Stdout   string        `json:"stdout"`
	Stderr   string        `json:"stderr"`
	ExitCode int           `json:"exit_code"`
	Duration time.Duration `json:"duration"`
}

// OutputEvent streams stdout/stderr lines from a running sandbox.
// Stream values: "stdout", "stderr", "exit".
type OutputEvent struct {
	Stream   string `json:"stream"`
	Data     string `json:"data,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	Duration string `json:"duration,omitempty"`
	Error    string `json:"error,omitempty"`
}

// DefaultTimeout is the fallback execution timeout.
const DefaultTimeout = 30 * time.Second

// DefaultMemLimit is the fallback memory limit (128 MB).
const DefaultMemLimit = 128 * 1024 * 1024

// DefaultCPULimit is the fallback CPU limit (0.5 cores).
const DefaultCPULimit = 0.5

// ImageForLanguage returns the Docker image tag for a language runtime.
func ImageForLanguage(lang Language) string {
	switch lang {
	case LangJavaScript:
		return "immaiwin/sandbox-node:20"
	case LangPython:
		return "immaiwin/sandbox-python:3.12"
	case LangGolang:
		return "immaiwin/sandbox-go:1.22"
	case LangRust:
		return "immaiwin/sandbox-rust:1.86"
	case LangPHP:
		return "immaiwin/sandbox-php:8.3"
	default:
		return ""
	}
}

// DebugImageForLanguage returns the debug-enabled Docker image for a language.
// Only JS and Python support interactive debugging.
func DebugImageForLanguage(lang Language) string {
	switch lang {
	case LangJavaScript:
		return "immaiwin/sandbox-node-debug:20"
	case LangPython:
		return "immaiwin/sandbox-python-debug:3.12"
	default:
		return ""
	}
}

// DebugPortForLanguage returns the container-internal debug port for a language.
func DebugPortForLanguage(lang Language) int {
	switch lang {
	case LangPython:
		return 5678 // debugpy
	case LangJavaScript:
		return 9229 // node --inspect
	default:
		return 0
	}
}
