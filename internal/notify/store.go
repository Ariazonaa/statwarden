package notify

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
)

// Notification is one configured channel, mirroring a notifications row.
// Config is plaintext JSON at this layer — the persistence boundary
// (PgStore + secrets.Box) handles decryption.
type Notification struct {
	ID     int64
	Name   string
	Type   string
	Config json.RawMessage
	// IsDefault marks channels that are auto-attached to new monitors.
	IsDefault bool
	Active    bool
	// Templates are the per-event-kind message overrides, keyed by
	// NotificationEventKind string.
	Templates map[string]MessageTemplate
}

// Assignment is one monitor↔channel link (monitor_notifications row).
type Assignment struct {
	Notification Notification
	// EscalationAfter is the escalation delay: 0 = alert immediately,
	// > 0 = additionally alert this channel once the monitor has been
	// DOWN for that long.
	EscalationAfter time.Duration
}

// MonitorSettings is the slice of a monitor row the dispatcher needs.
type MonitorSettings struct {
	Name string
	// URL is the checked endpoint (config "url" field) or the monitor's
	// link URL; may be empty.
	URL string
	// ResendInterval repeats DOWN notifications while the outage lasts;
	// 0 = never resend.
	ResendInterval time.Duration
}

// Outage is one monitor that is DOWN right now, with the instant its outage
// began. It exists so a restarted dispatcher can pick up outages that started
// before this process did — see Dispatcher.Restore.
type Outage struct {
	MonitorID int64
	// Since is when the monitor last transitioned into DOWN. It falls back to
	// the read time when no transition heartbeat survives retention, which only
	// makes the reported outage duration shorter, never wrong in kind.
	Since time.Time
}

// DeliveryStatus is the outcome of one delivery attempt series.
type DeliveryStatus string

// The delivery outcomes recorded in the audit log.
const (
	DeliverySuccess DeliveryStatus = "success"
	DeliveryFailed  DeliveryStatus = "failed"
)

// Delivery is one audit-log entry: an event × channel send with its outcome.
type Delivery struct {
	NotificationID int64
	// MonitorID is 0 for events without a monitor (test sends, digests).
	MonitorID    int64
	ProviderType string
	EventKind    domain.NotificationEventKind
	Status       DeliveryStatus
	Error        string
	// Attempts is how many sends were tried (retries included).
	Attempts   int
	StartedAt  time.Time
	FinishedAt time.Time
}

// Store is the dispatcher's persistence surface. PgStore implements it
// against PostgreSQL; MemStore backs tests and database-less DEV_MODE runs.
type Store interface {
	// Assignments returns the active channels assigned to a monitor,
	// ordered by escalation delay.
	Assignments(ctx context.Context, monitorID int64) ([]Assignment, error)
	// MonitorSettings returns the notification-relevant monitor fields.
	MonitorSettings(ctx context.Context, monitorID int64) (MonitorSettings, error)
	// RecordDelivery appends one entry to the delivery audit log.
	RecordDelivery(ctx context.Context, d Delivery) error
	// OpenOutages returns every active monitor currently in DOWN, with the
	// instant its outage began. Dispatcher.Restore reads it once at startup.
	OpenOutages(ctx context.Context) ([]Outage, error)
	// ChannelsAwaitingRecovery returns the notification IDs whose most recent
	// successful delivery for a monitor was a DOWN — that is, the channels that
	// were told about an outage and have not yet been told it ended.
	//
	// Restore needs it because the in-memory record of who was alerted dies
	// with the process. Guessing from escalation delays instead invents
	// recipients: a channel added during the outage, or one whose provider was
	// failing, gets a "back online" for something it never heard about. The
	// delivery audit log is the only place that knows what actually went out.
	ChannelsAwaitingRecovery(ctx context.Context, monitorID int64) ([]int64, error)

	// --- durable hand-off (see migration 0014) ---

	// EnqueueOutbox records an accepted event before it is delivered and
	// returns its row id. Everything after this point can fail — a full queue,
	// a crash, a provider outage — without the event being lost.
	//
	// lease is how long the caller claims its own row for. A row is never
	// inserted unleased: an unleased row is immediately due, so the sweeper
	// could pick up an event the caller is still holding and deliver it twice.
	EnqueueOutbox(ctx context.Context, rec OutboxRecord, lease time.Duration) (int64, error)
	// CompleteOutbox removes a row once its event has been processed.
	CompleteOutbox(ctx context.Context, id int64) error
	// ClaimOutbox leases up to limit rows whose claim has lapsed, incrementing
	// their attempt count, and returns them. The lease is what makes a crash
	// mid-delivery recoverable: the row simply becomes due again.
	ClaimOutbox(ctx context.Context, lease time.Duration, limit int) ([]OutboxItem, error)
	// DropExhaustedOutbox deletes rows that have been claimed maxAttempts times
	// and returns them, so the caller can say out loud which alerts it is
	// giving up on rather than deleting them quietly.
	DropExhaustedOutbox(ctx context.Context, maxAttempts int) ([]OutboxItem, error)
}

// OutboxKind discriminates an outbox payload. Every event the dispatcher can
// be handed is recorded, not only state changes: a certificate-expiry warning
// or a maintenance notice dropped by a full queue is just as lost as a DOWN.
type OutboxKind string

// The recorded event kinds: one per Dispatcher.Handle* entry point, plus
// escalation, which has no entry point of its own because it is raised by a
// timer rather than handed in from outside. It is recorded all the same — an
// escalation is the alert that reaches the on-call channel, so it is the last
// one that should be allowed to vanish on a transient provider failure.
const (
	OutboxStateChange OutboxKind = "state_change"
	OutboxCertExpiry  OutboxKind = "cert_expiry"
	OutboxMaintenance OutboxKind = "maintenance"
	OutboxEscalation  OutboxKind = "escalation"
)

// OutboxRecord is an event on its way into the outbox.
type OutboxRecord struct {
	Kind OutboxKind
	// MonitorID is 0 for events that carry no monitor; it is stored so a
	// deleted monitor takes its pending alerts with it.
	MonitorID int64
	Payload   json.RawMessage
}

// OutboxItem is one accepted event that has not finished processing.
type OutboxItem struct {
	ID   int64
	Kind OutboxKind
	// MonitorID is denormalised out of Payload purely so logs about a row can
	// name the monitor without decoding it.
	MonitorID int64
	Payload   json.RawMessage
	// Attempts is how often this row has been CLAIMED, which is incremented on
	// take rather than on failure so a handler that panics still advances it.
	Attempts int
}

// MemStore is an in-memory Store for tests and DEV_MODE without a database.
// All methods are safe for concurrent use.
type MemStore struct {
	mu          sync.Mutex
	assignments map[int64][]Assignment
	settings    map[int64]MonitorSettings
	deliveries  []Delivery
	outages     []Outage
	outbox      []outboxRow
	outboxSeq   int64
	outboxClock func() time.Time
}

// outboxRow is a stored event plus its lease, mirroring the columns of
// notification_outbox.
type outboxRow struct {
	item         OutboxItem
	claimedUntil time.Time
}

// NewMemStore returns an empty MemStore.
func NewMemStore() *MemStore {
	return &MemStore{
		assignments: make(map[int64][]Assignment),
		settings:    make(map[int64]MonitorSettings),
	}
}

// SetAssignments replaces a monitor's channel assignments.
func (s *MemStore) SetAssignments(monitorID int64, assignments ...Assignment) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.assignments[monitorID] = append([]Assignment(nil), assignments...)
}

// SetMonitorSettings replaces a monitor's notification settings.
func (s *MemStore) SetMonitorSettings(monitorID int64, ms MonitorSettings) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settings[monitorID] = ms
}

// Assignments implements Store.
func (s *MemStore) Assignments(_ context.Context, monitorID int64) ([]Assignment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Assignment(nil), s.assignments[monitorID]...), nil
}

// MonitorSettings implements Store.
func (s *MemStore) MonitorSettings(_ context.Context, monitorID int64) (MonitorSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settings[monitorID], nil
}

// RecordDelivery implements Store.
func (s *MemStore) RecordDelivery(_ context.Context, d Delivery) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deliveries = append(s.deliveries, d)
	return nil
}

// SetOpenOutages replaces the outages OpenOutages reports (test helper).
func (s *MemStore) SetOpenOutages(outages ...Outage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outages = append([]Outage(nil), outages...)
}

// OpenOutages implements Store.
func (s *MemStore) OpenOutages(context.Context) ([]Outage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Outage(nil), s.outages...), nil
}

// ChannelsAwaitingRecovery implements Store, mirroring PgStore: the most recent
// successful delivery per channel, kept when it was a DOWN.
func (s *MemStore) ChannelsAwaitingRecovery(_ context.Context, monitorID int64) ([]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	latest := map[int64]domain.NotificationEventKind{}
	for _, d := range s.deliveries { // append-only, so later entries are newer
		if d.MonitorID != monitorID || d.NotificationID == 0 || d.Status != DeliverySuccess {
			continue
		}
		if d.EventKind == domain.EventKindDown || d.EventKind == domain.EventKindUp {
			latest[d.NotificationID] = d.EventKind
		}
	}
	var out []int64
	for id, kind := range latest {
		if kind == domain.EventKindDown {
			out = append(out, id)
		}
	}
	slices.Sort(out) // deterministic for tests
	return out, nil
}

// Deliveries returns a copy of the recorded audit log.
func (s *MemStore) Deliveries() []Delivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Delivery(nil), s.deliveries...)
}

// ParseTemplateOverrides decodes the notifications.templates jsonb column
// into the per-kind override map. Broken JSON is logged and treated as "no
// overrides" — a bad override must never lose an alert.
func ParseTemplateOverrides(raw json.RawMessage, logger *slog.Logger) map[string]MessageTemplate {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]MessageTemplate
	if err := json.Unmarshal(raw, &m); err != nil {
		if logger != nil {
			logger.Warn("invalid notification templates column, ignoring overrides", "error", err)
		}
		return nil
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// --- durable hand-off (MemStore) ---

// EnqueueOutbox implements Store.
func (s *MemStore) EnqueueOutbox(_ context.Context, rec OutboxRecord, lease time.Duration) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outboxSeq++
	s.outbox = append(s.outbox, outboxRow{
		item: OutboxItem{
			ID:        s.outboxSeq,
			Kind:      rec.Kind,
			MonitorID: rec.MonitorID,
			Payload:   rec.Payload,
		},
		// Claimed by the caller on insert, exactly as PgStore does it, so a
		// test cannot pass on a race the real store does not have.
		claimedUntil: s.outboxNow().Add(lease),
	})
	return s.outboxSeq, nil
}

// CompleteOutbox implements Store.
func (s *MemStore) CompleteOutbox(_ context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, r := range s.outbox {
		if r.item.ID == id {
			s.outbox = append(s.outbox[:i], s.outbox[i+1:]...)
			return nil
		}
	}
	return nil
}

// ClaimOutbox implements Store, mirroring PgStore: rows whose lease has lapsed,
// oldest first, with the attempt count incremented on take.
func (s *MemStore) ClaimOutbox(_ context.Context, lease time.Duration, limit int) ([]OutboxItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.outboxNow()
	var out []OutboxItem
	for i := range s.outbox {
		if len(out) >= limit {
			break
		}
		if s.outbox[i].claimedUntil.After(now) {
			continue
		}
		s.outbox[i].item.Attempts++
		s.outbox[i].claimedUntil = now.Add(lease)
		out = append(out, s.outbox[i].item)
	}
	return out, nil
}

// DropExhaustedOutbox implements Store.
func (s *MemStore) DropExhaustedOutbox(_ context.Context, maxAttempts int) ([]OutboxItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var dropped []OutboxItem
	kept := s.outbox[:0]
	for _, r := range s.outbox {
		if r.item.Attempts >= maxAttempts {
			dropped = append(dropped, r.item)
			continue
		}
		kept = append(kept, r)
	}
	s.outbox = kept
	return dropped, nil
}

// PendingOutbox returns the rows still awaiting completion (test helper).
func (s *MemStore) PendingOutbox() []OutboxItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]OutboxItem, 0, len(s.outbox))
	for _, r := range s.outbox {
		out = append(out, r.item)
	}
	return out
}

// SetOutboxClock lets a test drive lease expiry without sleeping.
func (s *MemStore) SetOutboxClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outboxClock = now
}

func (s *MemStore) outboxNow() time.Time {
	if s.outboxClock != nil {
		return s.outboxClock()
	}
	return time.Now()
}
