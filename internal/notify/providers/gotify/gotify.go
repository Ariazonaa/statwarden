// Package gotify delivers notifications to a self-hosted Gotify server through
// its message API. It follows the reference slack provider's shape: implement
// registry.Notifier, self-register in init(), blank-import from cmd/statwarden.
package gotify

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

// defaultPriority is used when the config leaves priority unset.
const defaultPriority = 5

// Config is the Gotify channel's plaintext configuration (the secrets layer
// encrypts the whole blob at rest).
type Config struct {
	// ServerURL is the base URL of the Gotify server (required, http(s)).
	ServerURL string `json:"server_url"`
	// AppToken authenticates as a Gotify application (required).
	AppToken string `json:"app_token"`
	// Priority sets the Gotify message priority (0-10); 0 means "use default".
	Priority int `json:"priority,omitempty"`
}

// payload is the JSON body the Gotify /message endpoint accepts.
type payload struct {
	Title    string `json:"title"`
	Message  string `json:"message"`
	Priority int    `json:"priority"`
}

// Notifier is the Gotify provider.
type Notifier struct {
	client *http.Client
}

// New builds a Gotify Notifier with a timeout-bounded HTTP client.
func New() *Notifier {
	return &Notifier{client: httpx.NotifierClient(httpTimeout, "X-Gotify-Key")}
}

// Type implements registry.Notifier.
func (*Notifier) Type() string { return "gotify" }

// Validate implements registry.Notifier.
func (*Notifier) Validate(cfg json.RawMessage) error {
	c, err := parse(cfg)
	if err != nil {
		return err
	}
	if c.ServerURL == "" {
		return errors.New("gotify: server_url is required")
	}
	u, err := url.Parse(c.ServerURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("gotify: server_url %q must be an absolute http(s) URL", c.ServerURL)
	}
	if c.AppToken == "" {
		return errors.New("gotify: app_token is required")
	}
	if c.Priority < 0 || c.Priority > 10 {
		return fmt.Errorf("gotify: priority %d out of range 0-10", c.Priority)
	}
	return nil
}

// Send implements registry.Notifier.
func (n *Notifier) Send(ctx context.Context, cfg json.RawMessage, event domain.NotificationEvent) error {
	c, err := parse(cfg)
	if err != nil {
		return err
	}

	priority := c.Priority
	if priority == 0 {
		priority = defaultPriority
	}
	body := payload{
		Title:    event.Title,
		Message:  buildMessage(event),
		Priority: priority,
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("gotify: marshal payload: %w", err)
	}

	endpoint := strings.TrimRight(c.ServerURL, "/") + "/message"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("gotify: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gotify-Key", c.AppToken)
	req.Header.Set("User-Agent", "Statwarden")

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("gotify: POST message: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("gotify: POST message: unexpected status %s", resp.Status)
	}
	return nil
}

// buildMessage renders the message body from the already-rendered event.
func buildMessage(event domain.NotificationEvent) string {
	var b strings.Builder
	b.WriteString(event.Body)
	if event.MonitorURL != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(event.MonitorURL)
	}
	if b.Len() == 0 {
		return event.Title
	}
	return b.String()
}

func parse(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		return c, fmt.Errorf("gotify: invalid config: %w", err)
	}
	return c, nil
}
