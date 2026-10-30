// Package telegram delivers notifications through the Telegram Bot API's
// sendMessage method. It follows the reference webhook provider's shape:
// implement registry.Notifier, self-register in init(), blank-import from
// cmd/statwarden.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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

// defaultBaseURL is the public Telegram Bot API host. Tests point baseURL at
// an httptest server instead.
const defaultBaseURL = "https://api.telegram.org"

// Config is the Telegram channel's plaintext configuration (the secrets layer
// encrypts the whole blob at rest).
type Config struct {
	// BotToken authenticates the bot (required); of the form "<id>:<secret>".
	BotToken string `json:"bot_token"`
	// ChatID is the target chat, group or channel (required). It may be a
	// numeric id ("-1001234") or a public channel handle ("@mychannel").
	ChatID string `json:"chat_id"`
}

// sendMessageRequest is the JSON body for the Bot API sendMessage method.
type sendMessageRequest struct {
	ChatID                string `json:"chat_id"`
	Text                  string `json:"text"`
	ParseMode             string `json:"parse_mode,omitempty"`
	DisableWebPagePreview bool   `json:"disable_web_page_preview"`
}

// apiResponse is the envelope every Bot API method returns.
type apiResponse struct {
	OK          bool   `json:"ok"`
	ErrorCode   int    `json:"error_code"`
	Description string `json:"description"`
}

// Notifier is the Telegram Bot API provider.
type Notifier struct {
	client  *http.Client
	baseURL string
}

// New builds a Telegram Notifier with a timeout-bounded HTTP client pointed at
// the public Bot API.
func New() *Notifier {
	return &Notifier{client: httpx.NotifierClient(httpTimeout), baseURL: defaultBaseURL}
}

// Type implements registry.Notifier.
func (*Notifier) Type() string { return "telegram" }

// Validate implements registry.Notifier.
func (*Notifier) Validate(cfg json.RawMessage) error {
	c, err := parse(cfg)
	if err != nil {
		return err
	}
	if c.BotToken == "" {
		return errors.New("telegram: bot_token is required")
	}
	if !strings.Contains(c.BotToken, ":") {
		return errors.New("telegram: bot_token must be of the form \"<id>:<secret>\"")
	}
	if c.ChatID == "" {
		return errors.New("telegram: chat_id is required")
	}
	return nil
}

// Send implements registry.Notifier.
func (n *Notifier) Send(ctx context.Context, cfg json.RawMessage, event domain.NotificationEvent) error {
	c, err := parse(cfg)
	if err != nil {
		return err
	}

	body := sendMessageRequest{
		ChatID:                c.ChatID,
		Text:                  buildText(event),
		DisableWebPagePreview: true,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("telegram: marshal payload: %w", err)
	}

	endpoint := fmt.Sprintf("%s/bot%s/sendMessage", strings.TrimRight(n.baseURL, "/"), c.BotToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("telegram: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Statwarden")

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: sendMessage: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr apiResponse
		if json.Unmarshal(data, &apiErr) == nil && apiErr.Description != "" {
			return fmt.Errorf("telegram: sendMessage: %s (status %s)", apiErr.Description, resp.Status)
		}
		return fmt.Errorf("telegram: sendMessage: unexpected status %s", resp.Status)
	}
	var api apiResponse
	if err := json.Unmarshal(data, &api); err == nil && !api.OK {
		return fmt.Errorf("telegram: sendMessage rejected: %s", api.Description)
	}
	return nil
}

// buildText renders a plain-text message from the already-rendered event.
func buildText(event domain.NotificationEvent) string {
	var b strings.Builder
	b.WriteString(statusEmoji(event))
	b.WriteString(" ")
	b.WriteString(event.Title)
	if event.Body != "" {
		b.WriteString("\n\n")
		b.WriteString(event.Body)
	}
	if event.MonitorURL != "" {
		b.WriteString("\n")
		b.WriteString(event.MonitorURL)
	}
	return b.String()
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
		return c, fmt.Errorf("telegram: invalid config: %w", err)
	}
	return c, nil
}
