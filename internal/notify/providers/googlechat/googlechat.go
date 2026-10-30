// Package googlechat delivers notifications to a Google Chat space through an
// incoming webhook. It follows the reference slack provider's shape: implement
// registry.Notifier, self-register in init(), blank-import from cmd/statwarden.
package googlechat

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

// Config is the Google Chat channel's plaintext configuration (the secrets
// layer encrypts the whole blob at rest).
type Config struct {
	// WebhookURL is the Google Chat incoming-webhook endpoint (required,
	// https). It carries the space id plus key/token query parameters.
	WebhookURL string `json:"webhook_url"`
}

// payload is the JSON body a Google Chat webhook accepts. A cardsV2 body gives
// a titled, coloured card; text is kept as a fallback/summary line.
type payload struct {
	Text    string   `json:"text,omitempty"`
	CardsV2 []cardV2 `json:"cardsV2,omitempty"`
}

type cardV2 struct {
	CardID string `json:"cardId"`
	Card   card   `json:"card"`
}

type card struct {
	Header   *cardHeader `json:"header,omitempty"`
	Sections []section   `json:"sections,omitempty"`
}

type cardHeader struct {
	Title    string `json:"title"`
	Subtitle string `json:"subtitle,omitempty"`
}

type section struct {
	Widgets []widget `json:"widgets"`
}

type widget struct {
	DecoratedText *decoratedText `json:"decoratedText,omitempty"`
	TextParagraph *textParagraph `json:"textParagraph,omitempty"`
}

type decoratedText struct {
	TopLabel string `json:"topLabel,omitempty"`
	Text     string `json:"text"`
}

type textParagraph struct {
	Text string `json:"text"`
}

// Notifier is the Google Chat incoming-webhook provider.
type Notifier struct {
	client *http.Client
}

// New builds a Google Chat Notifier with a timeout-bounded HTTP client.
func New() *Notifier {
	return &Notifier{client: httpx.NotifierClient(httpTimeout)}
}

// Type implements registry.Notifier.
func (*Notifier) Type() string { return "googlechat" }

// Validate implements registry.Notifier.
func (*Notifier) Validate(cfg json.RawMessage) error {
	c, err := parse(cfg)
	if err != nil {
		return err
	}
	if c.WebhookURL == "" {
		return errors.New("googlechat: webhook_url is required")
	}
	u, err := url.Parse(c.WebhookURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("googlechat: webhook_url %q must be an absolute http(s) URL", c.WebhookURL)
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
		Text: fmt.Sprintf("%s %s", statusEmoji(event), event.Title),
		CardsV2: []cardV2{{
			CardID: "statwarden",
			Card: card{
				Header:   &cardHeader{Title: event.Title, Subtitle: "Statwarden"},
				Sections: buildSections(event),
			},
		}},
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("googlechat: marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.WebhookURL, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("googlechat: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("User-Agent", "Statwarden")

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("googlechat: POST webhook: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("googlechat: POST webhook: unexpected status %s", resp.Status)
	}
	return nil
}

func buildSections(event domain.NotificationEvent) []section {
	var widgets []widget
	if event.Body != "" {
		widgets = append(widgets, widget{TextParagraph: &textParagraph{Text: event.Body}})
	}
	if event.MonitorName != "" {
		widgets = append(widgets, widget{DecoratedText: &decoratedText{TopLabel: "Monitor", Text: event.MonitorName}})
	}
	if event.Status != "" {
		widgets = append(widgets, widget{DecoratedText: &decoratedText{TopLabel: "Status", Text: strings.ToUpper(string(event.Status))}})
	}
	if event.MonitorURL != "" {
		widgets = append(widgets, widget{DecoratedText: &decoratedText{TopLabel: "URL", Text: event.MonitorURL}})
	}
	if len(widgets) == 0 {
		return nil
	}
	return []section{{Widgets: widgets}}
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

func parse(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		return c, fmt.Errorf("googlechat: invalid config: %w", err)
	}
	return c, nil
}
