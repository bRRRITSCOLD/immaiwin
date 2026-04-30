// Package email provides a minimal pluggable email-sending interface.
//
// Today only LogSender is wired — it just logs the message body so a
// dev or operator can copy/paste a password-reset link from stdout.
// SMTP / SendGrid / SES senders can ship later behind the same Sender
// interface without touching callers.
//
// The interface is intentionally tiny: one Send method, no template
// engine. Templating belongs in the caller (handler) so the email
// package stays a transport.

package email

import (
	"context"
	"log/slog"
)

// Sender ships a single email. Implementations must be safe for
// concurrent use.
type Sender interface {
	Send(ctx context.Context, msg Message) error
}

// Message is the minimum field set for a transactional email.
type Message struct {
	To      string
	Subject string
	Body    string // plain text; rich HTML can land later via a separate field
}

// LogSender writes the message to slog.Info. Non-prod default —
// flips on for any environment where SMTP isn't configured. Logs at
// info-level so operators can grep recent reset links from the API
// log when running locally.
type LogSender struct{}

// NewLogSender returns the default no-op sender.
func NewLogSender() *LogSender { return &LogSender{} }

func (s *LogSender) Send(_ context.Context, msg Message) error {
	slog.Info("email (log-only)", "to", msg.To, "subject", msg.Subject, "body", msg.Body)
	return nil
}
