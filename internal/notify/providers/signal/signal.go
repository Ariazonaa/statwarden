// Package signal delivers notifications to Signal recipients through a
// self-hosted signal-cli REST API server. It follows the reference slack
// provider's shape: implement registry.Notifier, self-register in init(),
// blank-import from cmd/statwarden.
package signal

import (
	"bytes"
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

// Config is the Signal channel's plaintext configuration (the secrets layer
// encrypts the whole blob at rest).
type Config struct {
	// ServerURL is the base URL of the signal-cli REST API (required, http(s)).
	ServerURL string `json:"server_url"`
	// Number is the registered sender number in E.164 form, e.g. "+4915112345678"
	// (required).
	Number string `json:"number"`
	// Recipients are the target numbers/group ids the message goes to (required,
	// at least one).
	Recipients []string `json:"recipients"`
}

// payload is the JSON body the signal-cli REST API /v2/send endpoint accepts.
type payload struct {
	Message    string   `json:"message"`
	Number     string   `json:"number"`
	Recipients []string `json:"recipients"`
}

// Notifier is the signal-cli REST API provider.
type Notifier struct {
	client *http.Client
}

// New builds a Signal Notifier with a timeout-bounded HTTP client.
func New() *Notifier {
	return &Notifier{client: httpx.NotifierClient(httpTimeout)}
}

// Type implements registry.Notifier.
func (*Notifier) Type() string { return "signal" }

// Validate implements registry.Notifier.
func (*Notifier) Validate(cfg json.RawMessage) error {
	c, err := parse(cfg)
	if err != nil {
		return err
	}
	if c.ServerURL == "" {
		return errors.New("signal: server_url is required")
	}
	u, err := url.Parse(c.ServerURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("signal: server_url %q must be an absolute http(s) URL", c.ServerURL)
	}
	if c.Number == "" {
		return errors.New("signal: number is required")
	}
	if len(c.Recipients) == 0 {
		return errors.New("signal: at least one recipient is required")
	}
	for i, r := range c.Recipients {
		if strings.TrimSpace(r) == "" {
			return fmt.Errorf("signal: recipients[%d] is empty", i)
		}
	}
	return nil
}

// Send implements registry.Notifier.
func (n *Notifier) Send(ctx context.Context, cfg json.RawMessage, event domain.NotificationEvent) error {
	c, err := parse(cfg)
	if err != nil {
		return err
	}

	body := payload{
		Message:    buildMessage(event),
		Number:     c.Number,
		Recipients: c.Recipients,
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("signal: marshal payload: %w", err)
	}

	endpoint := strings.TrimRight(c.ServerURL, "/") + "/v2/send"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("signal: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Statwarden")

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("signal: POST send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("signal: POST send: unexpected status %s", resp.Status)
	}
	return nil
}

// buildMessage renders a plain-text message from the already-rendered event.
func buildMessage(event domain.NotificationEvent) string {
	var b strings.Builder
	b.WriteString(event.Title)
	if event.Body != "" {
		b.WriteString("\n\n")
		b.WriteString(event.Body)
	}
	if event.MonitorURL != "" {
		b.WriteString("\n")
		b.WriteString(event.MonitorURL)
	}
	return b.String()
}

func parse(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		return c, fmt.Errorf("signal: invalid config: %w", err)
	}
	return c, nil
}
