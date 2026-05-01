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
// http.Client). Both are nil-safe — the multiplexer logs + returns nil
// when its dependency for the requested transport isn't wired.
type MultiplexApprovalNotifier struct {
	Email      email.Sender
	HTTPClient *http.Client // for slack_webhook POSTs; nil falls back to http.DefaultClient.
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
