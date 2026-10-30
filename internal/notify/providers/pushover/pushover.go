// Package pushover delivers notifications through the Pushover messages API by
// POSTing a form-encoded request. It follows the reference webhook provider's
// shape: implement registry.Notifier, self-register in init(), blank-import
// from cmd/statwarden.
package pushover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
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

// defaultBaseURL is the public Pushover API host. Tests point baseURL at an
// httptest server instead.
const defaultBaseURL = "https://api.pushover.net"

// Config is the Pushover channel's plaintext configuration (the secrets layer
// encrypts the whole blob at rest).
type Config struct {
	// Token is the Pushover application/API token (required).
	Token string `json:"token"`
	// User is the user or group key to deliver to (required).
	User string `json:"user"`
	// Device restricts delivery to a named device (optional).
	Device string `json:"device,omitempty"`
	// Priority overrides the derived message priority (-2..2). A pointer so
	// that priority 0 (normal) can be set explicitly.
	Priority *int `json:"priority,omitempty"`
}

// apiResponse is the Pushover response envelope.
type apiResponse struct {
	Status  int      `json:"status"`
	Errors  []string `json:"errors"`
	Request string   `json:"request"`
}

// Notifier is the Pushover provider.
type Notifier struct {
	client  *http.Client
	baseURL string
}

// New builds a Pushover Notifier with a timeout-bounded HTTP client pointed at
// the public API.
func New() *Notifier {
	return &Notifier{client: httpx.NotifierClient(httpTimeout), baseURL: defaultBaseURL}
}

// Type implements registry.Notifier.
func (*Notifier) Type() string { return "pushover" }

// Validate implements registry.Notifier.
func (*Notifier) Validate(cfg json.RawMessage) error {
	c, err := parse(cfg)
	if err != nil {
		return err
	}
	if c.Token == "" {
		return errors.New("pushover: token is required")
	}
	if c.User == "" {
		return errors.New("pushover: user is required")
	}
	if c.Priority != nil && (*c.Priority < -2 || *c.Priority > 2) {
		return fmt.Errorf("pushover: priority %d must be between -2 and 2", *c.Priority)
	}
	return nil
}

// Send implements registry.Notifier.
func (n *Notifier) Send(ctx context.Context, cfg json.RawMessage, event domain.NotificationEvent) error {
	c, err := parse(cfg)
	if err != nil {
		return err
	}

	form := url.Values{}
	form.Set("token", c.Token)
	form.Set("user", c.User)
	form.Set("message", message(event))
	if event.Title != "" {
		form.Set("title", event.Title)
	}
	if c.Device != "" {
		form.Set("device", c.Device)
	}
	if event.MonitorURL != "" {
		form.Set("url", event.MonitorURL)
		if event.MonitorName != "" {
			form.Set("url_title", event.MonitorName)
		}
	}
	form.Set("priority", strconv.Itoa(priority(c, event)))
	if !event.At.IsZero() {
		form.Set("timestamp", strconv.FormatInt(event.At.Unix(), 10))
	}

	endpoint := strings.TrimRight(n.baseURL, "/") + "/1/messages.json"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("pushover: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Statwarden")

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("pushover: messages: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr apiResponse
		if json.Unmarshal(data, &apiErr) == nil && len(apiErr.Errors) > 0 {
			return fmt.Errorf("pushover: messages: %s (status %s)", strings.Join(apiErr.Errors, "; "), resp.Status)
		}
		return fmt.Errorf("pushover: messages: unexpected status %s", resp.Status)
	}
	return nil
}

func message(event domain.NotificationEvent) string {
	if event.Body != "" {
		return event.Body
	}
	if event.Title != "" {
		return event.Title
	}
	return "Statwarden notification"
}

// priority resolves the Pushover priority: an explicit config override wins,
// otherwise it derives from the event kind (-2 lowest .. 2 emergency).
func priority(c Config, event domain.NotificationEvent) int {
	if c.Priority != nil {
		return *c.Priority
	}
	switch event.Kind {
	case domain.EventKindDown:
		return 1
	case domain.EventKindCertExpiry:
		return 0
	case domain.EventKindDigest:
		return -1
	default:
		return 0
	}
}

func parse(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		return c, fmt.Errorf("pushover: invalid config: %w", err)
	}
	return c, nil
}
