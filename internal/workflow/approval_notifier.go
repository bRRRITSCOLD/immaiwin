// Out-of-band approval notifier — fires when a workflow gate
// (`require_node_approval` or per-tool `require_approval`) lands a run
// in `pending_approval` state. Each implementation ships the configured
// `ApprovalChannel` (SMTP / Slack webhook) with a magic-link the
// recipient clicks to redeem the gate.
//
// Notifiers are best-effort: a delivery failure is logged at warn but
// does NOT abort the gate. The /runs/:id Approve/Reject path remains a
// fallback — the dispatcher exists to deliver a more proactive prompt,
// not to be the system of record.
//
// PR-b ships SMTP + Slack-webhook implementations. The interface is
// intentionally narrow so a SES / SendGrid / generic-webhook adapter
// can land later without touching gate code.

package workflow

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// ApprovalNotifier dispatches a magic-link approval prompt out-of-band
// when a gate fires. Implementations must be safe for concurrent use;
// the gate fires the call from a goroutine so the executor doesn't
// block on slow channels.
type ApprovalNotifier interface {
	NotifyApprovalRequested(ctx context.Context, req ApprovalRequest) error
}

// ApprovalRequest is the bundle the notifier needs to produce a
// recipient-friendly message + a working magic-link. Workflow is
// embedded by value so the notifier can read `ApprovalChannel` (which
// transport / target to use) without re-querying Mongo.
type ApprovalRequest struct {
	RunID    string
	Token    string                  // signed JWT — already issued by SignApprovalToken.
	Workflow Workflow
	Pending  PendingApprovalState
	// UIBaseURL is the externally-reachable base URL for the magic
	// link (e.g. "https://burrow.example.com"). The notifier builds
	// `<UIBaseURL>/approve?run_id=<>&token=<>` from it. Empty falls
	// back to a relative `/approve?...` link, useful in dev where the
	// UI proxies the API.
	UIBaseURL string
}

// MagicLinkURL builds the full approve-page URL for this request. Used
// by both SMTP body templates and Slack message bodies — keep one
// formatter so the URL shape stays consistent across channels (the
// /approve route's `validateSearch` reads exactly these two keys).
func (r ApprovalRequest) MagicLinkURL() string {
	v := url.Values{}
	v.Set("run_id", r.RunID)
	v.Set("token", r.Token)
	base := strings.TrimRight(r.UIBaseURL, "/")
	return base + "/approve?" + v.Encode()
}

// HumanSubject is a short, informative one-line summary of WHAT needs
// approving. Used as the SMTP subject + the Slack message header.
// Format reads naturally for both channels:
//
//	Approval needed: <workflow name> · tool call get_weather
//	Approval needed: <workflow name> · node notify-1
func (r ApprovalRequest) HumanSubject() string {
	wfName := r.Workflow.Name
	if wfName == "" {
		wfName = r.Workflow.ID
	}
	switch r.Pending.Kind {
	case "tool_call":
		return fmt.Sprintf("Approval needed: %s · tool call %s", wfName, r.Pending.ToolName)
	case "node":
		label := r.Pending.NodeName
		if label == "" {
			label = string(r.Pending.NodeType) + " " + r.Pending.NodeID
		}
		return fmt.Sprintf("Approval needed: %s · node %s", wfName, label)
	default:
		return fmt.Sprintf("Approval needed: %s", wfName)
	}
}

// NoopApprovalNotifier is the safe-by-default notifier. Used when no
// dispatcher is wired (dev runs without SMTP, eval suites, etc.) so
// gates don't spew dispatch errors when no transport is configured.
type NoopApprovalNotifier struct{}

// NotifyApprovalRequested ignores the request. Returns nil.
func (NoopApprovalNotifier) NotifyApprovalRequested(_ context.Context, _ ApprovalRequest) error {
	return nil
}
