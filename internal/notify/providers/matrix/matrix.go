// Package matrix delivers notifications into a Matrix room through the
// client-server send API. It follows the reference slack provider's shape:
// implement registry.Notifier, self-register in init(), blank-import from
// cmd/statwarden.
package matrix

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/httpx"
	"github.com/Ariazonaa/statwarden/internal/notify/registry"
)

func init() { registry.Register(New()) }

// httpTimeout bounds a single delivery; the dispatcher also applies its own
// per-send context deadline.
const httpTimeout = 10 * time.Second

// txnCounter makes the per-send transaction id unique within a process so
// Matrix does not deduplicate distinct messages.
var txnCounter atomic.Uint64

// Config is the Matrix channel's plaintext configuration (the secrets layer
// encrypts the whole blob at rest).
type Config struct {
	// HomeserverURL is the base URL of the Matrix homeserver (required, http(s)).
	HomeserverURL string `json:"homeserver_url"`
	// AccessToken authenticates the sending user/bot (required).
	AccessToken string `json:"access_token"`
	// RoomID is the target room, e.g. "!abcdef:example.org" (required).
	RoomID string `json:"room_id"`
}

// payload is the m.room.message event content sent to the room. HTML is sent
// alongside the plain-text body so clients that support formatting render it.
type payload struct {
	MsgType       string `json:"msgtype"`
	Body          string `json:"body"`
	Format        string `json:"format,omitempty"`
	FormattedBody string `json:"formatted_body,omitempty"`
}

// Notifier is the Matrix client-server API provider.
type Notifier struct {
	client *http.Client
}

// New builds a Matrix Notifier with a timeout-bounded HTTP client.
func New() *Notifier {
	return &Notifier{client: httpx.NotifierClient(httpTimeout)}
}

// Type implements registry.Notifier.
func (*Notifier) Type() string { return "matrix" }

// Validate implements registry.Notifier.
func (*Notifier) Validate(cfg json.RawMessage) error {
	c, err := parse(cfg)
	if err != nil {
		return err
	}
	if c.HomeserverURL == "" {
		return errors.New("matrix: homeserver_url is required")
	}
	u, err := url.Parse(c.HomeserverURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("matrix: homeserver_url %q must be an absolute http(s) URL", c.HomeserverURL)
	}
	if c.AccessToken == "" {
		return errors.New("matrix: access_token is required")
	}
	if c.RoomID == "" {
		return errors.New("matrix: room_id is required")
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
		MsgType:       "m.text",
		Body:          buildBody(event),
		Format:        "org.matrix.custom.html",
		FormattedBody: buildHTML(event),
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("matrix: marshal payload: %w", err)
	}

	txn := strconv.FormatInt(time.Now().UnixNano(), 10) + "-" + strconv.FormatUint(txnCounter.Add(1), 10)
	endpoint := fmt.Sprintf("%s/_matrix/client/v3/rooms/%s/send/m.room.message/%s",
		strings.TrimRight(c.HomeserverURL, "/"),
		url.PathEscape(c.RoomID),
		url.PathEscape(txn),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("matrix: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	req.Header.Set("User-Agent", "Statwarden")

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("matrix: PUT send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("matrix: PUT send: unexpected status %s", resp.Status)
	}
	return nil
}

// buildBody renders the plain-text fallback body.
func buildBody(event domain.NotificationEvent) string {
	var b strings.Builder
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

// buildHTML renders a lightly formatted HTML body.
func buildHTML(event domain.NotificationEvent) string {
	var b strings.Builder
	b.WriteString("<strong>")
	b.WriteString(htmlEscape(event.Title))
	b.WriteString("</strong>")
	if event.Body != "" {
		b.WriteString("<br/>")
		b.WriteString(htmlEscape(event.Body))
	}
	if event.MonitorURL != "" {
		b.WriteString("<br/>")
		b.WriteString(htmlEscape(event.MonitorURL))
	}
	return b.String()
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

func parse(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		return c, fmt.Errorf("matrix: invalid config: %w", err)
	}
	return c, nil
}
