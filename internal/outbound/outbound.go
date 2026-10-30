// Package outbound delivers a generic JSON webhook on every monitor state
// change. It is distinct from the notification providers: a single
// settings-configured URL receives a machine-readable POST on each transition,
// intended for automation platforms (n8n, Node-RED, …) rather than humans.
//
// The queueing, ordering, shutdown-drain and panic containment all live in
// internal/webhookq, which internal/siem shares. What is left here is the
// payload and the engine-facing entry point.
package outbound

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/webhookq"
)

// Payload is the JSON body POSTed on each state change.
type Payload struct {
	Event       string    `json:"event"` // always "state_change"
	MonitorID   int64     `json:"monitor_id"`
	MonitorName string    `json:"monitor_name"`
	From        string    `json:"from"`
	To          string    `json:"to"`
	Status      string    `json:"status"` // == To, convenience alias
	Message     string    `json:"message,omitempty"`
	At          time.Time `json:"at"`
	Suppressed  bool      `json:"suppressed"`
}

// LogAttrs names the payload in the queue's drop and failure logs.
func (p Payload) LogAttrs() []any {
	return []any{"monitor_id", p.MonitorID, "to", p.To}
}

// PayloadFor builds the webhook payload for a state change.
func PayloadFor(sc domain.StateChange) Payload {
	return Payload{
		Event:       "state_change",
		MonitorID:   sc.MonitorID,
		MonitorName: sc.MonitorName,
		From:        string(sc.From),
		To:          string(sc.To),
		Status:      string(sc.To),
		Message:     sc.Message,
		At:          sc.At,
		Suppressed:  sc.Suppressed,
	}
}

// URLProvider yields the currently configured outbound webhook URL (read from
// settings on each call so a live settings change takes effect immediately). An
// empty string disables delivery.
type URLProvider = webhookq.URLProvider

// Webhook posts state-change payloads to the configured URL.
//
// Delivery happens on its own goroutine. HandleStateChange is called from
// engine worker goroutines, whose contract (engine.Options.OnEvent) is to
// return immediately, and a POST to an operator-supplied URL is the least
// bounded thing in the process: a target that accepts the connection and then
// says nothing holds the caller for the full timeout. Doing that inline meant
// every state transition could park a checker worker for ten seconds — and a
// fleet-wide incident produces many transitions at once, which is precisely
// when there are no workers to spare.
type Webhook struct {
	q *webhookq.Queue[Payload]
}

// Options configures a Webhook.
type Options struct {
	URL URLProvider
	// Secret yields the HMAC signing secret; empty or nil sends unsigned.
	Secret  webhookq.SecretProvider
	Client  *http.Client
	Timeout time.Duration
	Logger  *slog.Logger
}

// New builds a Webhook.
func New(o Options) *Webhook {
	return &Webhook{q: webhookq.New[Payload](webhookq.Options{
		Name:      "outbound webhook",
		URL:       o.URL,
		Secret:    o.Secret,
		Client:    o.Client,
		Timeout:   o.Timeout,
		Logger:    o.Logger,
		UserAgent: "Statwarden-Webhook/1",
	})}
}

// Run delivers queued payloads until ctx ends. It is started lazily by the
// first HandleStateChange, so a caller that never wires it up still works;
// starting it explicitly is only needed to control its lifetime.
func (h *Webhook) Run(ctx context.Context) { h.q.Run(ctx) }

// Wait blocks until the delivery goroutine has finished its shutdown drain, or
// until ctx expires. See webhookq.Queue.Wait: the drain needs the database, so
// whoever closes the pool has to wait for it first.
func (h *Webhook) Wait(ctx context.Context) error { return h.q.Wait(ctx) }

// HandleStateChange queues one transition for delivery. It is safe to pass
// every event (including suppressed ones), it never blocks, and it is a no-op
// when no URL is configured.
//
// The URL is re-read at delivery time rather than here, so a settings change
// still takes effect immediately and the caller does not pay for the lookup.
func (h *Webhook) HandleStateChange(sc domain.StateChange) {
	h.q.Enqueue(PayloadFor(sc))
}

// deliver POSTs one payload synchronously.
func (h *Webhook) deliver(ctx context.Context, url string, p Payload) error {
	return h.q.Deliver(ctx, url, "", p)
}
