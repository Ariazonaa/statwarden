// Package webpush delivers browser Web Push notifications using VAPID-signed
// requests to a subscription's push service. It follows the reference slack
// provider's shape: implement registry.Notifier, self-register in init(),
// blank-import from cmd/statwarden.
//
// The actual encryption + delivery is done by github.com/SherClockHolmes/
// webpush-go behind an injectable send function, so tests can capture the
// rendered payload without a real push service or VAPID crypto.
package webpush

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/httpx"
	"github.com/Ariazonaa/statwarden/internal/notify/registry"
)

func init() { registry.Register(New()) }

// httpTimeout bounds a single delivery; the dispatcher also applies its own
// per-send context deadline.
const httpTimeout = 10 * time.Second

// pushTTL is how long (seconds) the push service should retain the message if
// the client is offline.
const pushTTL = 60

// Config is the Web Push channel's plaintext configuration (the secrets layer
// encrypts the whole blob at rest).
type Config struct {
	// VAPIDPublicKey is the application server's VAPID public key (required).
	VAPIDPublicKey string `json:"vapid_public_key"`
	// VAPIDPrivateKey is the application server's VAPID private key (required).
	VAPIDPrivateKey string `json:"vapid_private_key"`
	// Subject identifies the sender to the push service; a mailto: or https URL
	// (required).
	Subject string `json:"subject"`
	// Subscription is the browser's PushSubscription JSON (endpoint + keys),
	// exactly as produced by the client's PushManager (required).
	Subscription json.RawMessage `json:"subscription"`
}

// message is the JSON payload the browser service worker receives.
type message struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url,omitempty"`
	Tag   string `json:"tag,omitempty"`
}

// sendFunc performs the encrypted push. It is a package variable so tests can
// substitute a capture that needs neither VAPID crypto nor a push service.
type sendFunc func(ctx context.Context, payload []byte, sub *webpush.Subscription, opts *webpush.Options) (*http.Response, error)

// Notifier is the Web Push provider.
type Notifier struct {
	send sendFunc
}

// New builds a Web Push Notifier that delivers through webpush-go.
func New() *Notifier {
	return &Notifier{send: webpush.SendNotificationWithContext}
}

// Type implements registry.Notifier.
func (*Notifier) Type() string { return "webpush" }

// Validate implements registry.Notifier.
func (*Notifier) Validate(cfg json.RawMessage) error {
	c, err := parse(cfg)
	if err != nil {
		return err
	}
	if c.VAPIDPublicKey == "" {
		return errors.New("webpush: vapid_public_key is required")
	}
	if c.VAPIDPrivateKey == "" {
		return errors.New("webpush: vapid_private_key is required")
	}
	if c.Subject == "" {
		return errors.New("webpush: subject is required")
	}
	if !strings.HasPrefix(c.Subject, "mailto:") && !strings.HasPrefix(c.Subject, "http://") && !strings.HasPrefix(c.Subject, "https://") {
		return fmt.Errorf("webpush: subject %q must be a mailto: or http(s) URL", c.Subject)
	}
	sub, err := parseSubscription(c.Subscription)
	if err != nil {
		return err
	}
	if sub.Endpoint == "" {
		return errors.New("webpush: subscription.endpoint is required")
	}
	if sub.Keys.P256dh == "" || sub.Keys.Auth == "" {
		return errors.New("webpush: subscription.keys.p256dh and .auth are required")
	}
	return nil
}

// Send implements registry.Notifier.
func (n *Notifier) Send(ctx context.Context, cfg json.RawMessage, event domain.NotificationEvent) error {
	c, err := parse(cfg)
	if err != nil {
		return err
	}
	sub, err := parseSubscription(c.Subscription)
	if err != nil {
		return err
	}

	payload, err := json.Marshal(message{
		Title: event.Title,
		Body:  buildBody(event),
		URL:   event.MonitorURL,
		Tag:   fmt.Sprintf("statwarden-%d", event.MonitorID),
	})
	if err != nil {
		return fmt.Errorf("webpush: marshal payload: %w", err)
	}

	opts := &webpush.Options{
		HTTPClient:      httpx.NotifierClient(httpTimeout),
		Subscriber:      c.Subject,
		VAPIDPublicKey:  c.VAPIDPublicKey,
		VAPIDPrivateKey: c.VAPIDPrivateKey,
		TTL:             pushTTL,
	}

	resp, err := n.send(ctx, payload, sub, opts)
	if err != nil {
		return fmt.Errorf("webpush: send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webpush: send: unexpected status %s", resp.Status)
	}
	return nil
}

// buildBody renders the notification body from the already-rendered event.
func buildBody(event domain.NotificationEvent) string {
	if event.Body != "" {
		return event.Body
	}
	return event.Title
}

func parseSubscription(raw json.RawMessage) (*webpush.Subscription, error) {
	if len(raw) == 0 {
		return nil, errors.New("webpush: subscription is required")
	}
	var sub webpush.Subscription
	if err := json.Unmarshal(raw, &sub); err != nil {
		return nil, fmt.Errorf("webpush: invalid subscription: %w", err)
	}
	return &sub, nil
}

func parse(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		return c, fmt.Errorf("webpush: invalid config: %w", err)
	}
	return c, nil
}
