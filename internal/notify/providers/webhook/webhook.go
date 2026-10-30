// Package webhook is the reference notification provider: a generic HTTP
// webhook. It POSTs a small JSON document describing the event to a
// configured URL. Concrete channels (Slack, Discord, Telegram, …) are
// separate work packages (Gitea #10–#12); this one exists both as a useful
// universal adapter and as the pattern every other provider follows —
// implement registry.Notifier, self-register in init(), blank-import from
// cmd/statwarden.
package webhook

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
	"github.com/Ariazonaa/statwarden/internal/notify/registry"

	"github.com/Ariazonaa/statwarden/internal/httpx"
)

func init() { registry.Register(New()) }

// Config is the webhook channel's plaintext configuration (the secrets layer
// encrypts the whole blob at rest).
type Config struct {
	// URL is the POST target (required, http/https).
	URL string `json:"url"`
	// Method defaults to POST.
	Method string `json:"method,omitempty"`
	// Headers are added verbatim (e.g. an Authorization token).
	Headers map[string]string `json:"headers,omitempty"`
}

// payload is the JSON body sent to the webhook.
type payload struct {
	Kind        string     `json:"kind"`
	MonitorID   int64      `json:"monitor_id,omitempty"`
	MonitorName string     `json:"monitor_name,omitempty"`
	MonitorURL  string     `json:"monitor_url,omitempty"`
	Status      string     `json:"status,omitempty"`
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	Message     string     `json:"message,omitempty"`
	At          *time.Time `json:"at,omitempty"`
}

// Notifier is the generic-webhook provider.
type Notifier struct {
	client *http.Client
}

// maxRedirects bounds a webhook's redirect chain. Generous enough for the
// usual one or two canonicalisation hops, low enough that a loop is an error
// rather than a hang.
const maxRedirects = 5

// New builds a webhook Notifier with a default HTTP client. The dispatcher
// bounds each Send with its own context timeout.
func New() *Notifier {
	return &Notifier{client: &http.Client{}}
}

// Type implements registry.Notifier.
func (*Notifier) Type() string { return "webhook" }

// Validate implements registry.Notifier.
func (*Notifier) Validate(cfg json.RawMessage) error {
	c, err := parse(cfg)
	if err != nil {
		return err
	}
	if c.URL == "" {
		return errors.New("webhook: url is required")
	}
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("webhook: url %q must be an absolute http(s) URL", c.URL)
	}
	return nil
}

// Send implements registry.Notifier.
func (n *Notifier) Send(ctx context.Context, cfg json.RawMessage, event domain.NotificationEvent) error {
	c, err := parse(cfg)
	if err != nil {
		return err
	}
	method := c.Method
	if method == "" {
		method = http.MethodPost
	}
	body := payload{
		Kind:        string(event.Kind),
		MonitorID:   event.MonitorID,
		MonitorName: event.MonitorName,
		MonitorURL:  event.MonitorURL,
		Status:      string(event.Status),
		Title:       event.Title,
		Body:        event.Body,
		Message:     event.Message,
	}
	if !event.At.IsZero() {
		at := event.At.UTC()
		body.At = &at
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("webhook: marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.URL, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("webhook: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Statwarden")
	names := make([]string, 0, len(c.Headers))
	for k, v := range c.Headers {
		req.Header.Set(k, v)
		names = append(names, k)
	}

	// Custom headers on a webhook are how the receiving system authenticates
	// the call — an X-Api-Key, a shared signing token — and net/http replays
	// anything it does not recognise as sensitive to whatever host answers a
	// 3xx. A webhook URL is operator-supplied and often points at a service
	// that redirects, so the token was handed to the redirect target.
	//
	// The client is rebuilt per send because the strip list is per config, but
	// it shares the Notifier's transport, so connection pooling is unaffected.
	client := &http.Client{
		Transport:     n.client.Transport,
		Timeout:       n.client.Timeout,
		CheckRedirect: httpx.StripCredentialsOnRedirect(maxRedirects, names),
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook: POST %s: %w", c.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook: POST %s: unexpected status %s", c.URL, resp.Status)
	}
	return nil
}

func parse(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		return c, fmt.Errorf("webhook: invalid config: %w", err)
	}
	return c, nil
}
