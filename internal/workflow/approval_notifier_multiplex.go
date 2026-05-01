// Multiplex approval notifier — routes a single ApprovalRequest to the
// transport configured on the workflow's `ApprovalChannel`. Owns the
// channel-type switch so gate code stays transport-agnostic; adding a
// new channel kind = adding one branch here + one impl.

package workflow

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/bRRRITSCOLD/burrow/internal/email"
)

// MultiplexApprovalNotifier reads `req.Workflow.ApprovalChannel.Type`
// and forwards to the matching backing notifier. Channel=none, nil, or
// an unknown type all degrade to no-op + warn so a misconfigured
// workflow doesn't break the gate.
//
// Email + Slack senders are constructed at boot from the same config
// (SMTP credentials live in `email.Sender`; Slack uses a plain
// http.Client). All are nil-safe — the multiplexer logs + returns nil
// when its dependency for the requested transport isn't wired.
type MultiplexApprovalNotifier struct {
	Email        email.Sender
	HTTPClient   *http.Client         // for slack_webhook + slack_bot POSTs; nil falls back to http.DefaultClient.
	ConnResolver *ConnectionResolver  // resolves slack_bot's Target connection_id → bot_token. nil disables slack_bot.
	// SlackAPIBase overrides the Slack API base URL (default
	// "https://slack.com/api"). Tests point this at an httptest server;
	// production deployments should leave it empty.
	SlackAPIBase string
}

// NotifyApprovalRequested dispatches according to the workflow's
// configured channel. Errors are returned so the caller can log them
// at the call site; the gate code intentionally does not abort on
// dispatch failure.
func (n *MultiplexApprovalNotifier) NotifyApprovalRequested(ctx context.Context, req ApprovalRequest) error {
	ch := req.Workflow.ApprovalChannel
	if ch == nil || ch.Type == "" || ch.Type == "none" {
		return nil
	}
	switch ch.Type {
	case "smtp":
		return n.sendSMTP(ctx, req, ch)
	case "slack_webhook":
		return n.sendSlack(ctx, req, ch)
	case "slack_bot":
		return n.sendSlackBot(ctx, req, ch)
	default:
		slog.Warn("approval notifier: unknown channel type — no dispatch",
			"workflow_id", req.Workflow.ID, "type", ch.Type)
		return nil
	}
}

func (n *MultiplexApprovalNotifier) sendSMTP(ctx context.Context, req ApprovalRequest, ch *ApprovalChannel) error {
	if n.Email == nil {
		slog.Warn("approval notifier: smtp channel configured but no email.Sender wired",
			"workflow_id", req.Workflow.ID, "target", ch.Target)
		return nil
	}
	return n.Email.Send(ctx, email.Message{
		To:      ch.Target,
		Subject: req.HumanSubject(),
		Body:    smtpApprovalBody(req),
	})
}

func (n *MultiplexApprovalNotifier) sendSlack(ctx context.Context, req ApprovalRequest, ch *ApprovalChannel) error {
	if n.HTTPClient == nil {
		n.HTTPClient = http.DefaultClient
	}
	if ch.Target == "" {
		return fmt.Errorf("slack_webhook channel: empty target")
	}
	return postSlackWebhook(ctx, n.HTTPClient, ch.Target, req)
}

// sendSlackBot resolves the configured `slack`-typed Connection,
// pulls the encrypted bot_token + optional default_channel out of its
// config, and calls chat.postMessage. Falls back to a no-op + warn
// when the resolver isn't wired (dev / eval) so a misconfigured deploy
// doesn't strand a workflow.
func (n *MultiplexApprovalNotifier) sendSlackBot(ctx context.Context, req ApprovalRequest, ch *ApprovalChannel) error {
	if n.ConnResolver == nil {
		slog.Warn("approval notifier: slack_bot channel configured but no ConnResolver wired",
			"workflow_id", req.Workflow.ID)
		return nil
	}
	if ch.Target == "" {
		return fmt.Errorf("slack_bot channel: empty target (must be a slack-connection_id)")
	}
	conn, err := n.ConnResolver.ResolveConnection(ctx, ch.Target)
	if err != nil {
		return fmt.Errorf("slack_bot: resolve connection %s: %w", ch.Target, err)
	}
	if conn.Type != ConnectionTypeSlack {
		return fmt.Errorf("slack_bot: connection %s has type %q (expected %q)",
			ch.Target, conn.Type, ConnectionTypeSlack)
	}
	botToken := conn.Config["bot_token"]
	channel := ch.Channel
	if channel == "" {
		channel = conn.Config["default_channel"]
	}
	if n.HTTPClient == nil {
		n.HTTPClient = http.DefaultClient
	}
	apiBase := n.SlackAPIBase
	if apiBase == "" {
		apiBase = "https://slack.com/api"
	}
	return postSlackBotMessage(ctx, n.HTTPClient, apiBase, botToken, channel, req)
}

// smtpApprovalBody renders the email body. Plain-text only — the
// `email.Message` shape doesn't carry HTML today, and a clear
// monospace-friendly layout reads fine in every client. Lines stay
// short enough to survive aggressive auto-wrapping.
func smtpApprovalBody(req ApprovalRequest) string {
	link := req.MagicLinkURL()
	wfName := req.Workflow.Name
	if wfName == "" {
		wfName = req.Workflow.ID
	}
	var what string
	switch req.Pending.Kind {
	case "tool_call":
		what = fmt.Sprintf("Tool call: %s (iter %d)", req.Pending.ToolName, req.Pending.Iter)
	case "node":
		label := req.Pending.NodeName
		if label == "" {
			label = string(req.Pending.NodeType) + " " + req.Pending.NodeID
		}
		what = "Node: " + label
	default:
		what = "Pending action"
	}
	return fmt.Sprintf(`A workflow run is paused waiting for your approval.

Workflow: %s
%s
Requested at: %s
Run ID: %s

Open this link to approve or reject:
%s

This link expires in 24 hours and can only be used once.
`, wfName, what, req.Pending.RequestedAt.Format("2006-01-02 15:04:05 MST"), req.RunID, link)
}
