package engine_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/engine"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
)

// Flapping damping and the outage backoff. The fakes and helpers live in
// engine_test.go.

// setFallback swaps what a script checker answers once its queue is empty.
func (s *scriptChecker) setFallback(res domain.CheckResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fallback = res
}

// kinds renders an event list compactly for comparisons and failure messages:
// "D" DOWN, "U" UP, "P" PENDING, "F+" / "F-" a flapping onset / settled summary,
// each with a trailing "~" when suppressed.
func kinds(evs []domain.StateChange) string {
	var b strings.Builder
	for i, ev := range evs {
		if i > 0 {
			b.WriteByte(' ')
		}
		switch {
		case ev.Flapping != nil && ev.Flapping.Ended:
			b.WriteString("F-")
		case ev.Flapping != nil:
			b.WriteString("F+")
		default:
			b.WriteString(strings.ToUpper(string(ev.To)[:1]))
		}
		if ev.Suppressed {
			b.WriteByte('~')
		}
	}
	return b.String()
}

// runChecks drives n scheduled checks `every` apart and returns the heartbeat
// count reached.
//
// Each round waits for the heartbeat AND for the re-armed timer: a check writes
// its heartbeat before it re-arms, so advancing the clock on the heartbeat alone
// can move time past a timer that does not exist yet, and the next check then
// never fires.
func runChecks(t *testing.T, clk *fakeClock, store *engine.MemStore, have, n int, every time.Duration) int {
	t.Helper()
	for range n {
		clk.Advance(every)
		have++
		waitHeartbeats(t, store, have)
		waitFor(t, "the re-armed timer", func() bool { return clk.Armed() > 0 })
	}
	return have
}

// firstCheck fires a freshly upserted monitor's first (unjittered) check and
// waits until it has re-armed.
func firstCheck(t *testing.T, clk *fakeClock, store *engine.MemStore) {
	t.Helper()
	runChecks(t, clk, store, 0, 1, 0)
}

// nextDelay waits for the job's timer to be re-armed and returns how far ahead
// it fires.
func nextDelay(t *testing.T, clk *fakeClock) time.Duration {
	t.Helper()
	var d time.Duration
	waitFor(t, "a re-armed timer", func() bool {
		at, ok := clk.NextAt()
		d = at.Sub(clk.Now())
		return ok && d > 0
	})
	return d
}

// A DOWN monitor that answers once between failures must not send a recovery
// and a fresh outage per bounce: with RecoveryChecks = 3 it stays DOWN until
// three successes in a row, those checks run at the retry cadence, and their
// heartbeats say what they are.
func TestRecoveryNeedsConsecutiveSuccesses(t *testing.T) {
	reg := registry.New()
	chk := &scriptChecker{
		typ:      "fake",
		queue:    []domain.CheckResult{down(), up(), down(), up(), up()},
		fallback: up(),
	}
	reg.Register(chk)
	rec := &eventRecorder{}
	e, store, clk := newEngine(t, engine.Options{Registry: reg, OnEvent: rec.record})

	m := testMonitor(1, "fake")
	m.RetryInterval = 10 * time.Second
	m.RecoveryChecks = 3
	if err := e.Upsert(m); err != nil {
		t.Fatal(err)
	}
	firstCheck(t, clk, store) // down

	n := runChecks(t, clk, store, 1, 1, time.Minute) // up: 1 of 3
	if hb := store.Heartbeats()[n-1]; hb.Status != domain.StatusDown || !strings.Contains(hb.Message, "1 of 3") {
		t.Fatalf("first success heartbeat = %+v, want DOWN saying 1 of 3", hb)
	}
	if d := nextDelay(t, clk); d != 10*time.Second {
		t.Fatalf("interval while confirming = %v, want the 10s retry cadence", d)
	}
	n = runChecks(t, clk, store, n, 1, 10*time.Second) // down: the count restarts
	n = runChecks(t, clk, store, n, 1, time.Minute)    // up: 1 of 3
	n = runChecks(t, clk, store, n, 1, 10*time.Second) // up: 2 of 3
	if got := kinds(rec.all()); got != "D" {
		t.Fatalf("events before the third success = %q, want only the DOWN", got)
	}
	runChecks(t, clk, store, n, 1, 10*time.Second) // up: 3 of 3 → UP
	waitFor(t, "the recovery", func() bool { return len(rec.all()) == 2 })
	if got := kinds(rec.all()); got != "D U" {
		t.Fatalf("events = %q, want %q", got, "D U")
	}
}

// flapEngine builds an engine with flapping damping (threshold changes within
// ten minutes) around a monitor checked once a minute with no retries, and runs
// its first check.
func flapEngine(t *testing.T, threshold int, queue ...domain.CheckResult) (*scriptChecker, *eventRecorder, *engine.MemStore, *fakeClock) {
	t.Helper()
	reg := registry.New()
	chk := &scriptChecker{typ: "fake", queue: queue, fallback: up()}
	reg.Register(chk)
	rec := &eventRecorder{}
	e, store, clk := newEngine(t, engine.Options{
		Registry:      reg,
		OnEvent:       rec.record,
		FlapThreshold: threshold,
		FlapWindow:    10 * time.Minute,
	})
	if err := e.Upsert(testMonitor(1, "fake")); err != nil {
		t.Fatal(err)
	}
	firstCheck(t, clk, store)
	return chk, rec, store, clk
}

// The third change inside the window switches the monitor to ONE summary: that
// change and every later one are held back, and after a quiet window a second
// summary says where it settled. It settled UP with nothing announced as down,
// so no recovery is owed.
func TestFlappingReplacesAlertsWithASummary(t *testing.T) {
	_, rec, store, clk := flapEngine(t, 3, down(), up(), down(), up())
	n := runChecks(t, clk, store, 1, 3, time.Minute) // up, down (trips), up (held)
	if got, want := kinds(rec.all()), "D U F+ D~ U~"; got != want {
		t.Fatalf("events while flapping = %q, want %q", got, want)
	}
	evs := rec.all()
	if evs[3].SuppressReason != domain.SuppressFlapping {
		t.Fatalf("held-back change reason = %q, want %q", evs[3].SuppressReason, domain.SuppressFlapping)
	}
	if fl := evs[2].Flapping; fl.Changes != 3 || fl.Window != 10*time.Minute {
		t.Fatalf("onset summary = %+v, want 3 changes in 10m", fl)
	}

	// Quiet, but not yet for a whole window: still flapping.
	n = runChecks(t, clk, store, n, 9, time.Minute)
	if got := kinds(rec.all()); got != "D U F+ D~ U~" {
		t.Fatalf("events before the window elapsed = %q, want nothing new", got)
	}
	runChecks(t, clk, store, n, 1, time.Minute)
	waitFor(t, "the settled summary", func() bool { return len(rec.all()) == 6 })
	evs = rec.all()
	if got, want := kinds(evs), "D U F+ D~ U~ F-"; got != want {
		t.Fatalf("events = %q, want %q", got, want)
	}
	if fl := evs[5].Flapping; fl.Changes != 2 || fl.Status != domain.StatusUp {
		t.Fatalf("settled summary = %+v, want 2 held-back changes, settled up", fl)
	}
}

// An outage announced before the flapping began is left open by the held-back
// UPs, so its channels are not told "resolved" by a single bounce — and it must
// therefore be resolved when the monitor settles UP, or they never hear it end.
func TestFlappingThatSettlesUpResolvesTheAnnouncedOutage(t *testing.T) {
	_, rec, store, clk := flapEngine(t, 4, down(), up(), down(), up())
	n := runChecks(t, clk, store, 1, 3, time.Minute) // up, down, up (trips)
	if got, want := kinds(rec.all()), "D U D F+ U~"; got != want {
		t.Fatalf("events while flapping = %q, want %q", got, want)
	}
	runChecks(t, clk, store, n, 10, time.Minute)
	waitFor(t, "the owed recovery", func() bool { return len(rec.all()) == 7 })
	evs := rec.all()
	if got, want := kinds(evs), "D U D F+ U~ F- U"; got != want {
		t.Fatalf("events = %q, want %q", got, want)
	}
	if last := evs[6]; last.From != domain.StatusDown || last.To != domain.StatusUp {
		t.Fatalf("owed recovery = %s→%s, want down→up (the dispatcher routes on it)", last.From, last.To)
	}
}

// Settling DOWN announces the held-back outage, once, dated from when it
// actually began rather than from when the flapping ended.
func TestFlappingThatSettlesDownAnnouncesTheOutage(t *testing.T) {
	chk, rec, store, clk := flapEngine(t, 3, down(), up(), down())
	chk.setFallback(down())
	n := runChecks(t, clk, store, 1, 2, time.Minute) // up, down (trips)
	downAt := clk.Now()
	runChecks(t, clk, store, n, 12, time.Minute)
	waitFor(t, "the re-announcement", func() bool { return len(rec.all()) == 6 })
	evs := rec.all()
	if got, want := kinds(evs), "D U F+ D~ F- D"; got != want {
		t.Fatalf("events = %q, want %q", got, want)
	}
	if !evs[5].At.Equal(downAt) {
		t.Errorf("re-announced outage starts %v, want %v", evs[5].At, downAt)
	}
}

// Off unless configured: without a threshold every change alerts as before.
func TestFlappingOffWithoutAThreshold(t *testing.T) {
	_, rec, store, clk := flapEngine(t, 0, down(), up(), down(), up(), down(), up())
	runChecks(t, clk, store, 1, 5, time.Minute)
	waitFor(t, "six transitions", func() bool { return len(rec.all()) == 6 })
	if got, want := kinds(rec.all()), "D U D U D U"; got != want {
		t.Fatalf("events = %q, want %q", got, want)
	}
}

// A monitor DOWN for longer than DownBackoffAfter is checked every
// DownBackoffInterval; the first success returns it to a fast cadence at once.
func TestLongOutagesBackOffAndTheFirstSuccessEndsIt(t *testing.T) {
	reg := registry.New()
	chk := &scriptChecker{typ: "fake", fallback: down()}
	reg.Register(chk)
	e, store, clk := newEngine(t, engine.Options{
		Registry:            reg,
		DownBackoffAfter:    5 * time.Minute,
		DownBackoffInterval: 10 * time.Minute,
	})
	m := testMonitor(1, "fake")
	m.RetryInterval = 20 * time.Second
	m.RecoveryChecks = 2
	if err := e.Upsert(m); err != nil {
		t.Fatal(err)
	}

	firstCheck(t, clk, store)                        // DOWN at t0
	n := runChecks(t, clk, store, 1, 4, time.Minute) // t0+4m
	if d := nextDelay(t, clk); d != time.Minute {
		t.Fatalf("interval after 4m down = %v, want the normal 1m", d)
	}
	n = runChecks(t, clk, store, n, 1, time.Minute) // t0+5m
	if d := nextDelay(t, clk); d != 10*time.Minute {
		t.Fatalf("interval after 5m down = %v, want the 10m backoff", d)
	}

	chk.setFallback(up())
	n = runChecks(t, clk, store, n, 1, 10*time.Minute) // success 1 of 2
	if d := nextDelay(t, clk); d != 20*time.Second {
		t.Fatalf("interval after the first success = %v, want the 20s retry cadence", d)
	}
	runChecks(t, clk, store, n, 1, 20*time.Second) // success 2 of 2: UP
	if d := nextDelay(t, clk); d != time.Minute {
		t.Fatalf("interval after recovery = %v, want the normal 1m", d)
	}
}

// The backoff never makes checks MORE frequent: a monitor whose own interval is
// already longer than the backoff interval keeps it.
func TestBackoffNeverShortensTheInterval(t *testing.T) {
	reg := registry.New()
	reg.Register(&scriptChecker{typ: "fake", fallback: down()})
	e, store, clk := newEngine(t, engine.Options{
		Registry:            reg,
		DownBackoffAfter:    time.Minute,
		DownBackoffInterval: 5 * time.Minute,
	})
	m := testMonitor(1, "fake")
	m.Interval = 30 * time.Minute
	if err := e.Upsert(m); err != nil {
		t.Fatal(err)
	}
	firstCheck(t, clk, store)
	runChecks(t, clk, store, 1, 2, 30*time.Minute)
	if d := nextDelay(t, clk); d != 30*time.Minute {
		t.Fatalf("interval = %v, want the monitor's own 30m", d)
	}
}
