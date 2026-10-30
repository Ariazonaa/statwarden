// Package ntfy delivers notifications to an ntfy topic (ntfy.sh or a
// self-hosted server) by POSTing the message body with the ntfy header
// controls (title, priority, tags). It follows the reference webhook
// provider's shape: implement registry.Notifier, self-register in init(),
// blank-import from cmd/statwarden.
package ntfy

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

// defaultServerURL is the public ntfy host used when the config omits one.
const defaultServerURL = "https://ntfy.sh"

// Config is the ntfy channel's plaintext configuration (the secrets layer
// encrypts the whole blob at rest).
type Config struct {
	// ServerURL is the ntfy base URL; defaults to https://ntfy.sh.
	ServerURL string `json:"server_url,omitempty"`
	// Topic is the target topic (required).
	Topic string `json:"topic"`
	// Token is an access token for protected topics (Bearer auth). Mutually
	// exclusive with Username/Password.
	Token string `json:"token,omitempty"`
	// Username/Password authenticate with HTTP Basic when Token is unset.
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	// Priority overrides the derived message priority (1..5).
	Priority int `json:"priority,omitempty"`
	// Tags are extra ntfy tags/emoji short-codes added to every message.
	Tags []string `json:"tags,omitempty"`
}

// Notifier is the ntfy provider.
type Notifier struct {
	client *http.Client
}

// New builds an ntfy Notifier with a timeout-bounded HTTP client.
func New() *Notifier {
	return &Notifier{client: httpx.NotifierClient(httpTimeout)}
}

// Type implements registry.Notifier.
func (*Notifier) Type() string { return "ntfy" }

// Validate implements registry.Notifier.
func (*Notifier) Validate(cfg json.RawMessage) error {
	c, err := parse(cfg)
	if err != nil {
		return err
	}
	if c.Topic == "" {
		return errors.New("ntfy: topic is required")
	}
	server := c.ServerURL
	if server == "" {
		server = defaultServerURL
	}
	u, err := url.Parse(server)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("ntfy: server_url %q must be an absolute http(s) URL", server)
	}
	if c.Token != "" && (c.Username != "" || c.Password != "") {
		return errors.New("ntfy: use either token or username/password, not both")
	}
	if c.Priority != 0 && (c.Priority < 1 || c.Priority > 5) {
		return fmt.Errorf("ntfy: priority %d must be between 1 and 5", c.Priority)
	}
	return nil
}

// Send implements registry.Notifier.
func (n *Notifier) Send(ctx context.Context, cfg json.RawMessage, event domain.NotificationEvent) error {
	c, err := parse(cfg)
	if err != nil {
		return err
	}

	server := c.ServerURL
	if server == "" {
		server = defaultServerURL
	}
	endpoint := strings.TrimRight(server, "/") + "/" + c.Topic

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body(event)))
	if err != nil {
		return fmt.Errorf("ntfy: build request: %w", err)
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	req.Header.Set("User-Agent", "Statwarden")
	if event.Title != "" {
		req.Header.Set("X-Title", event.Title)
	}
	req.Header.Set("X-Priority", strconv.Itoa(priority(c, event)))
	if tags := tags(c, event); tags != "" {
		req.Header.Set("X-Tags", tags)
	}
	switch {
	case c.Token != "":
		req.Header.Set("Authorization", "Bearer "+c.Token)
	case c.Username != "" || c.Password != "":
		req.SetBasicAuth(c.Username, c.Password)
	}

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy: POST %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy: POST %s: unexpected status %s", endpoint, resp.Status)
	}
	return nil
}

// body is the plain-text message; ntfy renders the title from the X-Title
// header, so the body carries the detail plus a URL when present.
func body(event domain.NotificationEvent) string {
	var b strings.Builder
	b.WriteString(event.Body)
	if event.MonitorURL != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(event.MonitorURL)
	}
	if b.Len() == 0 {
		return event.Title
	}
	return b.String()
}

// priority resolves the ntfy priority: an explicit config override wins,
// otherwise it derives from the event kind (1=min .. 5=max).
func priority(c Config, event domain.NotificationEvent) int {
	if c.Priority != 0 {
		return c.Priority
	}
	switch event.Kind {
	case domain.EventKindDown:
		return 5
	case domain.EventKindCertExpiry:
		return 4
	case domain.EventKindUp:
		return 3
	case domain.EventKindDigest:
		return 2
	default:
		return 3
	}
}

// tags merges the status emoji short-code with any configured tags.
func tags(c Config, event domain.NotificationEvent) string {
	all := make([]string, 0, len(c.Tags)+1)
	if e := statusTag(event); e != "" {
		all = append(all, e)
	}
	all = append(all, c.Tags...)
	return strings.Join(all, ",")
}

// statusTag maps the event onto an ntfy emoji short-code.
func statusTag(event domain.NotificationEvent) string {
	switch event.Kind {
	case domain.EventKindUp:
		return "green_circle"
	case domain.EventKindDown:
		return "red_circle"
	case domain.EventKindCertExpiry:
		return "lock"
	case domain.EventKindMaintenanceStart, domain.EventKindMaintenanceEnd:
		return "wrench"
	case domain.EventKindTest:
		return "test_tube"
	case domain.EventKindDigest:
		return "bar_chart"
	default:
		return "bell"
	}
}

func parse(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		return c, fmt.Errorf("ntfy: invalid config: %w", err)
	}
	return c, nil
}
