// SMTP sender — wraps net/smtp for transactional outbound mail.
//
// Why stdlib net/smtp instead of github.com/wneessen/go-mail or
// jordan-wright/email: net/smtp covers the 95% case (PLAIN auth +
// STARTTLS, a single recipient, plain-text body) without adding a
// dep. If we need MIME multipart / attachments / DKIM later, swap
// the SMTPSender impl behind the Sender interface — callers don't
// know the wire format.
//
// Failure mode: SMTP returns errors to the caller; password_reset
// + tenant invite handlers swallow them at warn-level so dispatch
// failures don't block user-facing 200 responses (the email handlers
// run dispatchEmail in a goroutine for exactly this reason).

package email

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/config"
)

// SMTPSender is a Sender backed by an SMTP relay.
type SMTPSender struct {
	host     string
	port     int
	user     string
	password string
	from     string
	starttls bool
}

// NewSMTPSender returns a sender configured for the given relay.
// Returns an error when required fields are missing — callers can
// fall back to LogSender on misconfiguration.
func NewSMTPSender(cfg config.EmailConfig) (*SMTPSender, error) {
	if cfg.SMTPHost == "" {
		return nil, errors.New("smtp: SMTP_HOST required")
	}
	if cfg.From == "" {
		return nil, errors.New("smtp: EMAIL_FROM required")
	}
	if cfg.SMTPPort == 0 {
		cfg.SMTPPort = 587
	}
	return &SMTPSender{
		host:     cfg.SMTPHost,
		port:     cfg.SMTPPort,
		user:     cfg.SMTPUser,
		password: cfg.SMTPPassword,
		from:     cfg.From,
		starttls: cfg.SMTPStartTLS,
	}, nil
}

// Send dials the SMTP server, optionally upgrades to TLS, authenticates
// (when user is set), and pushes the message. Honors ctx via the dial
// timeout; data-phase reads are bounded by the underlying SetDeadline.
func (s *SMTPSender) Send(ctx context.Context, msg Message) error {
	if msg.To == "" {
		return errors.New("smtp: To required")
	}
	addr := net.JoinHostPort(s.host, strconv.Itoa(s.port))

	// Use a Dialer so ctx can shorten the dial — net/smtp doesn't take
	// a ctx, so this is the only ctx-aware step. After dial, we set a
	// hard read/write deadline as a safety net.
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Second)
	}
	dialer.Deadline = deadline

	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("smtp dial %s: %w", addr, err)
	}
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return err
	}

	c, err := smtp.NewClient(conn, s.host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp new client: %w", err)
	}
	defer func() { _ = c.Close() }()

	if s.starttls {
		// Per RFC 3207, STARTTLS upgrades the existing connection.
		// Skip when host already speaks implicit TLS (port 465); for
		// the standard 587 path STARTTLS is the path. We always
		// VerifyHostname against ServerName to prevent MITM.
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(&tls.Config{ServerName: s.host}); err != nil {
				return fmt.Errorf("smtp starttls: %w", err)
			}
		}
	}

	if s.user != "" {
		auth := smtp.PlainAuth("", s.user, s.password, s.host)
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}

	if err := c.Mail(s.from); err != nil {
		return fmt.Errorf("smtp mail from: %w", err)
	}
	if err := c.Rcpt(msg.To); err != nil {
		return fmt.Errorf("smtp rcpt: %w", err)
	}

	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	// Minimal RFC 5322 headers — From / To / Subject / Date / blank
	// line / body. No MIME (plain text only). If we add HTML later,
	// switch to multipart/alternative here.
	headers := fmt.Sprintf(
		"From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n",
		s.from, msg.To, msg.Subject, time.Now().UTC().Format(time.RFC1123Z),
	)
	if _, err := fmt.Fprintf(w, "%s%s", headers, msg.Body); err != nil {
		_ = w.Close()
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp data close: %w", err)
	}
	return c.Quit()
}

// NewFromConfig picks the right Sender impl based on EmailConfig.
// "log" → LogSender (dev default). "smtp" → SMTPSender, falling back
// to LogSender + warn if SMTP misconfigured (rather than crashing
// the API at boot — operators can fix env then restart).
func NewFromConfig(cfg config.EmailConfig) Sender {
	switch cfg.Provider {
	case "smtp":
		s, err := NewSMTPSender(cfg)
		if err != nil {
			// Defer the warn to the caller — this package shouldn't
			// log directly. Return LogSender so the app still boots.
			return &fallbackSender{primary: nil, fallback: NewLogSender(), reason: err.Error()}
		}
		return s
	case "log", "":
		return NewLogSender()
	default:
		return &fallbackSender{primary: nil, fallback: NewLogSender(), reason: "unknown provider " + cfg.Provider}
	}
}

// fallbackSender wraps a misconfigured/unknown provider — Send routes
// to the fallback and stamps the reason once via the LogSender path
// so operators see *why* email is logged-only.
type fallbackSender struct {
	primary  Sender
	fallback Sender
	reason   string
}

func (f *fallbackSender) Send(ctx context.Context, msg Message) error {
	// Add the reason to the body so it's visible alongside the URL —
	// makes "wait, why didn't this go to actual email?" instantly
	// answerable in dev logs.
	msg.Body = "[" + f.reason + "] " + msg.Body
	return f.fallback.Send(ctx, msg)
}
