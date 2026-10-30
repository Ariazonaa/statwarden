// Package apprise bridges notifications to an Apprise API server, which in turn
// fans out to 80+ downstream services. It follows the reference slack
// provider's shape: implement registry.Notifier, self-register in init(),
// blank-import from cmd/statwarden.
package apprise

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

// Config is the Apprise channel's plaintext configuration (the secrets layer
// encrypts the whole blob at rest).
type Config struct {
	// ServerURL is the base URL of the Apprise API server (required, http(s)).
	ServerURL string `json:"server_url"`
	// URLs are Apprise service URLs for a stateless notification. Either URLs
	// or ConfigKey must be set.
	URLs []string `json:"urls,omitempty"`
	// ConfigKey selects a persisted server-side Apprise configuration; the
	// request is then POSTed to /notify/{config_key}. Either URLs or ConfigKey
	// must be set.
	ConfigKey string `json:"config_key,omitempty"`
	// Tag optionally limits which of a persisted config's URLs are notified.
	Tag string `json:"tag,omitempty"`
}

// payload is the JSON body the Apprise /notify endpoint accepts.
type payload struct {
	URLs   string `json:"urls,omitempty"`
	Tag    string `json:"tag,omitempty"`
	Title  string `json:"title,omitempty"`
	Body   string `json:"body"`
	Type   string `json:"type,omitempty"`
	Format string `json:"format,omitempty"`
}

// Notifier is the Apprise API bridge provider.
type Notifier struct {
	client *http.Client
}

// New builds an Apprise Notifier with a timeout-bounded HTTP client.
func New() *Notifier {
	return &Notifier{client: httpx.NotifierClient(httpTimeout)}
}

// Type implements registry.Notifier.
func (*Notifier) Type() string { return "apprise" }

// Validate implements registry.Notifier.
func (*Notifier) Validate(cfg json.RawMessage) error {
	c, err := parse(cfg)
	if err != nil {
		return err
	}
	if c.ServerURL == "" {
		return errors.New("apprise: server_url is required")
	}
	u, err := url.Parse(c.ServerURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("apprise: server_url %q must be an absolute http(s) URL", c.ServerURL)
	}
	if len(c.URLs) == 0 && c.ConfigKey == "" {
		return errors.New("apprise: either urls or config_key is required")
	}
	if len(c.URLs) > 0 && c.ConfigKey != "" {
		return errors.New("apprise: set only one of urls or config_key, not both")
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
		Tag:   c.Tag,
		Title: event.Title,
		Body:  buildBody(event),
		Type:  notifyType(event),
	}
	base := strings.TrimRight(c.ServerURL, "/")
	var endpoint string
	if c.ConfigKey != "" {
		endpoint = base + "/notify/" + url.PathEscape(c.ConfigKey)
	} else {
		endpoint = base + "/notify"
		body.URLs = strings.Join(c.URLs, ",")
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("apprise: marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("apprise: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Statwarden")

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("apprise: POST notify: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("apprise: POST notify: unexpected status %s", resp.Status)
	}
	return nil
}

// buildBody renders the notification body from the already-rendered event.
func buildBody(event domain.NotificationEvent) string {
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

// notifyType maps the event to Apprise's message type vocabulary.
func notifyType(event domain.NotificationEvent) string {
	switch event.Kind {
	case domain.EventKindUp:
		return "success"
	case domain.EventKindDown:
		return "failure"
	case domain.EventKindCertExpiry, domain.EventKindMaintenanceStart, domain.EventKindMaintenanceEnd:
		return "warning"
	default:
		return "info"
	}
}

func parse(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		return c, fmt.Errorf("apprise: invalid config: %w", err)
	}
	return c, nil
}
