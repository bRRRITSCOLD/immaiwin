// Slack bot-token approval sender — posts via `chat.postMessage` API
// using a workspace bot token (xoxb-*) resolved from a `slack`-typed
// Connection. Stronger story than the Incoming-Webhook path:
//   - Token sits in encrypted Connection config (AES-256-GCM via
//     ENCRYPTION_KEY), not plaintext on the workflow doc.
//   - Token rotates / revokes from Slack workspace admin — no need
//     to recreate burrow config when a key rolls.
//   - Bot can post to any channel it's been added to + DMs (with
//     `im:write` scope) — the webhook path is single-channel-pinned.
//   - Bearer header beats secret-in-URL for log hygiene.
//
// Future iterations (see .private/ai-automation/SLACK-AUTH-ROADMAP.md):
//   - Block Kit interactive buttons in the message body so the user
//     resolves the gate without leaving Slack.
//   - Per-tenant OAuth-installed token in `tenant_settings` for
//     multi-tenant SaaS deployments.

package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// chatPostMessageReq mirrors the subset of the Slack chat.postMessage
// API we currently send. `text` is required; channel can be a public
// channel ID, private channel ID, or a user ID for DMs (Slack treats
// `chat.postMessage` to a user ID as opening + posting in a DM).
type chatPostMessageReq struct {
	Channel string `json:"channel"`
	Text    string `json:"text"`
}

// chatPostMessageResp captures the Slack response shape for failure
// detection. We don't store ts / channel — those are only useful if
// we wanted to thread follow-ups, which we don't here.
type chatPostMessageResp struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// postSlackBotMessage sends one chat.postMessage call using `botToken`
// (Bearer xoxb-*). Returns an error when the HTTP call fails OR Slack
// returns `ok=false` — the latter typically means a missing scope, a
// channel the bot isn't a member of, or a revoked token.
func postSlackBotMessage(ctx context.Context, client *http.Client, apiBase, botToken, channel string, req ApprovalRequest) error {
	if botToken == "" {
		return fmt.Errorf("slack_bot: empty bot_token on referenced connection")
	}
	if channel == "" {
		return fmt.Errorf("slack_bot: no channel — set ApprovalChannel.Channel or connection's default_channel")
	}
	if apiBase == "" {
		apiBase = "https://slack.com/api"
	}

	body, err := json.Marshal(chatPostMessageReq{
		Channel: channel,
		Text:    slackApprovalBody(req),
	})
	if err != nil {
		return fmt.Errorf("slack_bot: marshal: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		apiBase+"/chat.postMessage", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("slack_bot: new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json; charset=utf-8")
	httpReq.Header.Set("Authorization", "Bearer "+botToken)

	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("slack_bot: post: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	// Slack returns 200 even for application-level errors; the `ok`
	// boolean is the source of truth. Bound the body read so a
	// runaway response can't OOM us.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("slack_bot: %d %s", resp.StatusCode, string(raw))
	}
	var parsed chatPostMessageResp
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("slack_bot: decode response: %w", err)
	}
	if !parsed.OK {
		// Common errors: `not_in_channel`, `channel_not_found`,
		// `invalid_auth`, `missing_scope`. Forward the slack error
		// string so operators can fix the config without reading slog.
		return fmt.Errorf("slack_bot: chat.postMessage rejected: %s", parsed.Error)
	}
	return nil
}
