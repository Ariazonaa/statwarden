// Package opsgenie delivers notifications through the Atlassian Opsgenie Alert
// API. A DOWN event creates an alert and the matching UP event closes it,
// keyed by a stable alias derived from the monitor id. It follows the
// reference webhook provider's shape: implement registry.Notifier,
// self-register in init(), blank-import from cmd/statwarden.
package opsgenie

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

// Base API hosts by region. Tests point baseURL at an httptest server instead.
const (
	baseURLUS = "https://api.opsgenie.com"
	baseURLEU = "https://api.eu.opsgenie.com"
)

// Config is the Opsgenie channel's plaintext configuration (the secrets layer
// encrypts the whole blob at rest).
type Config struct {
	// APIKey authenticates against an Opsgenie API integration (required).
	APIKey string `json:"api_key"`
	// Region selects the data residency host: "eu" for the EU instance,
	// "us"/"" for the default US instance.
	Region string `json:"region,omitempty"`
}

// createAlertRequest is the POST /v2/alerts body.
type createAlertRequest struct {
	Message     string            `json:"message"`
	Alias       string            `json:"alias"`
	Description string            `json:"description,omitempty"`
	Priority    string            `json:"priority,omitempty"`
	Source      string            `json:"source,omitempty"`
	Tags        []string          `json:"tags,omitempty"`
	Details     map[string]string `json:"details,omitempty"`
}

// closeAlertRequest is the POST /v2/alerts/{alias}/close body.
type closeAlertRequest struct {
	Source string `json:"source,omitempty"`
	Note   string `json:"note,omitempty"`
}

// apiResponse is the Opsgenie response envelope.
type apiResponse struct {
	Result  string `json:"result"`
	Message string `json:"message"`
}

// Notifier is the Opsgenie Alert API provider.
type Notifier struct {
	client *http.Client
	// baseURL overrides the region host in tests; empty means "derive from
	// Config.Region".
	baseURL string
}

// New builds an Opsgenie Notifier with a timeout-bounded HTTP client.
func New() *Notifier {
	return &Notifier{client: httpx.NotifierClient(httpTimeout)}
}

// Type implements registry.Notifier.
func (*Notifier) Type() string { return "opsgenie" }

// Validate implements registry.Notifier.
func (*Notifier) Validate(cfg json.RawMessage) error {
	c, err := parse(cfg)
	if err != nil {
		return err
	}
	if c.APIKey == "" {
		return errors.New("opsgenie: api_key is required")
	}
	switch strings.ToLower(c.Region) {
	case "", "us", "eu":
	default:
		return fmt.Errorf("opsgenie: region %q must be one of \"us\" or \"eu\"", c.Region)
	}
	return nil
}

// Send implements registry.Notifier.
func (n *Notifier) Send(ctx context.Context, cfg json.RawMessage, ev domain.NotificationEvent) error {
	c, err := parse(cfg)
	if err != nil {
		return err
	}

	alias := aliasFor(ev)
	base := n.base(c)

	var (
		endpoint string
		body     any
	)
	if ev.Kind == domain.EventKindUp {
		endpoint = fmt.Sprintf("%s/v2/alerts/%s/close?identifierType=alias", base, url.PathEscape(alias))
		body = closeAlertRequest{Source: "statwarden", Note: ev.Title}
	} else {
		endpoint = base + "/v2/alerts"
		body = createAlertRequest{
			Message:     message(ev),
			Alias:       alias,
			Description: ev.Body,
			Priority:    priority(ev),
			Source:      "statwarden",
			Tags:        []string{"statwarden", string(ev.Kind)},
			Details:     details(ev),
		}
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("opsgenie: marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("opsgenie: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "GenieKey "+c.APIKey)
	req.Header.Set("User-Agent", "Statwarden")

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("opsgenie: alert request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr apiResponse
		if json.Unmarshal(data, &apiErr) == nil && apiErr.Message != "" {
			return fmt.Errorf("opsgenie: alert request: %s (status %s)", apiErr.Message, resp.Status)
		}
		return fmt.Errorf("opsgenie: alert request: unexpected status %s", resp.Status)
	}
	return nil
}

// base returns the region host, honouring a test override.
func (n *Notifier) base(c Config) string {
	if n.baseURL != "" {
		return strings.TrimRight(n.baseURL, "/")
	}
	if strings.EqualFold(c.Region, "eu") {
		return baseURLEU
	}
	return baseURLUS
}

// aliasFor ties the create and its later close to the same alert.
func aliasFor(ev domain.NotificationEvent) string {
	return fmt.Sprintf("statwarden-monitor-%d", ev.MonitorID)
}

func message(ev domain.NotificationEvent) string {
	if ev.Title != "" {
		return ev.Title
	}
	if ev.Body != "" {
		return ev.Body
	}
	return "Statwarden notification"
}

// priority maps the event kind onto the Opsgenie priority scale (P1..P5).
func priority(ev domain.NotificationEvent) string {
	switch ev.Kind {
	case domain.EventKindDown:
		return "P1"
	case domain.EventKindCertExpiry:
		return "P3"
	case domain.EventKindMaintenanceStart, domain.EventKindMaintenanceEnd:
		return "P5"
	default:
		return "P3"
	}
}

func details(ev domain.NotificationEvent) map[string]string {
	d := map[string]string{}
	if ev.MonitorName != "" {
		d["monitor"] = ev.MonitorName
	}
	if ev.MonitorURL != "" {
		d["url"] = ev.MonitorURL
	}
	if ev.Status != "" {
		d["status"] = string(ev.Status)
	}
	if len(d) == 0 {
		return nil
	}
	return d
}

func parse(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		return c, fmt.Errorf("opsgenie: invalid config: %w", err)
	}
	return c, nil
}
