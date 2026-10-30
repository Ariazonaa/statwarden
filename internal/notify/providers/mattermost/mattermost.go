// Package mattermost delivers notifications to a Mattermost channel through an
// incoming webhook. It follows the reference slack provider's shape: implement
// registry.Notifier, self-register in init(), blank-import from cmd/statwarden.
package mattermost

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

// Config is the Mattermost channel's plaintext configuration (the secrets
// layer encrypts the whole blob at rest).
type Config struct {
	// WebhookURL is the Mattermost incoming-webhook endpoint (required, https).
	WebhookURL string `json:"webhook_url"`
	// Channel overrides the webhook's default channel (name or @user).
	Channel string `json:"channel,omitempty"`
	// Username overrides the message's sender name.
	Username string `json:"username,omitempty"`
	// IconURL sets the sender icon from an image URL.
	IconURL string `json:"icon_url,omitempty"`
	// IconEmoji sets the sender icon (e.g. ":rotating_light:").
	IconEmoji string `json:"icon_emoji,omitempty"`
}

// payload is the JSON body Mattermost incoming webhooks accept; it is
// Slack-compatible and reuses the same attachment shape.
type payload struct {
	Channel     string       `json:"channel,omitempty"`
	Username    string       `json:"username,omitempty"`
	IconURL     string       `json:"icon_url,omitempty"`
	IconEmoji   string       `json:"icon_emoji,omitempty"`
	Text        string       `json:"text,omitempty"`
	Attachments []attachment `json:"attachments,omitempty"`
}

type attachment struct {
	Color  string  `json:"color,omitempty"`
	Title  string  `json:"title,omitempty"`
	Text   string  `json:"text,omitempty"`
	Fields []field `json:"fields,omitempty"`
	Footer string  `json:"footer,omitempty"`
}

type field struct {
	Title string `json:"title"`
	Value string `json:"value"`
	Short bool   `json:"short"`
}

// Notifier is the Mattermost incoming-webhook provider.
type Notifier struct {
	client *http.Client
}

// New builds a Mattermost Notifier with a timeout-bounded HTTP client.
func New() *Notifier {
	return &Notifier{client: httpx.NotifierClient(httpTimeout)}
}

// Type implements registry.Notifier.
func (*Notifier) Type() string { return "mattermost" }

// Validate implements registry.Notifier.
func (*Notifier) Validate(cfg json.RawMessage) error {
	c, err := parse(cfg)
	if err != nil {
		return err
	}
	if c.WebhookURL == "" {
		return errors.New("mattermost: webhook_url is required")
	}
	u, err := url.Parse(c.WebhookURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("mattermost: webhook_url %q must be an absolute http(s) URL", c.WebhookURL)
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
		IconURL:   c.IconURL,
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

	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("mattermost: marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.WebhookURL, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("mattermost: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Statwarden")

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("mattermost: POST webhook: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("mattermost: POST webhook: unexpected status %s", resp.Status)
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

// statusColor is the attachment bar colour (hex) for the event.
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
		return c, fmt.Errorf("mattermost: invalid config: %w", err)
	}
	return c, nil
}
