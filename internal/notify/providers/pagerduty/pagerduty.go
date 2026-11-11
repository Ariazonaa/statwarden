// Package pagerduty delivers notifications through the PagerDuty Events API v2.
// A problem event triggers an incident and the matching "back to normal" event
// resolves it, keyed by a stable dedup_key per monitor and event class (see
// incidentFor). It follows the
// reference webhook provider's shape: implement registry.Notifier,
// self-register in init(), blank-import from cmd/statwarden.
package pagerduty

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

// defaultBaseURL is the public Events API host. Tests point baseURL at an
// httptest server instead.
const defaultBaseURL = "https://events.pagerduty.com"

// Config is the PagerDuty channel's plaintext configuration (the secrets layer
// encrypts the whole blob at rest).
type Config struct {
	// RoutingKey is the integration/routing key of a PagerDuty Events API v2
	// service (required, 32 chars).
	RoutingKey string `json:"routing_key"`
	// Source overrides the incident source (defaults to the monitor name/URL).
	Source string `json:"source,omitempty"`
}

// event is the Events API v2 request envelope. For a resolve action only
// routing_key, event_action and dedup_key are required.
type event struct {
	RoutingKey  string        `json:"routing_key"`
	EventAction string        `json:"event_action"`
	DedupKey    string        `json:"dedup_key,omitempty"`
	Payload     *eventPayload `json:"payload,omitempty"`
}

type eventPayload struct {
	Summary       string         `json:"summary"`
	Source        string         `json:"source"`
	Severity      string         `json:"severity"`
	Timestamp     string         `json:"timestamp,omitempty"`
	Component     string         `json:"component,omitempty"`
	CustomDetails map[string]any `json:"custom_details,omitempty"`
}

// apiResponse is the Events API v2 envelope.
type apiResponse struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

// Notifier is the PagerDuty Events API v2 provider.
type Notifier struct {
	client  *http.Client
	baseURL string
}

// New builds a PagerDuty Notifier with a timeout-bounded HTTP client pointed at
// the public Events API.
func New() *Notifier {
	return &Notifier{client: httpx.NotifierClient(httpTimeout), baseURL: defaultBaseURL}
}

// Type implements registry.Notifier.
func (*Notifier) Type() string { return "pagerduty" }

// Validate implements registry.Notifier.
func (*Notifier) Validate(cfg json.RawMessage) error {
	c, err := parse(cfg)
	if err != nil {
		return err
	}
	if c.RoutingKey == "" {
		return errors.New("pagerduty: routing_key is required")
	}
	return nil
}

// Send implements registry.Notifier.
func (n *Notifier) Send(ctx context.Context, cfg json.RawMessage, ev domain.NotificationEvent) error {
	c, err := parse(cfg)
	if err != nil {
		return err
	}

	key, action := incidentFor(ev)
	if err := n.enqueue(ctx, c, ev, key, action); err != nil {
		return err
	}
	if ev.Kind == domain.EventKindTest {
		// A test proves the routing key works; it is not an incident anybody
		// should have to close by hand.
		return n.enqueue(ctx, c, ev, key, "resolve")
	}
	return nil
}

// enqueue sends one Events API v2 event.
func (n *Notifier) enqueue(ctx context.Context, c Config, ev domain.NotificationEvent, key, action string) error {
	body := event{
		RoutingKey:  c.RoutingKey,
		EventAction: action,
		DedupKey:    key,
	}
	// A resolve carries no payload; a trigger describes the incident.
	if body.EventAction == "trigger" {
		body.Payload = &eventPayload{
			Summary:       summary(ev),
			Source:        source(c, ev),
			Severity:      severity(ev),
			Component:     ev.MonitorName,
			CustomDetails: customDetails(ev),
		}
		if !ev.At.IsZero() {
			body.Payload.Timestamp = ev.At.UTC().Format(time.RFC3339)
		}
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("pagerduty: marshal payload: %w", err)
	}

	endpoint := strings.TrimRight(n.baseURL, "/") + "/v2/enqueue"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("pagerduty: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Statwarden")

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("pagerduty: enqueue: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr apiResponse
		if json.Unmarshal(data, &apiErr) == nil && apiErr.Message != "" {
			return fmt.Errorf("pagerduty: enqueue: %s (status %s)", apiErr.Message, resp.Status)
		}
		return fmt.Errorf("pagerduty: enqueue: unexpected status %s", resp.Status)
	}
	return nil
}

// incidentFor returns the dedup_key and event_action for an event.
//
// Each class of problem — an outage, slow responses, flapping, a certificate,
// a maintenance window — is its own incident with its own key, and the event
// that says the problem is over resolves exactly that key. They all used to
// share statwarden-monitor-<id> with only an UP resolving it, so a certificate
// warning opened (or joined) the outage incident and anything other than an
// outage was never closed — while an UP closed whatever happened to be open.
//
// The outage keeps the bare key, so an incident opened before this change is
// still resolved by the next UP. Resend reminders and escalations are DOWN
// events and fold into the open outage incident, which is the point of a
// dedup key. A certificate warning has no "renewed" event and stays open until
// somebody acts on it, as it should. A test or digest has no monitor to key on
// and gets a key of its own per send.
func incidentFor(ev domain.NotificationEvent) (key, action string) {
	monitor := fmt.Sprintf("statwarden-monitor-%d", ev.MonitorID)
	switch ev.Kind {
	case domain.EventKindDown:
		return monitor, "trigger"
	case domain.EventKindUp:
		return monitor, "resolve"
	case domain.EventKindLatencyAnomaly:
		return monitor + "-latency", "trigger"
	case domain.EventKindLatencyNormal:
		return monitor + "-latency", "resolve"
	case domain.EventKindFlapping:
		return monitor + "-flapping", "trigger"
	case domain.EventKindFlappingStable:
		return monitor + "-flapping", "resolve"
	case domain.EventKindCertExpiry:
		return monitor + "-cert", "trigger"
	case domain.EventKindMaintenanceStart:
		return monitor + "-maintenance", "trigger"
	case domain.EventKindMaintenanceEnd:
		return monitor + "-maintenance", "resolve"
	default:
		return fmt.Sprintf("statwarden-%s-%d", ev.Kind, sendInstant(ev).UnixNano()), "trigger"
	}
}

// sendInstant is the event's timestamp, or now for one that carries none.
func sendInstant(ev domain.NotificationEvent) time.Time {
	if ev.At.IsZero() {
		return time.Now()
	}
	return ev.At
}

func summary(ev domain.NotificationEvent) string {
	if ev.Title != "" {
		return ev.Title
	}
	if ev.Body != "" {
		return ev.Body
	}
	return "Statwarden notification"
}

func source(c Config, ev domain.NotificationEvent) string {
	switch {
	case c.Source != "":
		return c.Source
	case ev.MonitorURL != "":
		return ev.MonitorURL
	case ev.MonitorName != "":
		return ev.MonitorName
	default:
		return "statwarden"
	}
}

// severity maps the event kind onto the Events API v2 severity scale
// (critical, error, warning, info).
func severity(ev domain.NotificationEvent) string {
	switch ev.Kind {
	case domain.EventKindDown:
		return "critical"
	case domain.EventKindCertExpiry, domain.EventKindLatencyAnomaly, domain.EventKindFlapping:
		return "warning"
	case domain.EventKindMaintenanceStart, domain.EventKindMaintenanceEnd:
		return "info"
	default:
		return "info"
	}
}

func customDetails(ev domain.NotificationEvent) map[string]any {
	details := map[string]any{}
	if ev.Body != "" {
		details["body"] = ev.Body
	}
	if ev.MonitorName != "" {
		details["monitor"] = ev.MonitorName
	}
	if ev.MonitorURL != "" {
		details["url"] = ev.MonitorURL
	}
	if ev.Status != "" {
		details["status"] = string(ev.Status)
	}
	if len(details) == 0 {
		return nil
	}
	return details
}

func parse(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		return c, fmt.Errorf("pagerduty: invalid config: %w", err)
	}
	return c, nil
}
