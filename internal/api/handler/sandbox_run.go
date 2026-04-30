package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/bRRRITSCOLD/burrow/internal/sandbox"
	"github.com/gin-gonic/gin"
)

// runWsMessage is the envelope for the streaming run WebSocket protocol.
type runWsMessage struct {
	Type     string         `json:"type"`
	Language string         `json:"language,omitempty"`
	Code     string         `json:"code,omitempty"`
	Input    any            `json:"input,omitempty"`
	Context  map[string]any `json:"context,omitempty"`
	Image    string         `json:"image,omitempty"`    // custom Docker image override
	Packages string         `json:"packages,omitempty"` // comma-separated package names for auto-build
	Network  bool           `json:"network,omitempty"`  // allow outbound egress
	// Response fields
	Stream   string `json:"stream,omitempty"`
	Data     string `json:"data,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Duration string `json:"duration,omitempty"`
	Error    string `json:"error,omitempty"`
}

// RunSandbox returns a gin handler that upgrades to WebSocket for streaming
// sandbox execution. Protocol:
//
//	→ {"type":"run","language":"python","code":"...","input":{},"context":{}}
//	← {"type":"output","stream":"stdout","data":"hello\n"}
//	← {"type":"output","stream":"stderr","data":"warning\n"}
//	← {"type":"done","exit_code":0,"duration":"1.2s"}
//	← {"type":"error","error":"timeout"}
func RunSandbox(mgr sandbox.Runtime) gin.HandlerFunc {
	return func(c *gin.Context) {
		if mgr == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "sandbox not enabled"})
			return
		}

		ws, err := debugUpgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			slog.Error("sandbox-run: ws upgrade failed", "err", err)
			return
		}
		defer func() { _ = ws.Close() }()

		// Wait for "run" message
		_, raw, err := ws.ReadMessage()
		if err != nil {
			return
		}

		var msg runWsMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			writeRunWS(ws, runWsMessage{Type: "error", Error: "invalid JSON"})
			return
		}

		if msg.Type != "run" {
			writeRunWS(ws, runWsMessage{Type: "error", Error: "first message must be 'run'"})
			return
		}

		ctx := c.Request.Context()

		req := sandbox.RunRequest{
			Language: sandbox.Language(msg.Language),
			Code:     msg.Code,
			Input:    msg.Input,
			Context:  msg.Context,
			Image:    msg.Image,
			Packages: parsePackages(msg.Packages),
			Network:  msg.Network,
		}

		events, err := mgr.StreamRun(ctx, req)
		if err != nil {
			writeRunWS(ws, runWsMessage{Type: "error", Error: err.Error()})
			return
		}

		// Relay OutputEvents → browser
		for event := range events {
			switch event.Stream {
			case "stdout", "stderr":
				writeRunWS(ws, runWsMessage{
					Type:   "output",
					Stream: event.Stream,
					Data:   event.Data,
				})
			case "exit":
				if event.Error != "" {
					writeRunWS(ws, runWsMessage{Type: "error", Error: event.Error})
				} else {
					writeRunWS(ws, runWsMessage{
						Type:     "done",
						ExitCode: &event.ExitCode,
						Duration: event.Duration,
					})
				}
			}
		}
	}
}

func writeRunWS(ws interface{ WriteMessage(int, []byte) error }, msg runWsMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	wsMu.Lock()
	defer wsMu.Unlock()
	_ = ws.WriteMessage(1, data) // 1 = TextMessage
}

// parsePackages splits a comma-separated string into trimmed, non-empty package names.
func parsePackages(s string) []string {
	if s == "" {
		return nil
	}
	var pkgs []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			pkgs = append(pkgs, p)
		}
	}
	return pkgs
}
