package domain

import "time"

// NotificationEventKind classifies what a notification is about. The string
// values are stored in notification_deliveries.event_kind and are the keys of
// per-notification template overrides.
type NotificationEventKind string

// The supported notification event kinds.
const (
	// EventKindDown fires when a monitor transitions to DOWN, and again on
	// every resend-interval repeat and escalation step while it stays DOWN.
	EventKindDown NotificationEventKind = "down"
	// EventKindUp fires when a monitor recovers from DOWN.
	EventKindUp NotificationEventKind = "up"
	// EventKindCertExpiry fires when a TLS certificate crosses one of the
	// configured expiry-day thresholds (or has expired).
	EventKindCertExpiry NotificationEventKind = "cert-expiry"
	// EventKindMaintenanceStart / EventKindMaintenanceEnd inform about a
	// maintenance window opening and closing.
	EventKindMaintenanceStart NotificationEventKind = "maintenance-start"
	EventKindMaintenanceEnd   NotificationEventKind = "maintenance-end"
	// EventKindTest is the "send a test message" button.
	EventKindTest NotificationEventKind = "test"
	// EventKindDigest is a periodic batched summary; Items carries the
	// collected events.
	EventKindDigest NotificationEventKind = "digest"
	// EventKindLatencyAnomaly fires when a monitor's response time crosses into
	// a sustained anomaly while it is still answering, and
	// EventKindLatencyNormal when it returns to its baseline. Neither is a DOWN:
	// the monitor is up throughout, only slow.
	//
	// Two kinds rather than one carrying a flag, for the same reason maintenance
	// start and end are separate: a channel overrides the template per kind, and
	// providers pick their colour and severity off the kind alone.
	EventKindLatencyAnomaly NotificationEventKind = "latency-anomaly"
	EventKindLatencyNormal  NotificationEventKind = "latency-normal"
	// EventKindFlapping replaces the individual up/down alerts of a monitor that
	// changes state too often, and EventKindFlappingStable reports where it
	// settled once it holds one state again. Two kinds for the same reason as
	// the latency pair: templates and provider colours key off the kind.
	EventKindFlapping       NotificationEventKind = "flapping"
	EventKindFlappingStable NotificationEventKind = "flapping-stable"
)

// Valid reports whether k is one of the known kinds.
func (k NotificationEventKind) Valid() bool {
	switch k {
	case EventKindDown, EventKindUp, EventKindCertExpiry,
		EventKindMaintenanceStart, EventKindMaintenanceEnd,
		EventKindTest, EventKindDigest,
		EventKindLatencyAnomaly, EventKindLatencyNormal,
		EventKindFlapping, EventKindFlappingStable:
		return true
	}
	return false
}

// NotificationEvent is what the dispatcher hands to a Notifier's Send. Title
// and Body are already rendered from the message templates (including any
// per-notification override); providers that can only carry one string should
// use Body and may prepend Title.
type NotificationEvent struct {
	Kind NotificationEventKind

	// Monitor context; zero values for kinds without one (test, digest).
	MonitorID   int64
	MonitorName string
	// MonitorURL is the checked endpoint (from the monitor config) or the
	// monitor's link URL; may be empty.
	MonitorURL string

	Status  MonitorStatus
	Message string
	// Latency is the last observed check latency, when known.
	Latency *time.Duration
	// At is when the underlying event happened.
	At time.Time

	// DownSince is set on up events and down reminders: the start of the
	// outage this event refers to.
	DownSince *time.Time
	// Reminder marks resend-interval repeats of an ongoing DOWN.
	Reminder bool
	// Escalation marks the first delivery to an escalation-level channel
	// (a channel configured to be alerted only after N minutes of DOWN).
	Escalation bool

	// Certificate expiry context (EventKindCertExpiry).
	CertDaysRemaining int
	CertExpiresAt     time.Time

	// Anomaly carries the response-time numbers on EventKindLatencyAnomaly and
	// EventKindLatencyNormal; nil on every other kind. It is the same payload the
	// engine attached to the StateChange, passed through rather than flattened, so
	// the two cannot drift apart.
	Anomaly *LatencyAnomaly

	// Flapping carries the summary numbers on EventKindFlapping and
	// EventKindFlappingStable; nil on every other kind.
	Flapping *Flapping

	// Items are the batched events of an EventKindDigest summary.
	Items []NotificationEvent

	// Title and Body are the rendered message; filled in by the dispatcher
	// before Send.
	Title string
	Body  string
}
