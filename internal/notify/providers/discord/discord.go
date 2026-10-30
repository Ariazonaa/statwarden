// Package discord delivers notifications to a Discord channel through a
// channel webhook. It follows the reference webhook provider's shape:
// implement registry.Notifier, self-register in init(), blank-import from
// cmd/statwarden.
package discord

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

// Config is the Discord channel's plaintext configuration (the secrets layer
// encrypts the whole blob at rest).
type Config struct {
	// WebhookURL is the Discord channel-webhook endpoint (required, https).
	WebhookURL string `json:"webhook_url"`
	// Username overrides the webhook's default sender name.
	Username string `json:"username,omitempty"`
	// AvatarURL overrides the webhook's default avatar image.
	AvatarURL string `json:"avatar_url,omitempty"`
}

// payload is the JSON body a Discord webhook accepts.
type payload struct {
	Username  string  `json:"username,omitempty"`
	AvatarURL string  `json:"avatar_url,omitempty"`
	Content   string  `json:"content,omitempty"`
	Embeds    []embed `json:"embeds,omitempty"`
}

type embed struct {
	Title       string       `json:"title,omitempty"`
	Description string       `json:"description,omitempty"`
	Color       int          `json:"color,omitempty"`
	Timestamp   string       `json:"timestamp,omitempty"`
	Fields      []embedField `json:"fields,omitempty"`
	Footer      *embedFooter `json:"footer,omitempty"`
}

type embedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

type embedFooter struct {
	Text string `json:"text"`
}

// Notifier is the Discord webhook provider.
type Notifier struct {
	client *http.Client
}

// New builds a Discord Notifier with a timeout-bounded HTTP client.
func New() *Notifier {
	return &Notifier{client: httpx.NotifierClient(httpTimeout)}
}

// Type implements registry.Notifier.
func (*Notifier) Type() string { return "discord" }

// Validate implements registry.Notifier.
func (*Notifier) Validate(cfg json.RawMessage) error {
	c, err := parse(cfg)
	if err != nil {
		return err
	}
	if c.WebhookURL == "" {
		return errors.New("discord: webhook_url is required")
	}
	u, err := url.Parse(c.WebhookURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("discord: webhook_url %q must be an absolute http(s) URL", c.WebhookURL)
	}
	return nil
}

// Send implements registry.Notifier.
func (n *Notifier) Send(ctx context.Context, cfg json.RawMessage, event domain.NotificationEvent) error {
	c, err := parse(cfg)
	if err != nil {
		return err
	}

	em := embed{
		Title:       fmt.Sprintf("%s %s", statusEmoji(event), event.Title),
		Description: event.Body,
		Color:       statusColor(event),
		Fields:      buildFields(event),
		Footer:      &embedFooter{Text: "Statwarden"},
	}
	if !event.At.IsZero() {
		em.Timestamp = event.At.UTC().Format(time.RFC3339)
	}
	body := payload{
		Username:  c.Username,
		AvatarURL: c.AvatarURL,
		Embeds:    []embed{em},
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("discord: marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.WebhookURL, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("discord: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Statwarden")

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("discord: POST webhook: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("discord: POST webhook: unexpected status %s", resp.Status)
	}
	return nil
}

func buildFields(event domain.NotificationEvent) []embedField {
	var fields []embedField
	if event.MonitorName != "" {
		fields = append(fields, embedField{Name: "Monitor", Value: event.MonitorName, Inline: true})
	}
	if event.Status != "" {
		fields = append(fields, embedField{Name: "Status", Value: string(event.Status), Inline: true})
	}
	if event.MonitorURL != "" {
		fields = append(fields, embedField{Name: "URL", Value: event.MonitorURL, Inline: false})
	}
	return fields
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

// statusColor is the Discord embed colour (24-bit int) for the event.
func statusColor(event domain.NotificationEvent) int {
	switch event.Kind {
	case domain.EventKindUp:
		return 0x2ECC71 // green
	case domain.EventKindDown:
		return 0xE74C3C // red
	case domain.EventKindCertExpiry, domain.EventKindMaintenanceStart, domain.EventKindMaintenanceEnd:
		return 0xF1C40F // amber
	default:
		return 0x3498DB // blue
	}
}

func parse(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		return c, fmt.Errorf("discord: invalid config: %w", err)
	}
	return c, nil
}
