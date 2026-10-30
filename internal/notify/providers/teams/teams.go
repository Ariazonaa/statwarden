// Package teams delivers notifications to a Microsoft Teams channel through an
// incoming webhook, posting a legacy MessageCard document (still accepted by
// both classic connectors and Workflows-based webhooks). It follows the
// reference webhook provider's shape: implement registry.Notifier,
// self-register in init(), blank-import from cmd/statwarden.
package teams

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/httpx"
	"github.com/Ariazonaa/statwarden/internal/notify/registry"
)

func init() { registry.Register(New()) }

// httpTimeout bounds a single delivery; the dispatcher also applies its own
// per-send context deadline.
const httpTimeout = 10 * time.Second

// Config is the Teams channel's plaintext configuration (the secrets layer
// encrypts the whole blob at rest).
type Config struct {
	// WebhookURL is the Teams incoming-webhook endpoint (required, https).
	WebhookURL string `json:"webhook_url"`
}

// messageCard is the legacy Office 365 connector "MessageCard" body. The
// @type/@context fields are mandatory; themeColor is a hex string without a
// leading '#'.
type messageCard struct {
	Type       string    `json:"@type"`
	Context    string    `json:"@context"`
	ThemeColor string    `json:"themeColor,omitempty"`
	Summary    string    `json:"summary"`
	Title      string    `json:"title,omitempty"`
	Text       string    `json:"text,omitempty"`
	Sections   []section `json:"sections,omitempty"`
}

type section struct {
	ActivityTitle string `json:"activityTitle,omitempty"`
	Facts         []fact `json:"facts,omitempty"`
}

type fact struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Notifier is the Microsoft Teams incoming-webhook provider.
type Notifier struct {
	client *http.Client
}

// New builds a Teams Notifier with a timeout-bounded HTTP client.
func New() *Notifier {
	return &Notifier{client: httpx.NotifierClient(httpTimeout)}
}

// Type implements registry.Notifier.
func (*Notifier) Type() string { return "teams" }

// Validate implements registry.Notifier.
func (*Notifier) Validate(cfg json.RawMessage) error {
	c, err := parse(cfg)
	if err != nil {
		return err
	}
	if c.WebhookURL == "" {
		return errors.New("teams: webhook_url is required")
	}
	u, err := url.Parse(c.WebhookURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("teams: webhook_url %q must be an absolute http(s) URL", c.WebhookURL)
	}
	return nil
}

// Send implements registry.Notifier.
func (n *Notifier) Send(ctx context.Context, cfg json.RawMessage, event domain.NotificationEvent) error {
	c, err := parse(cfg)
	if err != nil {
		return err
	}

	summary := event.Title
	if summary == "" {
		summary = "Statwarden notification"
	}
	card := messageCard{
		Type:       "MessageCard",
		Context:    "http://schema.org/extensions",
		ThemeColor: statusColor(event),
		Summary:    summary,
		Title:      fmt.Sprintf("%s %s", statusEmoji(event), event.Title),
		Text:       event.Body,
		Sections:   []section{{Facts: buildFacts(event)}},
	}

	raw, err := json.Marshal(card)
	if err != nil {
		return fmt.Errorf("teams: marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.WebhookURL, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("teams: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Statwarden")

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("teams: POST webhook: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("teams: POST webhook: unexpected status %s", resp.Status)
	}
	return nil
}

func buildFacts(event domain.NotificationEvent) []fact {
	var facts []fact
	if event.MonitorName != "" {
		facts = append(facts, fact{Name: "Monitor", Value: event.MonitorName})
	}
	if event.Status != "" {
		facts = append(facts, fact{Name: "Status", Value: string(event.Status)})
	}
	if event.MonitorURL != "" {
		facts = append(facts, fact{Name: "URL", Value: event.MonitorURL})
	}
	if !event.At.IsZero() {
		facts = append(facts, fact{Name: "When", Value: event.At.UTC().Format(time.RFC3339)})
	}
	return facts
}

// statusEmoji picks a leading emoji for the event.
func statusEmoji(event domain.NotificationEvent) string {
	switch event.Kind {
	case domain.EventKindUp:
		return "\U0001F7E2" // green circle
	case domain.EventKindDown:
		return "\U0001F534" // red circle
	case domain.EventKindCertExpiry:
		return "\U0001F512" // lock
	case domain.EventKindMaintenanceStart, domain.EventKindMaintenanceEnd:
		return "\U0001F527" // wrench
	case domain.EventKindTest:
		return "\U0001F9EA" // test tube
	case domain.EventKindDigest:
		return "\U0001F4CA" // bar chart
	default:
		return "\U0001F514" // bell
	}
}

// statusColor is the Teams themeColor (hex without '#') for the event.
func statusColor(event domain.NotificationEvent) string {
	switch event.Kind {
	case domain.EventKindUp:
		return "2eb67d"
	case domain.EventKindDown:
		return "e01e5a"
	case domain.EventKindCertExpiry, domain.EventKindMaintenanceStart, domain.EventKindMaintenanceEnd:
		return "ecb22e"
	default:
		return "36c5f0"
	}
}

func parse(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		return c, fmt.Errorf("teams: invalid config: %w", err)
	}
	return c, nil
}
