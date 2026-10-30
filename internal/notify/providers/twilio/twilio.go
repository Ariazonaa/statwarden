// Package twilio delivers notifications as SMS messages through the Twilio
// REST API. It follows the reference slack provider's shape: implement
// registry.Notifier, self-register in init(), blank-import from cmd/statwarden.
package twilio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/httpx"
	"github.com/Ariazonaa/statwarden/internal/notify/registry"
)

func init() { registry.Register(New()) }

// httpTimeout bounds a single delivery; the dispatcher also applies its own
// per-send context deadline.
const httpTimeout = 10 * time.Second

// defaultBaseURL is the public Twilio REST API host. Tests point baseURL at an
// httptest server instead.
const defaultBaseURL = "https://api.twilio.com"

// maxSMSLen keeps the message within a sane length for SMS delivery; longer
// bodies are truncated with an ellipsis.
const maxSMSLen = 1500

// Config is the Twilio channel's plaintext configuration (the secrets layer
// encrypts the whole blob at rest).
type Config struct {
	// AccountSID is the Twilio account identifier (required, "AC...").
	AccountSID string `json:"account_sid"`
	// AuthToken is the Twilio auth token used for HTTP basic auth (required).
	AuthToken string `json:"auth_token"`
	// From is the sending phone number or messaging-service sender (required).
	From string `json:"from"`
	// To are the destination phone numbers in E.164 form (required, at least one).
	To []string `json:"to"`
}

// Notifier is the Twilio SMS provider.
type Notifier struct {
	client  *http.Client
	baseURL string
}

// New builds a Twilio Notifier with a timeout-bounded HTTP client pointed at
// the public REST API.
func New() *Notifier {
	return &Notifier{client: httpx.NotifierClient(httpTimeout), baseURL: defaultBaseURL}
}

// Type implements registry.Notifier.
func (*Notifier) Type() string { return "twilio" }

// Validate implements registry.Notifier.
func (*Notifier) Validate(cfg json.RawMessage) error {
	c, err := parse(cfg)
	if err != nil {
		return err
	}
	if c.AccountSID == "" {
		return errors.New("twilio: account_sid is required")
	}
	if c.AuthToken == "" {
		return errors.New("twilio: auth_token is required")
	}
	if c.From == "" {
		return errors.New("twilio: from is required")
	}
	if len(c.To) == 0 {
		return errors.New("twilio: at least one recipient in to is required")
	}
	for i, to := range c.To {
		if strings.TrimSpace(to) == "" {
			return fmt.Errorf("twilio: to[%d] is empty", i)
		}
	}
	return nil
}

// Send implements registry.Notifier. Twilio sends one message per recipient,
// so a delivery is a POST per destination; the first failure aborts.
func (n *Notifier) Send(ctx context.Context, cfg json.RawMessage, event domain.NotificationEvent) error {
	c, err := parse(cfg)
	if err != nil {
		return err
	}

	endpoint := fmt.Sprintf("%s/2010-04-01/Accounts/%s/Messages.json",
		strings.TrimRight(n.baseURL, "/"), url.PathEscape(c.AccountSID))
	message := buildMessage(event)

	for _, to := range c.To {
		if err := n.sendOne(ctx, endpoint, c, to, message); err != nil {
			return err
		}
	}
	return nil
}

func (n *Notifier) sendOne(ctx context.Context, endpoint string, c Config, to, message string) error {
	form := url.Values{}
	form.Set("From", c.From)
	form.Set("To", to)
	form.Set("Body", message)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("twilio: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Statwarden")
	req.SetBasicAuth(c.AccountSID, c.AuthToken)

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("twilio: POST message: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &apiErr) == nil && apiErr.Message != "" {
			return fmt.Errorf("twilio: POST message to %s: %s (status %s)", to, apiErr.Message, resp.Status)
		}
		return fmt.Errorf("twilio: POST message to %s: unexpected status %s", to, resp.Status)
	}
	return nil
}

// buildMessage renders a plain-text SMS body from the already-rendered event.
func buildMessage(event domain.NotificationEvent) string {
	var b strings.Builder
	b.WriteString(event.Title)
	if event.Body != "" {
		b.WriteString("\n")
		b.WriteString(event.Body)
	}
	msg := b.String()
	if len(msg) > maxSMSLen {
		msg = msg[:maxSMSLen-1] + "…"
	}
	return msg
}

func parse(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		return c, fmt.Errorf("twilio: invalid config: %w", err)
	}
	return c, nil
}
