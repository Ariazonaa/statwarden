// Package domain holds Statwarden's shared core types. It is dependency-free on
// purpose: engine, checkers, API and persistence all speak these types without
// importing each other.
package domain

import (
	"encoding/json"
	"strings"
	"time"
)

// MonitorStatus is the lifecycle state of a monitor. The string values match
// the CHECK constraints on monitors.status / heartbeats.status in the schema.
type MonitorStatus string

// The five monitor states per docs/architecture.md.
const (
	StatusUp          MonitorStatus = "up"
	StatusDown        MonitorStatus = "down"
	StatusPending     MonitorStatus = "pending"
	StatusPaused      MonitorStatus = "paused"
	StatusMaintenance MonitorStatus = "maintenance"
)

// Valid reports whether s is one of the five known states.
func (s MonitorStatus) Valid() bool {
	switch s {
	case StatusUp, StatusDown, StatusPending, StatusPaused, StatusMaintenance:
		return true
	}
	return false
}

// Monitor is one configured check, mirroring a row of the monitors table with
// the per-second integer columns already widened to time.Duration.
type Monitor struct {
	ID   int64
	Name string
	// Type is the registry type name (e.g. "http"); the engine resolves it
	// to a Checker at run time.
	Type string
	// Config is the type-specific jsonb blob, validated by the checker's
	// Validate().
	Config json.RawMessage

	Interval time.Duration
	// Timeout limits a single check; 0 means "use the per-type default".
	Timeout time.Duration
	// MaxRetries is the number of PENDING re-checks before DOWN; 0 means a
	// single failure goes straight to DOWN.
	MaxRetries int
	// RecoveryChecks is the number of consecutive successful checks a DOWN
	// monitor needs before it counts as UP again — the counterpart of
	// MaxRetries on the way back. 0 and 1 both mean a single success recovers,
	// which is the historical behaviour; higher values stop a monitor that
	// answers once between failures from sending a recovery and a new DOWN
	// alert per bounce.
	RecoveryChecks int
	// RetryInterval is the (usually shorter) interval while PENDING, and while
	// a recovery is being confirmed.
	RetryInterval time.Duration
	// ResendInterval is how often to repeat DOWN notifications; 0 = never.
	ResendInterval time.Duration

	// UpsideDown inverts the check result: DOWN counts as OK.
	UpsideDown bool
	// Active is false for paused monitors.
	Active bool
	Status MonitorStatus

	// ParentID links to the monitor this one depends on; alerts are
	// suppressed while the parent is DOWN. Nil = no parent.
	ParentID *int64
	// GroupID files the monitor under an organisational folder. Nil = no
	// folder. It is presentation only — the engine never reads it — and is
	// unrelated to ParentID, which does carry runtime meaning.
	GroupID *int64
	// PushToken authenticates passive (push) monitors; empty otherwise.
	PushToken string

	Description string
	LinkURL     string

	// CreatedAt is the monitors row's creation timestamp. It is read-only:
	// the store fills it, nothing in the engine or the checkers writes it.
	CreatedAt time.Time
}

// Heartbeat is one recorded check result, mirroring a heartbeats row.
// Heartbeats are always recorded — suppression only ever affects alerts.
type Heartbeat struct {
	MonitorID int64
	CheckedAt time.Time
	Status    MonitorStatus
	// Latency is the measured check duration; nil when not applicable
	// (e.g. a timeout where no response ever arrived is still recorded
	// with its elapsed time, but push monitors may omit it).
	Latency *time.Duration
	Message string
	// Important marks state transitions; the transition-only event log is
	// the important=true slice of heartbeats.
	Important bool
	// Retries is the consecutive-failure count at the time of the check.
	Retries int
	// Extra carries type-specific details (e.g. resolved DNS records).
	Extra json.RawMessage
}

// CheckResult is what a Checker reports for one execution.
type CheckResult struct {
	// Status must be StatusUp or StatusDown; the engine's state machine
	// derives everything else.
	Status  MonitorStatus
	Latency time.Duration
	Message string
	// Extra carries type-specific details, stored on the heartbeat.
	Extra json.RawMessage
	// Cert is the observed TLS certificate for TLS-capable checks; nil
	// otherwise.
	Cert *CertInfo
}

// CertInfo describes the leaf certificate observed by a TLS-capable check,
// mirroring a cert_info row.
type CertInfo struct {
	Subject           string
	Issuer            string
	Serial            string
	FingerprintSHA256 string
	ValidFrom         time.Time
	ValidTo           time.Time
	DaysRemaining     int
}

// StateChange is emitted by the engine on every monitor state transition.
// Transitions drive notifications; a suppressed transition is still emitted
// (and its heartbeat recorded) so downstream consumers can log it, but must
// not alert on it.
//
// A StateChange also carries latency-anomaly events (see Anomaly): the monitor
// is still UP but has become unusually slow. Such an event has From == To ==
// StatusUp and a non-nil Anomaly, so a consumer can tell it apart from an
// up/down transition without reacting to it as one.
type StateChange struct {
	MonitorID   int64
	MonitorName string
	From        MonitorStatus
	To          MonitorStatus
	At          time.Time
	Message     string
	// Suppressed is true when alerts must not fire for this transition.
	Suppressed bool
	// SuppressReason is "parent-down" or "maintenance" when Suppressed.
	SuppressReason string
	// Anomaly is non-nil when this event reports a response-time anomaly
	// rather than an up/down transition. It is distinct from DOWN: the monitor
	// is still answering, only slowly (or has returned to its baseline speed).
	Anomaly *LatencyAnomaly
	// Flapping is non-nil when this event is a flapping summary rather than a
	// transition: the monitor changed state too often to alert on each change
	// (Ended == false), or has held one state long enough to leave that mode
	// (Ended == true). From == To == the monitor's current state.
	Flapping *Flapping
}

// SuppressFlapping is the SuppressReason of a transition held back because its
// monitor is flapping. Unlike the other reasons it does not end an outage that
// was already announced: the outage is resolved (or re-announced) once the
// flapping settles, so the channels that heard DOWN still hear the recovery.
const SuppressFlapping = "flapping"

// Flapping is the payload of a flapping summary event.
type Flapping struct {
	// Ended distinguishes the "has settled" summary (true) from the onset
	// (false).
	Ended bool
	// Changes is how many alertable state changes the summary covers: on the
	// onset, the changes inside Window that tripped the threshold; on the end,
	// the changes that were held back while flapping.
	Changes int
	// Window is the detection window the changes were counted in.
	Window time.Duration
	// Status is the state the monitor is in at the time of the summary.
	Status MonitorStatus
}

// WindowText renders Window for people: "30m" and "1h" rather than the "30m0s"
// and "1h0m0s" time.Duration prints.
func (f *Flapping) WindowText() string {
	s := f.Window.String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}

// LatencyAnomaly is the payload of a response-time anomaly event: a monitor
// whose latency ran a configured number of deviations above its rolling
// baseline (Cleared == false), or which has since returned to that baseline
// (Cleared == true).
type LatencyAnomaly struct {
	// Cleared distinguishes the recovery edge (true) from the onset edge
	// (false).
	Cleared bool
	// Latency is the observed check latency that triggered the event.
	Latency time.Duration
	// Baseline is the rolling baseline latency at the time of detection.
	Baseline time.Duration
	// Deviations is how many standard deviations above baseline Latency sits
	// on the onset edge; it may be +Inf against a zero-variance baseline.
	Deviations float64
}
