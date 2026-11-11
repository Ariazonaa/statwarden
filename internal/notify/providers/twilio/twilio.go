// Package twilio delivers notifications as SMS messages through the Twilio
// REST API. It follows the reference slack provider's shape: implement
// registry.Notifier, self-register in init(), blank-import from cmd/statwarden.
package twilio

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/httpx"
	"github.com/Ariazonaa/statwarden/internal/notify/registry"
)

func init() { registry.Register(New()) }

// httpTimeout bounds a single delivery; the dispatcher also applies its own
// per-send context deadline.
const httpTimeout = 10 * time.Second

// defaultBaseURL is the public Twilio REST API host. Tests point baseURL at an
// httptest server instead.
const defaultBaseURL = "https://api.twilio.com"

// maxSMSLen keeps the message within a sane length for SMS delivery; longer
// bodies are truncated with an ellipsis.
const maxSMSLen = 1500

// Config is the Twilio channel's plaintext configuration (the secrets layer
// encrypts the whole blob at rest).
type Config struct {
	// AccountSID is the Twilio account identifier (required, "AC...").
	AccountSID string `json:"account_sid"`
	// AuthToken is the Twilio auth token used for HTTP basic auth (required).
	AuthToken string `json:"auth_token"`
	// From is the sending phone number or messaging-service sender (required).
	From string `json:"from"`
	// To are the destination phone numbers in E.164 form (required, at least one).
	To []string `json:"to"`
}

// Notifier is the Twilio SMS provider.
type Notifier struct {
	client  *http.Client
	baseURL string
	sent    *sentLog
}

// New builds a Twilio Notifier with a timeout-bounded HTTP client pointed at
// the public REST API.
func New() *Notifier {
	return &Notifier{client: httpx.NotifierClient(httpTimeout), baseURL: defaultBaseURL, sent: newSentLog()}
}

// Type implements registry.Notifier.
func (*Notifier) Type() string { return "twilio" }

// Validate implements registry.Notifier.
func (*Notifier) Validate(cfg json.RawMessage) error {
	c, err := parse(cfg)
	if err != nil {
		return err
	}
	if c.AccountSID == "" {
		return errors.New("twilio: account_sid is required")
	}
	if c.AuthToken == "" {
		return errors.New("twilio: auth_token is required")
	}
	if c.From == "" {
		return errors.New("twilio: from is required")
	}
	if len(c.To) == 0 {
		return errors.New("twilio: at least one recipient in to is required")
	}
	for i, to := range c.To {
		if strings.TrimSpace(to) == "" {
			return fmt.Errorf("twilio: to[%d] is empty", i)
		}
	}
	return nil
}

// Send implements registry.Notifier. Twilio sends one message per recipient,
// so a delivery is a POST per destination.
//
// Every recipient is tried. Returning on the first failure meant one mistyped
// number at the top of the list silenced everyone below it. The failures are
// joined into the returned error, so the delivery still counts as failed and is
// retried — and a retry skips the recipients this alert already reached (see
// sentLog), because Twilio has no idempotency key that would let the API
// discard the duplicate for us.
func (n *Notifier) Send(ctx context.Context, cfg json.RawMessage, event domain.NotificationEvent) error {
	c, err := parse(cfg)
	if err != nil {
		return err
	}

	endpoint := fmt.Sprintf("%s/2010-04-01/Accounts/%s/Messages.json",
		strings.TrimRight(n.baseURL, "/"), url.PathEscape(c.AccountSID))
	message := buildMessage(event)

	var errs []error
	for _, to := range c.To {
		key := sentKey(c, to, message, event)
		if n.sent.has(key) {
			continue
		}
		if err := n.sendOne(ctx, endpoint, c, to, message); err != nil {
			errs = append(errs, err)
			continue
		}
		n.sent.add(key)
	}
	return errors.Join(errs...)
}

// sentKey identifies one alert to one recipient: the same event re-sent by a
// retry produces the same key, the next alert for the monitor does not.
func sentKey(c Config, to, message string, ev domain.NotificationEvent) string {
	h := sha256.New()
	for _, part := range []string{
		c.AccountSID, c.From, to, message, string(ev.Kind),
		strconv.FormatInt(ev.MonitorID, 10), strconv.FormatInt(ev.At.UnixNano(), 10),
	} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// sentTTL is how long a delivered alert is remembered. It covers the
// dispatcher's in-process retries and the outbox's re-runs of a failed event,
// which stop well inside an hour; after that a repeat is a new alert.
const sentTTL = time.Hour

// sentCap bounds the log. Reached only by a very large recipient list under a
// storm of alerts; forgetting early then costs at most a duplicate SMS.
const sentCap = 10000

// sentLog remembers which recipient has already been sent which alert. It is
// in memory on purpose: a duplicate after a restart is a nuisance, while a
// skipped alert would be the failure the whole provider exists to prevent.
type sentLog struct {
	mu  sync.Mutex
	now func() time.Time
	at  map[string]time.Time
}

func newSentLog() *sentLog {
	return &sentLog{now: time.Now, at: map[string]time.Time{}}
}

func (s *sentLog) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.at[key]
	return ok && s.now().Sub(at) < sentTTL
}

func (s *sentLog) add(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if len(s.at) >= sentCap {
		for k, at := range s.at {
			if now.Sub(at) >= sentTTL {
				delete(s.at, k)
			}
		}
		if len(s.at) >= sentCap {
			s.at = map[string]time.Time{}
		}
	}
	s.at[key] = now
}

func (n *Notifier) sendOne(ctx context.Context, endpoint string, c Config, to, message string) error {
	form := url.Values{}
	form.Set("From", c.From)
	form.Set("To", to)
	form.Set("Body", message)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("twilio: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Statwarden")
	req.SetBasicAuth(c.AccountSID, c.AuthToken)

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("twilio: POST message: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &apiErr) == nil && apiErr.Message != "" {
			return fmt.Errorf("twilio: POST message to %s: %s (status %s)", to, apiErr.Message, resp.Status)
		}
		return fmt.Errorf("twilio: POST message to %s: unexpected status %s", to, resp.Status)
	}
	return nil
}

// buildMessage renders a plain-text SMS body from the already-rendered event.
func buildMessage(event domain.NotificationEvent) string {
	var b strings.Builder
	b.WriteString(event.Title)
	if event.Body != "" {
		b.WriteString("\n")
		b.WriteString(event.Body)
	}
	// Counted in characters: a byte offset can land inside a multi-byte
	// character and send invalid UTF-8.
	if runes := []rune(b.String()); len(runes) > maxSMSLen {
		return string(runes[:maxSMSLen-1]) + "…"
	}
	return b.String()
}

func parse(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		return c, fmt.Errorf("twilio: invalid config: %w", err)
	}
	return c, nil
}
