// Package slack delivers notifications to a Slack channel through an incoming
// webhook. It follows the reference webhook provider's shape: implement
// registry.Notifier, self-register in init(), blank-import from cmd/statwarden.
package slack

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

// Config is the Slack channel's plaintext configuration (the secrets layer
// encrypts the whole blob at rest).
type Config struct {
	// WebhookURL is the Slack incoming-webhook endpoint (required, https).
	WebhookURL string `json:"webhook_url"`
	// Channel overrides the webhook's default channel (e.g. "#alerts").
	Channel string `json:"channel,omitempty"`
	// Username overrides the message's sender name.
	Username string `json:"username,omitempty"`
	// IconEmoji sets the sender icon (e.g. ":rotating_light:").
	IconEmoji string `json:"icon_emoji,omitempty"`
	// IconURL sets the sender icon from an image URL (ignored if IconEmoji is set).
	IconURL string `json:"icon_url,omitempty"`
}

// payload is the JSON body Slack incoming webhooks accept.
type payload struct {
	Channel     string       `json:"channel,omitempty"`
	Username    string       `json:"username,omitempty"`
	IconEmoji   string       `json:"icon_emoji,omitempty"`
	IconURL     string       `json:"icon_url,omitempty"`
	Text        string       `json:"text,omitempty"`
	Attachments []attachment `json:"attachments,omitempty"`
}

type attachment struct {
	Color  string  `json:"color,omitempty"`
	Title  string  `json:"title,omitempty"`
	Text   string  `json:"text,omitempty"`
	Fields []field `json:"fields,omitempty"`
	Footer string  `json:"footer,omitempty"`
	Ts     int64   `json:"ts,omitempty"`
}

type field struct {
	Title string `json:"title"`
	Value string `json:"value"`
	Short bool   `json:"short"`
}

// Notifier is the Slack incoming-webhook provider.
type Notifier struct {
	client *http.Client
}

// New builds a Slack Notifier with a timeout-bounded HTTP client.
func New() *Notifier {
	return &Notifier{client: httpx.NotifierClient(httpTimeout)}
}

// Type implements registry.Notifier.
func (*Notifier) Type() string { return "slack" }

// Validate implements registry.Notifier.
func (*Notifier) Validate(cfg json.RawMessage) error {
	c, err := parse(cfg)
	if err != nil {
		return err
	}
	if c.WebhookURL == "" {
		return errors.New("slack: webhook_url is required")
	}
	u, err := url.Parse(c.WebhookURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("slack: webhook_url %q must be an absolute http(s) URL", c.WebhookURL)
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
		Channel:   c.Channel,
		Username:  c.Username,
		IconEmoji: c.IconEmoji,
		Text:      fmt.Sprintf("%s %s", statusEmoji(event), event.Title),
		Attachments: []attachment{{
			Color:  statusColor(event),
			Title:  event.Title,
			Text:   event.Body,
			Fields: buildFields(event),
			Footer: "Statwarden",
		}},
	}
	if c.IconEmoji == "" {
		body.IconURL = c.IconURL
	}
	if !event.At.IsZero() {
		body.Attachments[0].Ts = event.At.Unix()
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("slack: marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.WebhookURL, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("slack: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Statwarden")

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("slack: POST webhook: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("slack: POST webhook: unexpected status %s", resp.Status)
	}
	return nil
}

func buildFields(event domain.NotificationEvent) []field {
	var fields []field
	if event.MonitorName != "" {
		fields = append(fields, field{Title: "Monitor", Value: event.MonitorName, Short: true})
	}
	if event.Status != "" {
		fields = append(fields, field{Title: "Status", Value: string(event.Status), Short: true})
	}
	if event.MonitorURL != "" {
		fields = append(fields, field{Title: "URL", Value: event.MonitorURL, Short: false})
	}
	return fields
}

// statusEmoji picks a leading emoji for the event.
func statusEmoji(event domain.NotificationEvent) string {
	switch event.Kind {
	case domain.EventKindUp:
		return ":large_green_circle:"
	case domain.EventKindDown:
		return ":red_circle:"
	case domain.EventKindCertExpiry:
		return ":lock:"
	case domain.EventKindMaintenanceStart, domain.EventKindMaintenanceEnd:
		return ":wrench:"
	case domain.EventKindTest:
		return ":test_tube:"
	case domain.EventKindDigest:
		return ":bar_chart:"
	default:
		return ":bell:"
	}
}

// statusColor is the Slack attachment bar colour (hex) for the event.
func statusColor(event domain.NotificationEvent) string {
	switch event.Kind {
	case domain.EventKindUp:
		return "#2eb67d"
	case domain.EventKindDown:
		return "#e01e5a"
	case domain.EventKindCertExpiry, domain.EventKindMaintenanceStart, domain.EventKindMaintenanceEnd:
		return "#ecb22e"
	default:
		return "#36c5f0"
	}
}

func parse(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		return c, fmt.Errorf("slack: invalid config: %w", err)
	}
	return c, nil
}
