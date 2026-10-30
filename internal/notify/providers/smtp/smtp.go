// Package smtp delivers notifications as e-mail through any SMTP server. It
// follows the reference webhook provider's shape: implement registry.Notifier,
// self-register in init(), blank-import from cmd/statwarden. The actual wire send
// is behind an injectable function so tests capture the message without a real
// server.
package smtp

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/notify/registry"
)

func init() { registry.Register(New()) }

// dialTimeout bounds establishing the SMTP connection.
const dialTimeout = 10 * time.Second

// Encryption modes for the SMTP connection.
const (
	encNone     = "none"
	encStartTLS = "starttls"
	encTLS      = "tls" // implicit TLS ("SMTPS", usually port 465)
)

// Config is the SMTP channel's plaintext configuration (the secrets layer
// encrypts the whole blob at rest).
type Config struct {
	// Host is the SMTP server hostname (required).
	Host string `json:"host"`
	// Port is the SMTP server port; defaults to 465 for tls, else 587.
	Port int `json:"port,omitempty"`
	// Username / Password enable SMTP AUTH (PLAIN); omit for anonymous relays.
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	// From is the envelope and header sender address (required).
	From string `json:"from"`
	// To is the list of recipient addresses (at least one required).
	To []string `json:"to"`
	// Encryption is one of "none", "starttls" (default) or "tls".
	Encryption string `json:"encryption,omitempty"`
}

func (c Config) encryption() string {
	if c.Encryption == "" {
		return encStartTLS
	}
	return strings.ToLower(c.Encryption)
}

func (c Config) effectivePort() int {
	if c.Port != 0 {
		return c.Port
	}
	if c.encryption() == encTLS {
		return 465
	}
	return 587
}

// sendFunc performs the actual SMTP transaction. It is a field on Notifier so
// tests can replace it with an in-process capture.
type sendFunc func(ctx context.Context, c Config, msg []byte) error

// Notifier is the SMTP e-mail provider.
type Notifier struct {
	send sendFunc
	now  func() time.Time
}

// New builds an SMTP Notifier that talks to a real server.
func New() *Notifier {
	return &Notifier{send: smtpSend, now: time.Now}
}

// Type implements registry.Notifier.
func (*Notifier) Type() string { return "smtp" }

// Validate implements registry.Notifier.
func (*Notifier) Validate(cfg json.RawMessage) error {
	c, err := parse(cfg)
	if err != nil {
		return err
	}
	if c.Host == "" {
		return errors.New("smtp: host is required")
	}
	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("smtp: port %d out of range 1-65535", c.Port)
	}
	if c.From == "" {
		return errors.New("smtp: from is required")
	}
	if _, err := mail.ParseAddress(c.From); err != nil {
		return fmt.Errorf("smtp: from %q is not a valid address: %w", c.From, err)
	}
	if len(c.To) == 0 {
		return errors.New("smtp: at least one recipient (to) is required")
	}
	for _, addr := range c.To {
		if _, err := mail.ParseAddress(addr); err != nil {
			return fmt.Errorf("smtp: recipient %q is not a valid address: %w", addr, err)
		}
	}
	switch c.encryption() {
	case encNone, encStartTLS, encTLS:
	default:
		return fmt.Errorf("smtp: encryption %q must be one of none, starttls, tls", c.Encryption)
	}
	return nil
}

// Send implements registry.Notifier.
func (n *Notifier) Send(ctx context.Context, cfg json.RawMessage, event domain.NotificationEvent) error {
	c, err := parse(cfg)
	if err != nil {
		return err
	}
	msg := n.buildMessage(c, event)
	if err := n.send(ctx, c, msg); err != nil {
		return fmt.Errorf("smtp: send via %s: %w", c.Host, err)
	}
	return nil
}

// buildMessage renders an RFC 5322 text/plain message from the event. Header
// values are sanitised against CRLF injection and the subject is Q-encoded
// when it contains non-ASCII text.
func (n *Notifier) buildMessage(c Config, event domain.NotificationEvent) []byte {
	subject := event.Title
	if subject == "" {
		subject = "Statwarden notification"
	}
	var b strings.Builder
	b.WriteString("From: " + sanitizeHeader(c.From) + "\r\n")
	b.WriteString("To: " + sanitizeHeader(strings.Join(c.To, ", ")) + "\r\n")
	b.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", sanitizeHeader(subject)) + "\r\n")
	b.WriteString("Date: " + n.now().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")
	b.WriteString(normalizeBody(event.Body))
	return []byte(b.String())
}

// smtpSend runs a real SMTP transaction honouring ctx for the dial and,
// where possible, the whole exchange.
func smtpSend(ctx context.Context, c Config, msg []byte) error {
	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.effectivePort()))
	dialer := net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	if c.encryption() == encTLS {
		conn = tls.Client(conn, &tls.Config{ServerName: c.Host})
	}
	client, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp handshake: %w", err)
	}
	defer func() { _ = client.Close() }()

	if c.encryption() == encStartTLS {
		if err := client.StartTLS(&tls.Config{ServerName: c.Host}); err != nil {
			return fmt.Errorf("starttls: %w", err)
		}
	}
	if c.Username != "" {
		if err := client.Auth(smtp.PlainAuth("", c.Username, c.Password, c.Host)); err != nil {
			return fmt.Errorf("auth: %w", err)
		}
	}
	if err := client.Mail(c.From); err != nil {
		return fmt.Errorf("MAIL FROM: %w", err)
	}
	for _, rcpt := range c.To {
		if err := client.Rcpt(rcpt); err != nil {
			return fmt.Errorf("RCPT TO %s: %w", rcpt, err)
		}
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

// sanitizeHeader strips CR and LF to prevent header injection.
func sanitizeHeader(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}

// normalizeBody ensures CRLF line endings for the message body.
func normalizeBody(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}

func parse(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		return c, fmt.Errorf("smtp: invalid config: %w", err)
	}
	return c, nil
}
