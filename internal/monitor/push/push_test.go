package push

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
)

func pushMonitor(id int64, token string, status domain.MonitorStatus) domain.Monitor {
	return domain.Monitor{
		ID:        id,
		Name:      "job",
		Type:      "push",
		Config:    json.RawMessage(`{"expected_interval_seconds":60,"grace_period_seconds":30}`),
		Active:    true,
		Status:    status,
		PushToken: token,
	}
}

func TestRecordUnknownToken(t *testing.T) {
	r := NewRecorder(Options{Store: NewMemStore()})
	if _, err := r.Record(context.Background(), "nope", Beat{}); err != ErrNotFound {
		t.Fatalf("Record unknown token err = %v, want ErrNotFound", err)
	}
}

func TestRecordUpHeartbeat(t *testing.T) {
	store := NewMemStore(pushMonitor(1, "tok", domain.StatusPending))
	var events []domain.StateChange
	r := NewRecorder(Options{
		Store:   store,
		Clock:   func() time.Time { return time.Unix(1000, 0) },
		OnEvent: func(sc domain.StateChange) { events = append(events, sc) },
	})
	ping := 84
	m, err := r.Record(context.Background(), "tok", Beat{Status: "up", Ping: &ping, Message: "ok"})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if m.Status != domain.StatusUp {
		t.Fatalf("status = %s, want up", m.Status)
	}
	beats := store.Heartbeats()
	if len(beats) != 1 || beats[0].Status != domain.StatusUp || !beats[0].Important {
		t.Fatalf("unexpected heartbeats: %+v", beats)
	}
	if beats[0].Latency == nil || *beats[0].Latency != 84*time.Millisecond {
		t.Fatalf("latency = %v, want 84ms", beats[0].Latency)
	}
	if len(events) != 1 || events[0].To != domain.StatusUp {
		t.Fatalf("expected one up transition, got %+v", events)
	}
}

func TestRecordNoTransitionNoEvent(t *testing.T) {
	store := NewMemStore(pushMonitor(1, "tok", domain.StatusUp))
	var events []domain.StateChange
	r := NewRecorder(Options{
		Store:   store,
		Clock:   func() time.Time { return time.Unix(1000, 0) },
		OnEvent: func(sc domain.StateChange) { events = append(events, sc) },
	})
	if _, err := r.Record(context.Background(), "tok", Beat{Status: "up"}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected no transition event, got %+v", events)
	}
	if len(store.Heartbeats()) != 1 {
		t.Fatal("heartbeat should still be recorded even without a transition")
	}
}

func TestRecordDownHeartbeat(t *testing.T) {
	store := NewMemStore(pushMonitor(1, "tok", domain.StatusUp))
	r := NewRecorder(Options{Store: store, Clock: func() time.Time { return time.Unix(1000, 0) }})
	m, err := r.Record(context.Background(), "tok", Beat{Status: "down", Message: "boom"})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if m.Status != domain.StatusDown {
		t.Fatalf("status = %s, want down", m.Status)
	}
}

func TestRecordUpsideDown(t *testing.T) {
	m := pushMonitor(1, "tok", domain.StatusPending)
	m.UpsideDown = true
	store := NewMemStore(m)
	r := NewRecorder(Options{Store: store, Clock: func() time.Time { return time.Unix(1000, 0) }})
	got, err := r.Record(context.Background(), "tok", Beat{Status: "up"})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if got.Status != domain.StatusDown {
		t.Fatalf("upside-down up push should be DOWN, got %s", got.Status)
	}
}

func TestWatchdogTripsOverdue(t *testing.T) {
	store := NewMemStore(pushMonitor(1, "tok", domain.StatusUp))
	// Seed a heartbeat at t=1000; deadline is 1000+90 = 1090.
	_ = store.SaveHeartbeat(context.Background(), domain.Heartbeat{MonitorID: 1, CheckedAt: time.Unix(1000, 0), Status: domain.StatusUp})

	var events []domain.StateChange
	// now = 1200 > deadline 1090 -> trip.
	w := NewWatchdog(Options{
		Store:   store,
		Clock:   func() time.Time { return time.Unix(1200, 0) },
		OnEvent: func(sc domain.StateChange) { events = append(events, sc) },
	})
	n, err := w.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("tripped = %d, want 1", n)
	}
	if st, _ := store.StatusOf(1); st != domain.StatusDown {
		t.Fatalf("monitor status = %s, want down", st)
	}
	if len(events) != 1 || events[0].To != domain.StatusDown {
		t.Fatalf("expected one down transition, got %+v", events)
	}
}

func TestWatchdogWithinWindowNoTrip(t *testing.T) {
	store := NewMemStore(pushMonitor(1, "tok", domain.StatusUp))
	_ = store.SaveHeartbeat(context.Background(), domain.Heartbeat{MonitorID: 1, CheckedAt: time.Unix(1000, 0), Status: domain.StatusUp})
	w := NewWatchdog(Options{Store: store, Clock: func() time.Time { return time.Unix(1050, 0) }}) // < 1090
	n, err := w.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("tripped = %d, want 0 (still within window)", n)
	}
}

func TestWatchdogNeverSeenLeftAlone(t *testing.T) {
	store := NewMemStore(pushMonitor(1, "tok", domain.StatusPending)) // no heartbeat yet
	w := NewWatchdog(Options{Store: store, Clock: func() time.Time { return time.Unix(999999, 0) }})
	n, err := w.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("never-seen monitor must not be tripped, tripped = %d", n)
	}
}

func TestWatchdogAlreadyDownNoTrip(t *testing.T) {
	store := NewMemStore(pushMonitor(1, "tok", domain.StatusDown))
	_ = store.SaveHeartbeat(context.Background(), domain.Heartbeat{MonitorID: 1, CheckedAt: time.Unix(1000, 0), Status: domain.StatusDown})
	w := NewWatchdog(Options{Store: store, Clock: func() time.Time { return time.Unix(9000, 0) }})
	n, _ := w.Sweep(context.Background())
	if n != 0 {
		t.Fatalf("already-down monitor must not re-trip, tripped = %d", n)
	}
}

func TestRecordPausedMonitorRejected(t *testing.T) {
	m := pushMonitor(1, "tok", domain.StatusPaused)
	m.Active = false
	r := NewRecorder(Options{Store: NewMemStore(m)})
	if _, err := r.Record(context.Background(), "tok", Beat{Status: "up"}); err != ErrNotFound {
		t.Fatalf("recording to a paused push monitor err = %v, want ErrNotFound", err)
	}
}

func TestRecordNonPushMonitorRejected(t *testing.T) {
	m := domain.Monitor{ID: 2, Name: "web", Type: "http", Active: true, Status: domain.StatusUp, PushToken: "tok2"}
	r := NewRecorder(Options{Store: NewMemStore(m)})
	if _, err := r.Record(context.Background(), "tok2", Beat{Status: "up"}); err != ErrNotFound {
		t.Fatalf("recording to a non-push monitor err = %v, want ErrNotFound", err)
	}
}

// staleSnapshotStore models the watchdog race: ListPushMonitors hands out a
// snapshot whose LastSeen predates a push that has, by write time, already
// landed in the underlying store. It lets the test drive Watchdog.Sweep against
// a monitor the watchdog believes is overdue while a fresh heartbeat exists.
type staleSnapshotStore struct {
	*MemStore
	snapshot []PushMonitor
}

func (s *staleSnapshotStore) ListPushMonitors(context.Context) ([]PushMonitor, error) {
	return s.snapshot, nil
}

func TestWatchdogDoesNotClobberFreshPush(t *testing.T) {
	mem := NewMemStore(pushMonitor(1, "tok", domain.StatusUp))
	// Observed at snapshot time: last heartbeat at t=1000 (deadline 1090).
	_ = mem.SaveHeartbeat(context.Background(), domain.Heartbeat{MonitorID: 1, CheckedAt: time.Unix(1000, 0), Status: domain.StatusUp})
	// A fresh push has since landed at t=1150 — after the snapshot the watchdog
	// is acting on, before its write.
	_ = mem.SaveHeartbeat(context.Background(), domain.Heartbeat{MonitorID: 1, CheckedAt: time.Unix(1150, 0), Status: domain.StatusUp})

	store := &staleSnapshotStore{
		MemStore: mem,
		snapshot: []PushMonitor{{Monitor: pushMonitor(1, "tok", domain.StatusUp), LastSeen: time.Unix(1000, 0), HasHeartbeat: true}},
	}
	var events []domain.StateChange
	w := NewWatchdog(Options{
		Store:   store,
		Clock:   func() time.Time { return time.Unix(1200, 0) }, // past the stale deadline 1090
		OnEvent: func(sc domain.StateChange) { events = append(events, sc) },
	})
	n, err := w.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("tripped = %d, want 0: a fresh push must not be clobbered to DOWN", n)
	}
	if st, _ := mem.StatusOf(1); st != domain.StatusUp {
		t.Fatalf("monitor status = %s, want up (fresh push preserved)", st)
	}
	if len(events) != 0 {
		t.Fatalf("expected no transition, got %+v", events)
	}
}

func TestGenerateTokenUnique(t *testing.T) {
	a, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	b, _ := GenerateToken()
	if a == b || len(a) != 32 {
		t.Fatalf("tokens not unique/expected length: %q %q", a, b)
	}
}

func TestHashToken(t *testing.T) {
	tok := "deadbeefdeadbeefdeadbeefdeadbeef"
	h := HashToken(tok)
	// Deterministic: the recorder must hash an incoming token to the same value
	// the mint stored, or no push would ever resolve.
	if HashToken(tok) != h {
		t.Fatal("HashToken is not deterministic")
	}
	// Hex SHA-256 is 64 chars and is not the token itself — storing it means a
	// dump never yields a working credential.
	if len(h) != 64 {
		t.Fatalf("hash length = %d, want 64", len(h))
	}
	if h == tok {
		t.Fatal("hash equals the raw token")
	}
	if HashToken("other") == h {
		t.Fatal("distinct tokens collided")
	}
}

// TestMonitorByPushTokenResolvesViaHash pins that the store matches on the hash
// of the incoming token, not the raw value: a monitor seeded with a raw token
// resolves for that token, but never for its stored hash presented as the token.
func TestMonitorByPushTokenResolvesViaHash(t *testing.T) {
	store := NewMemStore(pushMonitor(1, "tok", domain.StatusUp))
	if _, err := store.MonitorByPushToken(context.Background(), "tok"); err != nil {
		t.Fatalf("resolve by raw token: %v", err)
	}
	// Presenting the hash as if it were the token must not resolve — otherwise a
	// leaked at-rest hash would be a usable credential.
	if _, err := store.MonitorByPushToken(context.Background(), HashToken("tok")); err != ErrNotFound {
		t.Fatalf("hash presented as token resolved: err = %v, want ErrNotFound", err)
	}
}

// --- alert suppression --------------------------------------------------------

// stubSuppressor reports a fixed verdict, standing in for the engine's shared
// maintenance / parent-down evaluation.
type stubSuppressor struct{ reason string }

func (s stubSuppressor) Suppression(domain.Monitor, time.Time) (bool, string) {
	return s.reason != "", s.reason
}

// Push monitors never pass through the scheduler's emit path, so they used to
// emit StateChange events with Suppressed left false — their DOWN and recovery
// alerts went out during maintenance windows and while the parent was down,
// which is precisely what a maintenance window exists to prevent.
func TestRecorderMarksAlertsSuppressed(t *testing.T) {
	store := NewMemStore(pushMonitor(1, "tok", domain.StatusUp))
	var events []domain.StateChange
	r := NewRecorder(Options{
		Store:    store,
		Clock:    func() time.Time { return time.Unix(2000, 0) },
		OnEvent:  func(sc domain.StateChange) { events = append(events, sc) },
		Suppress: stubSuppressor{reason: "maintenance"},
	})

	if _, err := r.Record(context.Background(), "tok", Beat{Status: "down"}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if !events[0].Suppressed || events[0].SuppressReason != "maintenance" {
		t.Fatalf("event not marked suppressed: %+v", events[0])
	}
	// The heartbeat and the status column must still be truthful — suppression
	// silences the alert, never the data.
	if st, _ := store.StatusOf(1); st != domain.StatusDown {
		t.Fatalf("monitor status = %s, want down even while suppressed", st)
	}
}

func TestWatchdogMarksAlertsSuppressed(t *testing.T) {
	store := NewMemStore(pushMonitor(1, "tok", domain.StatusUp))
	_ = store.SaveHeartbeat(context.Background(), domain.Heartbeat{
		MonitorID: 1, CheckedAt: time.Unix(1000, 0), Status: domain.StatusUp,
	})
	var events []domain.StateChange
	w := NewWatchdog(Options{
		Store:    store,
		Clock:    func() time.Time { return time.Unix(1200, 0) },
		OnEvent:  func(sc domain.StateChange) { events = append(events, sc) },
		Suppress: stubSuppressor{reason: "parent-down"},
	})
	if _, err := w.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if !events[0].Suppressed || events[0].SuppressReason != "parent-down" {
		t.Fatalf("watchdog event not marked suppressed: %+v", events[0])
	}
	if st, _ := store.StatusOf(1); st != domain.StatusDown {
		t.Fatalf("monitor status = %s, want down even while suppressed", st)
	}
}

// A nil Suppressor leaves every alert live, which is what a deployment without
// the engine wired in must keep doing.
func TestNilSuppressorLeavesAlertsLive(t *testing.T) {
	store := NewMemStore(pushMonitor(1, "tok", domain.StatusUp))
	var events []domain.StateChange
	r := NewRecorder(Options{
		Store:   store,
		Clock:   func() time.Time { return time.Unix(2000, 0) },
		OnEvent: func(sc domain.StateChange) { events = append(events, sc) },
	})
	if _, err := r.Record(context.Background(), "tok", Beat{Status: "down"}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if len(events) != 1 || events[0].Suppressed {
		t.Fatalf("event should not be suppressed: %+v", events)
	}
}
