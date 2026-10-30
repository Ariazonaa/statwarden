package retention

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeStore is an in-memory Store: it records the cutoffs it was asked to prune
// with and returns a scripted rows-deleted / error result. No live database is
// involved, so the scheduling logic is tested deterministically.
// errAny is any failure; the tests care that it IS one, not which.
var errAny = errors.New("boom")

type fakeStore struct {
	mu      sync.Mutex
	cutoffs []time.Time
	deleted int64
	err     error
	calls   int

	deliveryCutoffs []time.Time
	deliveryDeleted int64
	deliveryErr     error
	deliveryCalls   int

	// binDays records the WINDOW each sweep asked for. The cutoff instant is
	// the database's business now, so there is no timestamp here to assert on.
	binDays   []int
	binPruned []PrunedMonitor
	binErr    error
	binCalls  int
}

func (f *fakeStore) PruneBefore(_ context.Context, cutoff time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.cutoffs = append(f.cutoffs, cutoff)
	return f.deleted, f.err
}

func (f *fakeStore) PruneDeliveriesBefore(_ context.Context, cutoff time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deliveryCalls++
	f.deliveryCutoffs = append(f.deliveryCutoffs, cutoff)
	return f.deliveryDeleted, f.deliveryErr
}

func (f *fakeStore) PruneBin(_ context.Context, olderThanDays int) ([]PrunedMonitor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.binCalls++
	f.binDays = append(f.binDays, olderThanDays)
	if f.binErr != nil {
		return nil, f.binErr
	}
	return append([]PrunedMonitor(nil), f.binPruned...), nil
}

func (f *fakeStore) binSnapshot() (int, []int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.binCalls, append([]int(nil), f.binDays...)
}

func (f *fakeStore) deliverySnapshot() (int, []time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deliveryCalls, append([]time.Time(nil), f.deliveryCutoffs...)
}

func (f *fakeStore) snapshot() (int, []time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, append([]time.Time(nil), f.cutoffs...)
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fixedNow returns a clock function pinned to t.
func fixedNow(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func TestPruneComputesCutoffFromRetentionDays(t *testing.T) {
	// A reference "now" with a non-UTC zone to prove the cutoff is normalised.
	loc := time.FixedZone("UTC+5", 5*3600)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, loc)

	store := &fakeStore{deleted: 7}
	p := New(Options{
		Store:         store,
		RetentionDays: 30,
		Logger:        quietLogger(),
		Now:           fixedNow(now),
	})

	deleted, err := p.Prune(context.Background())
	if err != nil {
		t.Fatalf("Prune returned error: %v", err)
	}
	if deleted != 7 {
		t.Fatalf("deleted = %d, want 7", deleted)
	}

	calls, cutoffs := store.snapshot()
	if calls != 1 {
		t.Fatalf("store called %d times, want 1", calls)
	}
	want := now.UTC().AddDate(0, 0, -30)
	if !cutoffs[0].Equal(want) {
		t.Fatalf("cutoff = %s, want %s", cutoffs[0], want)
	}
	if cutoffs[0].Location() != time.UTC {
		t.Fatalf("cutoff zone = %s, want UTC", cutoffs[0].Location())
	}
}

func TestPruneDisabledWhenRetentionNonPositive(t *testing.T) {
	for _, days := range []int{0, -5} {
		store := &fakeStore{deleted: 99}
		p := New(Options{
			Store:         store,
			RetentionDays: days,
			Logger:        quietLogger(),
			Now:           fixedNow(time.Now()),
		})
		deleted, err := p.Prune(context.Background())
		if err != nil {
			t.Fatalf("days=%d: unexpected error: %v", days, err)
		}
		if deleted != 0 {
			t.Fatalf("days=%d: deleted = %d, want 0", days, deleted)
		}
		if calls, _ := store.snapshot(); calls != 0 {
			t.Fatalf("days=%d: store called %d times, want 0", days, calls)
		}
	}
}

func TestPrunePropagatesStoreError(t *testing.T) {
	wantErr := errors.New("db down")
	store := &fakeStore{err: wantErr}
	p := New(Options{
		Store:         store,
		RetentionDays: 1,
		Logger:        quietLogger(),
		Now:           fixedNow(time.Now()),
	})
	if _, err := p.Prune(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("Prune error = %v, want %v", err, wantErr)
	}
}

func TestStartRunsImmediatelyAndStopsOnContextCancel(t *testing.T) {
	store := &fakeStore{}
	p := New(Options{
		Store:         store,
		RetentionDays: 90,
		Interval:      time.Hour, // long enough that only the immediate pass fires
		Logger:        quietLogger(),
		Now:           fixedNow(time.Now()),
	})

	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)

	// The loop runs one pass immediately; poll until it lands.
	waitFor(t, func() bool { c, _ := store.snapshot(); return c >= 1 })

	cancel()
	// After cancellation no further passes should occur.
	calls, _ := store.snapshot()
	if calls != 1 {
		t.Fatalf("calls after immediate pass = %d, want 1", calls)
	}
}

func TestStartTicksRepeatedly(t *testing.T) {
	store := &fakeStore{}
	p := New(Options{
		Store:         store,
		RetentionDays: 90,
		Interval:      5 * time.Millisecond,
		Logger:        quietLogger(),
		Now:           fixedNow(time.Now()),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)

	// Immediate pass + several ticks.
	waitFor(t, func() bool { c, _ := store.snapshot(); return c >= 3 })
}

func TestStartDisabledDoesNotSpawnLoop(t *testing.T) {
	store := &fakeStore{}
	p := New(Options{
		Store:         store,
		RetentionDays: 0,
		Interval:      time.Millisecond,
		Logger:        quietLogger(),
		Now:           fixedNow(time.Now()),
	})
	p.Start(context.Background())
	time.Sleep(20 * time.Millisecond)
	if calls, _ := store.snapshot(); calls != 0 {
		t.Fatalf("disabled pruner called store %d times, want 0", calls)
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	p := New(Options{Store: &fakeStore{}, RetentionDays: 10})
	if p.interval != DefaultInterval {
		t.Fatalf("interval = %s, want default %s", p.interval, DefaultInterval)
	}
	if p.log == nil {
		t.Fatal("logger not defaulted")
	}
	if p.now == nil {
		t.Fatal("clock not defaulted")
	}
}

// waitFor polls cond up to a generous deadline, failing the test on timeout.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

// TestDeliveryLogIsPrunedOnItsOwnWindow
//
// The delivery log is the record of who was told what and when. It grows for
// the life of the instance and nothing trimmed it — which became more than a
// storage question when Dispatcher.Restore started reading it on the startup
// path, where the work then grows with the age of the deployment rather than
// with the size of the outage.
//
// Its own window, not the heartbeats': one is a stream of samples, the other a
// short list of events an operator reads after an incident.
func TestDeliveryLogIsPrunedOnItsOwnWindow(t *testing.T) {
	store := &fakeStore{}
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	p := New(Options{
		Store:                 store,
		RetentionDays:         30,
		DeliveryRetentionDays: 400,
		Now:                   func() time.Time { return now },
		Logger:                quietLogger(),
	})
	p.runOnce(context.Background())

	beats, beatCutoffs := store.snapshot()
	deliveries, deliveryCutoffs := store.deliverySnapshot()
	if beats != 1 || deliveries != 1 {
		t.Fatalf("passes = %d heartbeat, %d delivery; want one each", beats, deliveries)
	}
	if want := now.AddDate(0, 0, -30); !beatCutoffs[0].Equal(want) {
		t.Fatalf("heartbeat cutoff = %s, want %s", beatCutoffs[0], want)
	}
	if want := now.AddDate(0, 0, -400); !deliveryCutoffs[0].Equal(want) {
		t.Fatalf("delivery cutoff = %s, want %s — the two windows are not independent",
			deliveryCutoffs[0], want)
	}
}

// A window of zero leaves the delivery log alone while heartbeats keep being
// pruned. The two policies are set separately and one must not switch off the
// other.
func TestDeliveryPruningCanBeDisabledAlone(t *testing.T) {
	store := &fakeStore{}
	p := New(Options{
		Store: store, RetentionDays: 30, DeliveryRetentionDays: 0,
		Now: time.Now, Logger: quietLogger(),
	})
	p.runOnce(context.Background())

	if beats, _ := store.snapshot(); beats != 1 {
		t.Fatalf("heartbeat passes = %d, want 1", beats)
	}
	if deliveries, _ := store.deliverySnapshot(); deliveries != 0 {
		t.Fatalf("delivery passes = %d, want 0", deliveries)
	}
}

// A failing delivery prune must not stop heartbeats being pruned: that is the
// pass with the volume behind it, and the two tables are unrelated.
func TestAFailingDeliveryPruneDoesNotStopTheHeartbeatPass(t *testing.T) {
	store := &fakeStore{deliveryErr: errAny}
	p := New(Options{
		Store: store, RetentionDays: 30, DeliveryRetentionDays: 400,
		Now: time.Now, Logger: quietLogger(),
	})
	p.runOnce(context.Background())
	p.runOnce(context.Background())

	if beats, _ := store.snapshot(); beats != 2 {
		t.Fatalf("heartbeat passes = %d, want 2 — a delivery failure stopped them", beats)
	}
}

// …and the other way round, which is the direction that was actually broken.
//
// runOnce used to return as soon as the heartbeat delete failed, so the delivery
// prune never ran. PruneBefore is one unbounded DELETE over the biggest table in
// the schema, so a statement timeout or lock contention hits it and keeps hitting
// it — and the delivery log then grew forever, unmentioned by any log line,
// re-opening the unbounded growth that #85 was closed to fix.
func TestAFailingHeartbeatPruneDoesNotStopTheDeliveryPass(t *testing.T) {
	store := &fakeStore{err: errAny}
	p := New(Options{
		Store: store, RetentionDays: 30, DeliveryRetentionDays: 400,
		Now: time.Now, Logger: quietLogger(),
	})
	p.runOnce(context.Background())
	p.runOnce(context.Background())

	if beats, _ := store.snapshot(); beats != 2 {
		t.Fatalf("heartbeat passes = %d, want 2", beats)
	}
	deliveries, _ := store.deliverySnapshot()
	if deliveries != 2 {
		t.Fatalf("delivery passes = %d, want 2 — a heartbeat failure stopped them, "+
			"which leaves notification_deliveries growing without bound", deliveries)
	}
}

// The bin sweep is the one pass that destroys a monitor's whole history, so its
// window has to be its own: an operator who shortens the heartbeat window is not
// asking for their recycle bin to empty faster.
func TestBinSweepUsesItsOwnWindow(t *testing.T) {
	store := &fakeStore{}
	now := time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC)
	p := New(Options{
		Store:                 store,
		RetentionDays:         90,
		DeliveryRetentionDays: 365,
		TrashRetentionDays:    30,
		Logger:                quietLogger(),
		Now:                   func() time.Time { return now },
	})
	p.runOnce(context.Background())

	calls, days := store.binSnapshot()
	if calls != 1 {
		t.Fatalf("bin sweep ran %d times, want 1", calls)
	}
	if days[0] != 30 {
		t.Fatalf("bin window = %d days, want 30 (not the heartbeat window)", days[0])
	}
}

// The sweep passes a WINDOW and never an instant, so the application clock
// cannot move the cutoff.
//
// It used to compute `now() - trashDays` in Go and hand the result to Postgres.
// That made an irreversible delete depend on two clocks agreeing, and the
// failure is one-directional: a container whose clock has drifted forward hands
// over a cutoff from the future and empties a bin that was meant to hold
// another fortnight. Nothing about the request tells the database that the
// instant is wrong.
func TestBinSweepDoesNotDependOnTheApplicationClock(t *testing.T) {
	store := &fakeStore{}
	skewed := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC) // years fast
	p := New(Options{
		Store:              store,
		RetentionDays:      90,
		TrashRetentionDays: 30,
		Logger:             quietLogger(),
		Now:                func() time.Time { return skewed },
	})
	p.runOnce(context.Background())

	_, days := store.binSnapshot()
	if len(days) != 1 || days[0] != 30 {
		t.Fatalf("bin sweep asked for %v, want exactly [30] whatever the local clock says", days)
	}
}

// The one irreversible pass has to say what it took. Afterwards the row is gone
// and the log line is the only remaining trace of it.
func TestBinSweepNamesEveryMonitorItDestroys(t *testing.T) {
	store := &fakeStore{binPruned: []PrunedMonitor{
		{ID: 7, Name: "old-api"},
		{ID: 9, Name: "retired-cert-check"},
	}}
	var buf bytes.Buffer
	p := New(Options{
		Store:              store,
		RetentionDays:      90,
		TrashRetentionDays: 30,
		Logger:             slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:                time.Now,
	})
	p.runOnce(context.Background())

	out := buf.String()
	for _, want := range []string{"old-api", "retired-cert-check", "monitor_id=7", "monitor_id=9"} {
		if !strings.Contains(out, want) {
			t.Errorf("the log does not mention %q; it is the only record left:\n%s", want, out)
		}
	}
}

// Zero disables it and leaves the other two passes alone. The bin then keeps
// everything until someone empties it by hand, which is a legitimate choice.
func TestBinSweepDisabledByZero(t *testing.T) {
	store := &fakeStore{}
	p := New(Options{
		Store:                 store,
		RetentionDays:         90,
		DeliveryRetentionDays: 365,
		TrashRetentionDays:    0,
		Logger:                quietLogger(),
		Now:                   time.Now,
	})
	p.runOnce(context.Background())

	if calls, _ := store.binSnapshot(); calls != 0 {
		t.Fatalf("bin sweep ran %d times with the window disabled", calls)
	}
	if calls, _ := store.snapshot(); calls != 1 {
		t.Fatalf("heartbeat prune ran %d times; disabling the bin must not disable it", calls)
	}
}

// A failing sweep must not take the other passes down with it: the bin is the
// least important of the three, and it runs last for exactly that reason.
func TestAFailingBinSweepDoesNotStopTheOthers(t *testing.T) {
	store := &fakeStore{binErr: errors.New("nope")}
	p := New(Options{
		Store:                 store,
		RetentionDays:         90,
		DeliveryRetentionDays: 365,
		TrashRetentionDays:    30,
		Logger:                quietLogger(),
		Now:                   time.Now,
	})
	p.runOnce(context.Background())
	p.runOnce(context.Background())

	if calls, _ := store.snapshot(); calls != 2 {
		t.Fatalf("heartbeat prune ran %d times after a bin failure, want 2", calls)
	}
	if calls, _ := store.binSnapshot(); calls != 2 {
		t.Fatalf("bin sweep ran %d times, want 2 — a failure must not disable it", calls)
	}
}

// A panic in one pass must not take the other two with it.
//
// The caller wraps runOnce in a single safego.Do, which recovers ABOVE it — so
// before each pass got its own recover, a panic in the heartbeat delete returned
// from all three, and the delivery log and the recycle bin were silently not
// swept for as long as it kept happening. Errors have been isolated between the
// passes for a while; this is the failure mode nobody writes a test for.
func TestAPanicInOnePassDoesNotStopTheOthers(t *testing.T) {
	store := &panickingStore{}
	p := New(Options{
		Store:                 store,
		RetentionDays:         90,
		DeliveryRetentionDays: 365,
		TrashRetentionDays:    30,
		Logger:                quietLogger(),
		Now:                   time.Now,
	})
	p.runOnce(context.Background())

	if store.deliveryCalls == 0 {
		t.Error("the delivery log was not pruned after the heartbeat pass panicked")
	}
	if store.binCalls == 0 {
		t.Error("the recycle bin was not swept after the heartbeat pass panicked")
	}
	// And the next tick still runs, rather than the job being dead.
	p.runOnce(context.Background())
	if store.deliveryCalls != 2 || store.binCalls != 2 {
		t.Errorf("second pass: deliveries=%d bin=%d, want 2 and 2",
			store.deliveryCalls, store.binCalls)
	}
}

// panickingStore panics on the first pass and records the other two.
type panickingStore struct {
	deliveryCalls int
	binCalls      int
}

func (s *panickingStore) PruneBefore(context.Context, time.Time) (int64, error) {
	panic("the largest DELETE in the schema, and something went wrong in it")
}

func (s *panickingStore) PruneDeliveriesBefore(context.Context, time.Time) (int64, error) {
	s.deliveryCalls++
	return 0, nil
}

func (s *panickingStore) PruneBin(context.Context, int) ([]PrunedMonitor, error) {
	s.binCalls++
	return nil, nil
}
