package api

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// Mailer delivers a transactional e-mail. It is the seam between the
// status-page subscriber flows and real SMTP delivery: production wires an SMTP
// implementation, tests use a capturing fake, and DEV_MODE can use a logger.
// Keeping it minimal (no templating, no queueing) means the budget-sensitive
// delivery path can be swapped without touching the handlers.
type Mailer interface {
	Send(ctx context.Context, to, subject, textBody, htmlBody string) error
}

// LogMailer is a Mailer that only logs the message. It lets the double-opt-in
// and notification flows run end-to-end without an SMTP server (DEV_MODE).
type LogMailer struct{ Logger *slog.Logger }

// Send implements Mailer.
func (m LogMailer) Send(_ context.Context, to, subject, _, _ string) error {
	if m.Logger != nil {
		m.Logger.Info("status-page mail (log-only)", "to", to, "subject", subject)
	}
	return nil
}

// SMTP encryption modes, matching config.SMTPConfig / the SMTP notifier.
const (
	smtpEncNone     = "none"
	smtpEncStartTLS = "starttls"
	smtpEncTLS      = "tls" // implicit TLS ("SMTPS"), usually port 465
)

// smtpDialTimeout bounds establishing the SMTP connection.
const smtpDialTimeout = 10 * time.Second

// SMTPSettings is the wire configuration an SMTPMailer dials with. It is the
// mailer's own view of config.SMTPConfig, kept here so the api package does not
// depend on config just to send mail.
type SMTPSettings struct {
	Host       string
	Port       int // 0 = derive from Encryption (465 for tls, else 587)
	Username   string
	Password   string
	From       string
	Encryption string // "none", "starttls" (default) or "tls"
}

func (s SMTPSettings) encryption() string {
	if s.Encryption == "" {
		return smtpEncStartTLS
	}
	return strings.ToLower(s.Encryption)
}

func (s SMTPSettings) effectivePort() int {
	if s.Port != 0 {
		return s.Port
	}
	if s.encryption() == smtpEncTLS {
		return 465
	}
	return 587
}

// smtpSendFunc performs the actual SMTP transaction. It is a field on SMTPMailer
// so tests can capture the assembled message without a live server.
type smtpSendFunc func(ctx context.Context, s SMTPSettings, to string, msg []byte) error

// SMTPMailer delivers transactional mail through a real SMTP server. It is the
// production Mailer: main.go selects it whenever SMTP is configured and falls
// back to LogMailer (with a loud warning) otherwise, so that the reset and
// subscriber flows either send for real or make their non-delivery obvious.
type SMTPMailer struct {
	settings SMTPSettings
	send     smtpSendFunc
	now      func() time.Time
	logger   *slog.Logger
}

// NewSMTPMailer builds an SMTPMailer that talks to a real server.
func NewSMTPMailer(settings SMTPSettings, logger *slog.Logger) *SMTPMailer {
	return &SMTPMailer{settings: settings, send: smtpSend, now: time.Now, logger: logger}
}

// Send implements Mailer: it assembles a MIME message (text + optional HTML,
// with header-injection-safe headers) and hands it to the SMTP transaction.
func (m *SMTPMailer) Send(ctx context.Context, to, subject, textBody, htmlBody string) error {
	msg := buildMIMEMessage(m.settings.From, to, subject, textBody, htmlBody, m.now())
	if err := m.send(ctx, m.settings, to, msg); err != nil {
		return fmt.Errorf("smtp: send to %s via %s: %w", to, m.settings.Host, err)
	}
	return nil
}

// smtpSend runs a real SMTP transaction, honouring ctx for the dial and the
// whole exchange. It mirrors the SMTP notification provider's transport.
func smtpSend(ctx context.Context, s SMTPSettings, to string, msg []byte) error {
	addr := net.JoinHostPort(s.Host, strconv.Itoa(s.effectivePort()))
	dialer := net.Dialer{Timeout: smtpDialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	if s.encryption() == smtpEncTLS {
		conn = tls.Client(conn, &tls.Config{ServerName: s.Host})
	}
	client, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp handshake: %w", err)
	}
	defer func() { _ = client.Close() }()

	if s.encryption() == smtpEncStartTLS {
		if err := client.StartTLS(&tls.Config{ServerName: s.Host}); err != nil {
			return fmt.Errorf("starttls: %w", err)
		}
	}
	if s.Username != "" {
		if err := client.Auth(smtp.PlainAuth("", s.Username, s.Password, s.Host)); err != nil {
			return fmt.Errorf("auth: %w", err)
		}
	}
	if err := client.Mail(s.From); err != nil {
		return fmt.Errorf("MAIL FROM: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("RCPT TO %s: %w", to, err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("DATA: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("write body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("close body: %w", err)
	}
	return client.Quit()
}

// WSPublisher broadcasts a public status-page update to WebSocket subscribers of
// the status-page:{slug} topic. *ws.Hub satisfies it.
type WSPublisher interface {
	PublishStatusPage(slug string, event string, data any)
}
