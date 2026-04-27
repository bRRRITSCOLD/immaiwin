package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Sandbox entrypoint for Go execution.
//
// Protocol:
//
//	stdin  → JSON { code, input, context, params }
//	stdout → JSON output (from output() call)
//	stderr → logs / errors
//
// User code runs inside main(). Available:
//
//	input (any), context (map[string]any), params (map[string]string)
//	output(val any) — call to produce result
//	fmt.Println → stderr (stdout reserved for JSON output)

type Payload struct {
	Code    string            `json:"code"`
	Input   json.RawMessage   `json:"input"`
	Context json.RawMessage   `json:"context"`
	Params  map[string]string `json:"params"`
}

func main() {
	var p Payload
	if err := json.NewDecoder(os.Stdin).Decode(&p); err != nil {
		fmt.Fprintf(os.Stderr, "entrypoint: invalid JSON payload: %v\n", err)
		os.Exit(1)
	}

	inputJSON := string(p.Input)
	if inputJSON == "" {
		inputJSON = "null"
	}
	contextJSON := string(p.Context)
	if contextJSON == "" {
		contextJSON = "{}"
	}
	paramsBytes, _ := json.Marshal(p.Params)
	if p.Params == nil {
		paramsBytes = []byte("{}")
	}

	tmpDir, err := os.MkdirTemp("", "sandbox-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "entrypoint: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmpDir)

	// Write data files so user code reads them (avoids string escaping issues)
	inputPath := filepath.Join(tmpDir, "input.json")
	contextPath := filepath.Join(tmpDir, "context.json")
	paramsPath := filepath.Join(tmpDir, "params.json")

	_ = os.WriteFile(inputPath, []byte(inputJSON), 0644)
	_ = os.WriteFile(contextPath, []byte(contextJSON), 0644)
	_ = os.WriteFile(paramsPath, paramsBytes, 0644)

	// Write go.mod for the user program
	goMod := "module sandbox\ngo 1.22\n"
	_ = os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(goMod), 0644)

	// Generate user source
	src := `package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

var _realStdout = os.Stdout

func init() {
	os.Stdout = os.Stderr
}

func output(val any) {
	b, err := json.Marshal(val)
	if err != nil {
		fmt.Fprintf(os.Stderr, "output serialization error: %v\n", err)
		_realStdout.Write([]byte("null"))
	} else {
		_realStdout.Write(b)
	}
	os.Exit(0)
}

// Suppress unused import errors
var (
	_ = math.Pi
	_ = sort.Ints
	_ = strconv.Itoa
	_ = strings.Contains
	_ = time.Now
)

func main() {
	var input any
	inputBytes, _ := os.ReadFile(` + quote(inputPath) + `)
	json.Unmarshal(inputBytes, &input)

	context := map[string]any{}
	contextBytes, _ := os.ReadFile(` + quote(contextPath) + `)
	json.Unmarshal(contextBytes, &context)

	params := map[string]string{}
	paramsBytes, _ := os.ReadFile(` + quote(paramsPath) + `)
	json.Unmarshal(paramsBytes, &params)

	_ = input
	_ = context
	_ = params

` + p.Code + `

	_realStdout.Write([]byte("null"))
}
`

	srcPath := filepath.Join(tmpDir, "main.go")
	if err := os.WriteFile(srcPath, []byte(src), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "entrypoint: write source: %v\n", err)
		os.Exit(1)
	}

	cmd := exec.Command("go", "run", ".")
	cmd.Dir = tmpDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		os.Exit(1)
	}
}

// quote returns a Go double-quoted string literal.
func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
