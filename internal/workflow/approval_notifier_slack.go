// Slack incoming-webhook approval sender. Slack incoming-webhooks
// don't support interactive callbacks (those need a registered Slack
// app + an HTTP receiver), so we POST a plain message body that
// includes the magic-link as a clickable URL. Slack auto-renders the
// link — recipient clicks → lands on /approve → submits decision.

package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// slackPayload mirrors the Slack incoming-webhook spec. We only set
// `text` — that's enough to surface a clickable link, a header line,
// and a small context block. Block-kit attachments add visual polish
// but constrain message length and aren't worth the complexity at
// this layer.
type slackPayload struct {
	Text string `json:"text"`
}

func postSlackWebhook(ctx context.Context, client *http.Client, webhookURL string, req ApprovalRequest) error {
	body, err := json.Marshal(slackPayload{Text: slackApprovalBody(req)})
	if err != nil {
		return fmt.Errorf("slack_webhook: marshal: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("slack_webhook: new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("slack_webhook: post: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("slack_webhook: %d %s", resp.StatusCode, string(raw))
	}
	return nil
}

func slackApprovalBody(req ApprovalRequest) string {
	link := req.MagicLinkURL()
	// Workflow name shows up via req.HumanSubject(); no separate
	// var needed here.
	var detail string
	switch req.Pending.Kind {
	case "tool_call":
		detail = fmt.Sprintf("Tool call: `%s` (iter %d)", req.Pending.ToolName, req.Pending.Iter)
	case "node":
		label := req.Pending.NodeName
		if label == "" {
			label = string(req.Pending.NodeType) + " " + req.Pending.NodeID
		}
		detail = fmt.Sprintf("Node: `%s`", label)
	default:
		detail = "Pending action"
	}
	return fmt.Sprintf(":bell: *%s*\n%s\nRun ID: `%s`\n<%s|Open approval link>",
		req.HumanSubject(), detail, req.RunID, link)
}
