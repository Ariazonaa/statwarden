package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/anomaly"
	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/maintenance"
	"github.com/Ariazonaa/statwarden/internal/monitor/engine"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
	"github.com/Ariazonaa/statwarden/internal/testutil"
)

// ---------------------------------------------------------------------------
// Fakes: clock, checker, event recorder
// ---------------------------------------------------------------------------

// fakeClock implements engine.Clock with manually advanced time. Advance
// fires due timers synchronously (each callback spawns its own goroutine
// inside the engine, mirroring time.AfterFunc semantics closely enough).
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	clk     *fakeClock
	when    time.Time
	f       func()
	fired   bool
	stopped bool
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) engine.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{clk: c, when: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return t
}

func (t *fakeTimer) Stop() bool {
	t.clk.mu.Lock()
	defer t.clk.mu.Unlock()
	if t.fired || t.stopped {
		return false
	}
	t.stopped = true
	return true
}

// Advance moves the clock to now+d, firing due timers in chronological order.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	for {
		var next *fakeTimer
		for _, t := range c.timers {
			if t.fired || t.stopped || t.when.After(target) {
				continue
			}
			if next == nil || t.when.Before(next.when) {
				next = t
			}
		}
		if next == nil {
			break
		}
		if next.when.After(c.now) {
			c.now = next.when
		}
		next.fired = true
		f := next.f
		c.mu.Unlock()
		f()
		c.mu.Lock()
	}
	c.now = target
	c.mu.Unlock()
}

// Armed returns the number of timers that are still pending (neither fired
// nor stopped) — i.e. how many concurrent timers a job currently owns.
func (c *fakeClock) Armed() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.timers {
		if !t.fired && !t.stopped {
			n++
		}
	}
	return n
}

// NextAt returns the earliest pending timer's fire time.
func (c *fakeClock) NextAt() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var when time.Time
	found := false
	for _, t := range c.timers {
		if t.fired || t.stopped {
			continue
		}
		if !found || t.when.Before(when) {
			when = t.when
			found = true
		}
	}
	return when, found
}

// scriptChecker returns queued results in order (then the fallback). It can
// optionally block until released and counts invocations.
type scriptChecker struct {
	typ      string
	started  atomic.Int32
	release  chan struct{} // nil = never block
	current  atomic.Int32  // concurrent invocations
	maxSeen  atomic.Int32
	mu       sync.Mutex
	queue    []domain.CheckResult
	fallback domain.CheckResult
	validate func(json.RawMessage) error
}

func (s *scriptChecker) Type() string { return s.typ }

func (s *scriptChecker) Validate(cfg json.RawMessage) error {
	if s.validate != nil {
		return s.validate(cfg)
	}
	return nil
}

func (s *scriptChecker) Check(ctx context.Context, _ json.RawMessage) (domain.CheckResult, error) {
	s.started.Add(1)
	cur := s.current.Add(1)
	for {
		maxSeen := s.maxSeen.Load()
		if cur <= maxSeen || s.maxSeen.CompareAndSwap(maxSeen, cur) {
			break
		}
	}
	defer s.current.Add(-1)

	if s.release != nil {
		select {
		case <-s.release:
		case <-ctx.Done():
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) > 0 {
		res := s.queue[0]
		s.queue = s.queue[1:]
		return res, nil
	}
	return s.fallback, nil
}

// passiveChecker carries the PassiveChecker marker.
type passiveChecker struct{ scriptChecker }

func (*passiveChecker) Passive() {}

// eventRecorder collects StateChange events thread-safely.
type eventRecorder struct {
	mu     sync.Mutex
	events []domain.StateChange
}

func (r *eventRecorder) record(ev domain.StateChange) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *eventRecorder) all() []domain.StateChange {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]domain.StateChange(nil), r.events...)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func up() domain.CheckResult {
	return domain.CheckResult{Status: domain.StatusUp, Latency: 5 * time.Millisecond, Message: "OK"}
}
func down() domain.CheckResult { return domain.CheckResult{Status: domain.StatusDown, Message: "boom"} }

func testMonitor(id int64, typ string) domain.Monitor {
	return domain.Monitor{
		ID:       id,
		Name:     "m",
		Type:     typ,
		Config:   json.RawMessage(`{}`),
		Interval: 60 * time.Second,
		Active:   true,
		Status:   domain.StatusUp,
	}
}

// newEngine builds an engine with test defaults: fake clock, no jitter.
func newEngine(t *testing.T, opts engine.Options) (*engine.Engine, *engine.MemStore, *fakeClock) {
	t.Helper()
	clk := newFakeClock()
	store := engine.NewMemStore()
	if opts.Store == nil {
		opts.Store = store
	}
	if opts.Clock == nil {
		opts.Clock = clk
	}
	if opts.MaxStartupJitter == 0 {
		opts.MaxStartupJitter = -1 // deterministic first fire
	}
	if opts.Workers == 0 {
		opts.Workers = 4
	}
	e := engine.New(opts)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), testutil.Wait(2*time.Second))
		defer cancel()
		_ = e.Stop(ctx)
	})
	return e, store, clk
}

func waitFor(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testutil.Wait(3 * time.Second))
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", msg)
}

func waitHeartbeats(t *testing.T, store *engine.MemStore, n int) []domain.Heartbeat {
	t.Helper()
	waitFor(t, "heartbeats", func() bool { return len(store.Heartbeats()) >= n })
	return store.Heartbeats()
}

// checkNowAndWait drives exactly one more check and waits for its heartbeat.
//
// A completing check writes its heartbeat BEFORE it clears its running flag, so
// a caller that waits only for the heartbeat can call CheckNow again while the
// previous one is still winding down. That returns ErrCheckInProgress, nothing
// new is started, and a wait for the next heartbeat then times out — a flake
// with a three-second penalty and a misleading message ("timed out waiting for
// heartbeats", when the real answer is "no check was ever started"). Retrying
// on that one error is what closes the window.
func checkNowAndWait(t *testing.T, e *engine.Engine, store *engine.MemStore, id int64, want int) {
	t.Helper()
	deadline := time.Now().Add(testutil.Wait(3 * time.Second))
	for time.Now().Before(deadline) {
		err := e.CheckNow(id)
		if errors.Is(err, engine.ErrCheckInProgress) {
			time.Sleep(time.Millisecond)
			continue
		}
		if err != nil {
			t.Fatalf("CheckNow: %v", err)
		}
		waitHeartbeats(t, store, want)
		return
	}
	t.Fatalf("could not start check %d: one was in progress for the whole deadline", want)
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestSchedulerDriftCorrection verifies the timer is re-armed only after the
// check completes and that the next fire time is based on the scheduled fire
// time, not on when the (slow) check finished.
func TestSchedulerDriftCorrection(t *testing.T) {
	reg := registry.New()
	chk := &scriptChecker{typ: "fake", release: make(chan struct{}), fallback: up()}
	reg.Register(chk)

	e, store, clk := newEngine(t, engine.Options{Registry: reg})
	t0 := clk.Now()

	m := testMonitor(1, "fake")
	m.Interval = 10 * time.Second
	if err := e.Upsert(m); err != nil {
		t.Fatal(err)
	}
	// First fire is scheduled immediately (jitter disabled).
	if at, ok := clk.NextAt(); !ok || !at.Equal(t0) {
		t.Fatalf("first fire at %v, want %v", at, t0)
	}

	clk.Advance(0) // fire check 1; checker blocks
	waitFor(t, "check started", func() bool { return chk.started.Load() == 1 })

	// While the check is running no timer may be armed (re-arm happens
	// strictly after completion) and the clock "takes" 3s.
	clk.Advance(3 * time.Second)
	if _, ok := clk.NextAt(); ok {
		t.Fatal("timer armed while check still running")
	}

	close(chk.release)
	waitHeartbeats(t, store, 1)
	waitFor(t, "re-arm", func() bool { _, ok := clk.NextAt(); return ok })

	// Drift-corrected: next fire at t0+10s, not (t0+3s)+10s.
	if at, _ := clk.NextAt(); !at.Equal(t0.Add(10 * time.Second)) {
		t.Fatalf("next fire at %v, want %v (drift-corrected)", at, t0.Add(10*time.Second))
	}

	// Second cycle fires at the corrected time and re-arms at t0+20s.
	clk.Advance(7 * time.Second)
	waitHeartbeats(t, store, 2)
	waitFor(t, "second re-arm", func() bool {
		at, ok := clk.NextAt()
		return ok && at.Equal(t0.Add(20*time.Second))
	})
}

// TestNoOverlappingRuns verifies the same monitor never runs twice
// concurrently, even when checks are triggered while one is in flight.
func TestNoOverlappingRuns(t *testing.T) {
	reg := registry.New()
	chk := &scriptChecker{typ: "fake", release: make(chan struct{}), fallback: up()}
	reg.Register(chk)

	e, store, clk := newEngine(t, engine.Options{Registry: reg})
	if err := e.Upsert(testMonitor(1, "fake")); err != nil {
		t.Fatal(err)
	}

	clk.Advance(0)
	waitFor(t, "check started", func() bool { return chk.started.Load() == 1 })

	// A manual trigger while running is satisfied by the in-flight check, and
	// now SAYS so. This used to assert a nil error, which is the same value a
	// trigger that actually started a check returns — so the one thing the
	// caller might want to know was the one thing it could not learn. The
	// behaviour is unchanged; only its reporting is.
	if err := e.CheckNow(1); !errors.Is(err, engine.ErrCheckInProgress) {
		t.Fatalf("CheckNow while a check is running = %v, want ErrCheckInProgress", err)
	}
	time.Sleep(20 * time.Millisecond)
	if got := chk.started.Load(); got != 1 {
		t.Fatalf("started %d checks, want 1 (no overlap)", got)
	}

	close(chk.release)
	waitHeartbeats(t, store, 1)
	// Retrying: the first check writes its heartbeat before it clears its
	// running flag, so a trigger issued the instant the heartbeat lands can
	// still be refused.
	waitFor(t, "the second check to start", func() bool {
		return e.CheckNow(1) == nil
	})
	waitFor(t, "second check", func() bool { return chk.started.Load() == 2 })
	if maxSeen := chk.maxSeen.Load(); maxSeen != 1 {
		t.Fatalf("max concurrency for one monitor = %d, want 1", maxSeen)
	}
}

// TestWorkerPoolBounded verifies at most Workers checks run concurrently.
func TestWorkerPoolBounded(t *testing.T) {
	reg := registry.New()
	chk := &scriptChecker{typ: "fake", release: make(chan struct{}), fallback: up()}
	reg.Register(chk)

	e, store, _ := newEngine(t, engine.Options{Registry: reg, Workers: 2})
	const monitors = 5
	for id := int64(1); id <= monitors; id++ {
		if err := e.Upsert(testMonitor(id, "fake")); err != nil {
			t.Fatal(err)
		}
	}
	for id := int64(1); id <= monitors; id++ {
		if err := e.CheckNow(id); err != nil {
			t.Fatal(err)
		}
	}

	waitFor(t, "pool saturation", func() bool { return chk.current.Load() == 2 })
	time.Sleep(20 * time.Millisecond) // give excess goroutines a chance to misbehave
	if maxSeen := chk.maxSeen.Load(); maxSeen != 2 {
		t.Fatalf("max concurrent checks = %d, want 2", maxSeen)
	}

	close(chk.release)
	waitHeartbeats(t, store, monitors)
	if maxSeen := chk.maxSeen.Load(); maxSeen > 2 {
		t.Fatalf("max concurrent checks = %d, want <= 2", maxSeen)
	}
}

// TestStateMachineIntegration walks UP → PENDING → DOWN → UP through the
// full engine: heartbeat recording, important flags, retry-interval
// scheduling and event emission.
func TestStateMachineIntegration(t *testing.T) {
	reg := registry.New()
	extra := json.RawMessage(`{"detail":"x"}`)
	downExtra := down()
	downExtra.Extra = extra
	chk := &scriptChecker{
		typ:      "fake",
		queue:    []domain.CheckResult{downExtra, down(), down(), up()},
		fallback: up(),
	}
	reg.Register(chk)

	events := &eventRecorder{}
	e, store, clk := newEngine(t, engine.Options{Registry: reg, OnEvent: events.record})

	m := testMonitor(1, "fake")
	m.Interval = 60 * time.Second
	m.RetryInterval = 5 * time.Second
	m.MaxRetries = 2
	if err := e.Upsert(m); err != nil {
		t.Fatal(err)
	}

	t0 := clk.Now()
	clk.Advance(0) // failure 1: UP → PENDING
	hbs := waitHeartbeats(t, store, 1)
	if hbs[0].Status != domain.StatusPending || !hbs[0].Important || hbs[0].Retries != 1 {
		t.Fatalf("hb1 = %+v, want pending/important/retries=1", hbs[0])
	}
	if string(hbs[0].Extra) != string(extra) {
		t.Fatalf("hb1 extra = %s, want %s", hbs[0].Extra, extra)
	}
	if hbs[0].Latency == nil {
		t.Fatal("hb1 latency missing")
	}
	// PENDING uses the shorter retry interval, drift-corrected.
	waitFor(t, "retry re-arm", func() bool {
		at, ok := clk.NextAt()
		return ok && at.Equal(t0.Add(5*time.Second))
	})

	clk.Advance(5 * time.Second) // failure 2: stays PENDING, no event
	hbs = waitHeartbeats(t, store, 2)
	if hbs[1].Status != domain.StatusPending || hbs[1].Important || hbs[1].Retries != 2 {
		t.Fatalf("hb2 = %+v, want pending/not-important/retries=2", hbs[1])
	}

	clk.Advance(5 * time.Second) // failure 3: PENDING → DOWN
	hbs = waitHeartbeats(t, store, 3)
	if hbs[2].Status != domain.StatusDown || !hbs[2].Important {
		t.Fatalf("hb3 = %+v, want down/important", hbs[2])
	}
	if st, _ := store.Status(1); st != domain.StatusDown {
		t.Fatalf("persisted status = %s, want down", st)
	}
	// DOWN returns to the normal interval.
	waitFor(t, "normal re-arm", func() bool {
		at, ok := clk.NextAt()
		return ok && at.Equal(t0.Add(10*time.Second).Add(60*time.Second))
	})

	clk.Advance(60 * time.Second) // recovery: DOWN → UP
	hbs = waitHeartbeats(t, store, 4)
	if hbs[3].Status != domain.StatusUp || !hbs[3].Important || hbs[3].Retries != 0 {
		t.Fatalf("hb4 = %+v, want up/important/retries=0", hbs[3])
	}

	waitFor(t, "events", func() bool { return len(events.all()) == 3 })
	evs := events.all()
	wantTransitions := []struct{ from, to domain.MonitorStatus }{
		{domain.StatusUp, domain.StatusPending},
		{domain.StatusPending, domain.StatusDown},
		{domain.StatusDown, domain.StatusUp},
	}
	for i, want := range wantTransitions {
		if evs[i].From != want.from || evs[i].To != want.to || evs[i].Suppressed {
			t.Fatalf("event %d = %+v, want %s→%s unsuppressed", i, evs[i], want.from, want.to)
		}
	}
}

// TestLatencyAnomalyEmitsEvent wires a detector into the engine and drives a
// monitor that stays UP but slows down: the engine must raise exactly one
// anomaly StateChange (distinct from DOWN — From == To == StatusUp with a
// non-nil Anomaly) after the configured consecutive slow checks, and exactly
// one clear event when latency returns to baseline. Warmup checks and the
// intermediate slow checks emit nothing (no spam), and no UP/DOWN transition
// event is ever produced because the monitor never leaves UP.
func TestLatencyAnomalyEmitsEvent(t *testing.T) {
	reg := registry.New()
	upLat := func(ms int) domain.CheckResult {
		return domain.CheckResult{Status: domain.StatusUp, Latency: time.Duration(ms) * time.Millisecond, Message: "OK"}
	}
	// 5 warmup (baseline ~100ms), 3 sustained spikes at 900ms (raise on the
	// third), then a return to baseline (clear).
	chk := &scriptChecker{
		typ: "fake",
		queue: []domain.CheckResult{
			upLat(100), upLat(110), upLat(90), upLat(105), upLat(95),
			upLat(900), upLat(900), upLat(900),
			upLat(100),
		},
		fallback: upLat(100),
	}
	reg.Register(chk)

	events := &eventRecorder{}
	det := anomaly.New(anomaly.Config{Sensitivity: 3, MinSamples: 5, Consecutive: 3, Alpha: 0.3})
	e, store, _ := newEngine(t, engine.Options{Registry: reg, OnEvent: events.record, Anomaly: det})
	if err := e.Upsert(testMonitor(1, "fake")); err != nil {
		t.Fatal(err)
	}

	const checks = 9
	for i := 1; i <= checks; i++ {
		checkNowAndWait(t, e, store, 1, i)
	}

	waitFor(t, "raise and clear events", func() bool { return len(events.all()) == 2 })
	evs := events.all()
	if len(evs) != 2 {
		t.Fatalf("emitted %d events, want exactly 2 (raise + clear): %+v", len(evs), evs)
	}

	raise := evs[0]
	if raise.Anomaly == nil {
		t.Fatalf("raise event carries no Anomaly payload: %+v", raise)
	}
	if raise.From != domain.StatusUp || raise.To != domain.StatusUp {
		t.Fatalf("raise event = %s→%s, want up→up (distinct from DOWN)", raise.From, raise.To)
	}
	if raise.Anomaly.Cleared {
		t.Fatal("raise event has Cleared=true, want the onset edge")
	}
	if raise.Anomaly.Deviations <= 3 {
		t.Fatalf("raise Deviations = %v, want above the sensitivity threshold", raise.Anomaly.Deviations)
	}
	if raise.Anomaly.Latency != 900*time.Millisecond {
		t.Fatalf("raise Latency = %v, want 900ms", raise.Anomaly.Latency)
	}

	clear := evs[1]
	if clear.Anomaly == nil || !clear.Anomaly.Cleared {
		t.Fatalf("clear event = %+v, want a Cleared anomaly", clear)
	}
	if clear.From != domain.StatusUp || clear.To != domain.StatusUp {
		t.Fatalf("clear event = %s→%s, want up→up", clear.From, clear.To)
	}
}

// TestAnomalyDetectionOffByDefault verifies that without a detector wired in,
// even a wild latency swing produces no anomaly event — existing monitors are
// unaffected.
func TestAnomalyDetectionOffByDefault(t *testing.T) {
	reg := registry.New()
	upLat := func(ms int) domain.CheckResult {
		return domain.CheckResult{Status: domain.StatusUp, Latency: time.Duration(ms) * time.Millisecond, Message: "OK"}
	}
	chk := &scriptChecker{typ: "fake", fallback: upLat(5000)}
	reg.Register(chk)

	events := &eventRecorder{}
	e, store, _ := newEngine(t, engine.Options{Registry: reg, OnEvent: events.record}) // no Anomaly
	if err := e.Upsert(testMonitor(1, "fake")); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 6; i++ {
		checkNowAndWait(t, e, store, 1, i)
	}
	if evs := events.all(); len(evs) != 0 {
		t.Fatalf("emitted %d events with detection off, want 0: %+v", len(evs), evs)
	}
}

// TestUpsideDown verifies inverted result interpretation.
func TestUpsideDown(t *testing.T) {
	reg := registry.New()
	chk := &scriptChecker{typ: "fake", fallback: down()}
	reg.Register(chk)

	e, store, _ := newEngine(t, engine.Options{Registry: reg})
	m := testMonitor(1, "fake")
	m.UpsideDown = true
	m.Status = domain.StatusPending
	if err := e.Upsert(m); err != nil {
		t.Fatal(err)
	}
	if err := e.CheckNow(1); err != nil {
		t.Fatal(err)
	}
	hbs := waitHeartbeats(t, store, 1)
	if hbs[0].Status != domain.StatusUp {
		t.Fatalf("upside-down DOWN result recorded as %s, want up", hbs[0].Status)
	}
}

// TestSuppression verifies parent-DOWN and maintenance-window suppression:
// events still fire (and heartbeats record) but are marked suppressed.
func TestSuppression(t *testing.T) {
	reg := registry.New()
	chk := &scriptChecker{typ: "fake", fallback: down()}
	reg.Register(chk)

	events := &eventRecorder{}
	const maintID = int64(3)
	e, store, _ := newEngine(t, engine.Options{
		Registry: reg,
		OnEvent:  events.record,
		Maintenance: engine.MaintenanceFunc(func(id int64, _ time.Time) bool {
			return id == maintID
		}),
	})

	parent := testMonitor(1, "fake")
	child := testMonitor(2, "fake")
	parentID := parent.ID
	child.ParentID = &parentID
	maint := testMonitor(maintID, "fake")
	for _, m := range []domain.Monitor{parent, child, maint} {
		if err := e.Upsert(m); err != nil {
			t.Fatal(err)
		}
	}

	// Parent goes DOWN first: its own event must not be suppressed.
	if err := e.CheckNow(parent.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "parent down", func() bool {
		st, ok := e.StatusOf(parent.ID)
		return ok && st == domain.StatusDown
	})
	evs := events.all()
	if len(evs) != 1 || evs[0].Suppressed {
		t.Fatalf("parent event = %+v, want unsuppressed", evs)
	}

	// Child failure while parent is DOWN → suppressed, heartbeat recorded.
	if err := e.CheckNow(child.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "child event", func() bool { return len(events.all()) == 2 })
	ev := events.all()[1]
	if !ev.Suppressed || ev.SuppressReason != "parent-down" {
		t.Fatalf("child event = %+v, want suppressed by parent-down", ev)
	}

	// Monitor in maintenance → suppressed with reason maintenance.
	if err := e.CheckNow(maintID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "maintenance event", func() bool { return len(events.all()) == 3 })
	ev = events.all()[2]
	if !ev.Suppressed || ev.SuppressReason != "maintenance" {
		t.Fatalf("maintenance event = %+v, want suppressed by maintenance", ev)
	}

	// Suppression never suppresses data: all three heartbeats recorded.
	if got := len(store.Heartbeats()); got != 3 {
		t.Fatalf("recorded %d heartbeats, want 3", got)
	}
}

// TestRecurringMaintenanceSuppressesAlertsNotHeartbeats wires the real
// maintenance evaluator (not a stub predicate) into the engine and pins the two
// halves of the requirement: a monitor inside a recurring window loses its
// ALERT, and keeps its heartbeat; a monitor the window is not scoped to is
// untouched. The window is scoped by monitor id, so it also covers "a window
// scoped to other monitors does not suppress".
func TestRecurringMaintenanceSuppressesAlertsNotHeartbeats(t *testing.T) {
	reg := registry.New()
	chk := &scriptChecker{typ: "fake", fallback: down()}
	reg.Register(chk)

	clk := newFakeClock()
	now := clk.Now().UTC()

	// A window on today's weekday, opening on the hour the clock reads and
	// running an hour, scoped to monitor 1 only.
	const insideID, outsideID = int64(1), int64(2)
	snap, err := maintenance.NewSnapshot([]maintenance.Window{{
		ID:       10,
		Title:    "nightly",
		Active:   true,
		Strategy: maintenance.StrategyRecurring,
		Recurrence: &maintenance.Recurrence{
			Weekdays:        []time.Weekday{now.Weekday()},
			Start:           now.Format("15:04"),
			DurationMinutes: 60,
		},
		Timezone:   "UTC",
		MonitorIDs: []int64{insideID},
	}})
	if err != nil {
		t.Fatalf("build snapshot: %v", err)
	}
	if !snap.InMaintenance(insideID, now) || snap.InMaintenance(outsideID, now) {
		t.Fatalf("snapshot precondition wrong at %s: inside=%v outside=%v",
			now, snap.InMaintenance(insideID, now), snap.InMaintenance(outsideID, now))
	}

	events := &eventRecorder{}
	e, store, _ := newEngine(t, engine.Options{
		Registry:    reg,
		Clock:       clk,
		OnEvent:     events.record,
		Maintenance: snap,
	})
	for _, id := range []int64{insideID, outsideID} {
		if err := e.Upsert(testMonitor(id, "fake")); err != nil {
			t.Fatal(err)
		}
	}

	// Inside the window: the transition still happens and is still recorded,
	// but the alert is suppressed.
	if err := e.CheckNow(insideID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "inside event", func() bool { return len(events.all()) == 1 })
	ev := events.all()[0]
	if !ev.Suppressed || ev.SuppressReason != "maintenance" {
		t.Fatalf("inside event = %+v, want suppressed by maintenance", ev)
	}

	// Outside the window's scope: alert delivered as normal.
	if err := e.CheckNow(outsideID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "outside event", func() bool { return len(events.all()) == 2 })
	if ev := events.all()[1]; ev.Suppressed {
		t.Fatalf("outside event = %+v, want unsuppressed", ev)
	}

	// Heartbeats are never suppressed: both monitors recorded one, and the
	// suppressed monitor's carries its real DOWN status.
	hbs := waitHeartbeats(t, store, 2)
	if len(hbs) != 2 {
		t.Fatalf("recorded %d heartbeats, want 2", len(hbs))
	}
	for _, hb := range hbs {
		if hb.Status != domain.StatusDown {
			t.Fatalf("heartbeat for monitor %d = %s, want down", hb.MonitorID, hb.Status)
		}
	}
}

// TestLiveReloadAPI covers Upsert validation, Remove, Pause/Resume and
// CheckNow error cases including passive monitors.
func TestLiveReloadAPI(t *testing.T) {
	reg := registry.New()
	chk := &scriptChecker{typ: "fake", fallback: up()}
	reg.Register(chk)
	reg.Register(&passiveChecker{scriptChecker{typ: "push"}})
	badCfg := errors.New("bad config")
	reg.Register(&scriptChecker{typ: "strict", validate: func(json.RawMessage) error { return badCfg }})

	e, store, clk := newEngine(t, engine.Options{Registry: reg})

	// Unknown type and invalid config are rejected.
	if err := e.Upsert(testMonitor(9, "nope")); err == nil {
		t.Fatal("Upsert with unknown type must fail")
	}
	if err := e.Upsert(testMonitor(9, "strict")); !errors.Is(err, badCfg) {
		t.Fatalf("Upsert with invalid config = %v, want wrapped %v", err, badCfg)
	}

	// Passive monitors are registered but never scheduled.
	if err := e.Upsert(testMonitor(5, "push")); err != nil {
		t.Fatal(err)
	}
	if _, armed := clk.NextAt(); armed {
		t.Fatal("passive monitor must not arm a timer")
	}
	if err := e.CheckNow(5); !errors.Is(err, engine.ErrMonitorPassive) {
		t.Fatalf("CheckNow(passive) = %v, want ErrMonitorPassive", err)
	}

	// Active monitor: pause disarms and persists 'paused'.
	if err := e.Upsert(testMonitor(1, "fake")); err != nil {
		t.Fatal(err)
	}
	if err := e.Pause(1); err != nil {
		t.Fatal(err)
	}
	if _, armed := clk.NextAt(); armed {
		t.Fatal("paused monitor must not keep a timer")
	}
	if st, _ := store.Status(1); st != domain.StatusPaused {
		t.Fatalf("persisted status = %s, want paused", st)
	}
	if err := e.CheckNow(1); !errors.Is(err, engine.ErrMonitorPaused) {
		t.Fatalf("CheckNow(paused) = %v, want ErrMonitorPaused", err)
	}

	// Resume restarts at PENDING and re-arms.
	if err := e.Resume(1); err != nil {
		t.Fatal(err)
	}
	if st, _ := store.Status(1); st != domain.StatusPending {
		t.Fatalf("persisted status = %s, want pending", st)
	}
	if _, armed := clk.NextAt(); !armed {
		t.Fatal("resumed monitor must arm a timer")
	}
	if err := e.CheckNow(1); err != nil {
		t.Fatal(err)
	}
	waitHeartbeats(t, store, 1)

	// Remove takes effect immediately.
	e.Remove(1)
	if err := e.CheckNow(1); !errors.Is(err, engine.ErrUnknownMonitor) {
		t.Fatalf("CheckNow(removed) = %v, want ErrUnknownMonitor", err)
	}
	if err := e.CheckNow(404); !errors.Is(err, engine.ErrUnknownMonitor) {
		t.Fatalf("CheckNow(unknown) = %v, want ErrUnknownMonitor", err)
	}
}

// TestStartLoadsMonitorsWithJitter verifies Start schedules every stored
// active monitor with a first fire inside the jitter window.
func TestStartLoadsMonitorsWithJitter(t *testing.T) {
	reg := registry.New()
	reg.Register(&scriptChecker{typ: "fake", fallback: up()})

	clk := newFakeClock()
	store := engine.NewMemStore(testMonitor(1, "fake"), testMonitor(2, "fake"))
	e := engine.New(engine.Options{
		Registry:         reg,
		Store:            store,
		Clock:            clk,
		Workers:          2,
		MaxStartupJitter: 10 * time.Second,
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), testutil.Wait(2*time.Second))
		defer cancel()
		_ = e.Stop(ctx)
	})

	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{1, 2} {
		if _, ok := e.StatusOf(id); !ok {
			t.Fatalf("monitor %d not loaded", id)
		}
	}
	at, ok := clk.NextAt()
	if !ok {
		t.Fatal("no timer armed after Start")
	}
	t0 := clk.Now()
	if at.Before(t0) || !at.Before(t0.Add(10*time.Second)) {
		t.Fatalf("first fire %v outside jitter window [%v, %v)", at, t0, t0.Add(10*time.Second))
	}
}

// TestStopDrainsInFlightChecks verifies Stop waits for running checks and
// times out (with error) when they exceed the deadline.
func TestStopDrainsInFlightChecks(t *testing.T) {
	reg := registry.New()
	chk := &scriptChecker{typ: "fake", release: make(chan struct{}), fallback: up()}
	reg.Register(chk)

	e, store, _ := newEngine(t, engine.Options{Registry: reg})
	if err := e.Upsert(testMonitor(1, "fake")); err != nil {
		t.Fatal(err)
	}
	if err := e.CheckNow(1); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "check started", func() bool { return chk.started.Load() == 1 })

	go func() {
		time.Sleep(30 * time.Millisecond)
		close(chk.release)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), testutil.Wait(2*time.Second))
	defer cancel()
	if err := e.Stop(ctx); err != nil {
		t.Fatalf("Stop = %v, want nil (drained)", err)
	}
	// The drained check persisted its heartbeat before shutdown finished.
	if got := len(store.Heartbeats()); got != 1 {
		t.Fatalf("recorded %d heartbeats, want 1", got)
	}
}

func TestStopTimesOutOnStuckCheck(t *testing.T) {
	reg := registry.New()
	// This checker only unblocks on context cancellation.
	chk := &scriptChecker{typ: "fake", release: make(chan struct{}), fallback: up()}
	reg.Register(chk)

	clk := newFakeClock()
	e := engine.New(engine.Options{
		Registry:         reg,
		Store:            engine.NewMemStore(),
		Clock:            clk,
		Workers:          1,
		MaxStartupJitter: -1,
	})
	if err := e.Upsert(testMonitor(1, "fake")); err != nil {
		t.Fatal(err)
	}
	if err := e.CheckNow(1); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "check started", func() bool { return chk.started.Load() == 1 })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := e.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop = %v, want DeadlineExceeded", err)
	}
}

// ---------------------------------------------------------------------------
// Engine-race regressions (todo §? P1): Pause / live-update vs. an in-flight
// check. These are deterministic generation tests — `go test -race` is not
// available in this environment (no cgo/gcc), so the invariants are pinned
// with the fake clock instead of the race detector.
// ---------------------------------------------------------------------------

// TestPauseDiscardsInFlightCheck pins the first race: a check that started
// before Pause must not persist a heartbeat, must not write a status and must
// not emit a StateChange once the monitor has been paused. The engine advances
// the job's generation on Pause; the completing check belongs to the previous
// generation and is discarded wholesale.
func TestPauseDiscardsInFlightCheck(t *testing.T) {
	reg := registry.New()
	// down() would drive UP -> DOWN (MaxRetries 0) and emit an event if the
	// stale result were applied.
	chk := &scriptChecker{typ: "fake", release: make(chan struct{}), fallback: down()}
	reg.Register(chk)

	events := &eventRecorder{}
	e, store, clk := newEngine(t, engine.Options{Registry: reg, OnEvent: events.record})
	if err := e.Upsert(testMonitor(1, "fake")); err != nil {
		t.Fatal(err)
	}

	clk.Advance(0) // fire the check; it blocks inside the checker
	waitFor(t, "check started", func() bool { return chk.started.Load() == 1 })

	// Pause while the check is still blocked. This is the racing window: the
	// worker holds a DOWN result it is about to persist.
	if err := e.Pause(1); err != nil {
		t.Fatal(err)
	}

	// Let the in-flight check finish; drain deterministically via Stop, which
	// waits for the worker goroutine.
	close(chk.release)
	ctx, cancel := context.WithTimeout(context.Background(), testutil.Wait(2*time.Second))
	defer cancel()
	if err := e.Stop(ctx); err != nil {
		t.Fatalf("Stop = %v, want nil (drained)", err)
	}

	if got := len(store.Heartbeats()); got != 0 {
		t.Fatalf("recorded %d heartbeats after pause, want 0 (stale generation discarded)", got)
	}
	if evs := events.all(); len(evs) != 0 {
		t.Fatalf("emitted %d events after pause, want 0: %+v", len(evs), evs)
	}
	if st, _ := store.Status(1); st != domain.StatusPaused {
		t.Fatalf("persisted status = %s, want paused (not overwritten by stale check)", st)
	}
	if st, ok := e.StatusOf(1); !ok || st != domain.StatusPaused {
		t.Fatalf("engine status = %v (ok=%v), want paused", st, ok)
	}
}

// TestUpsertDuringCheckKeepsSingleTimer pins the second race: an Upsert that
// lands while a check is running must not leave two timers armed. Before the
// fix Upsert armed one timer and the completing check armed another; here the
// running check is the single timer owner and Upsert defers to it.
func TestUpsertDuringCheckKeepsSingleTimer(t *testing.T) {
	reg := registry.New()
	chk := &scriptChecker{typ: "fake", release: make(chan struct{}), fallback: up()}
	reg.Register(chk)

	e, store, clk := newEngine(t, engine.Options{Registry: reg})
	m := testMonitor(1, "fake")
	m.Interval = 10 * time.Second
	if err := e.Upsert(m); err != nil {
		t.Fatal(err)
	}

	clk.Advance(0) // fire; the check blocks
	waitFor(t, "check started", func() bool { return chk.started.Load() == 1 })

	// Live-reload the configuration while the check runs.
	m.Interval = 30 * time.Second
	if err := e.Upsert(m); err != nil {
		t.Fatal(err)
	}

	// Complete the check; it re-arms exactly one timer.
	close(chk.release)
	waitHeartbeats(t, store, 1)
	waitFor(t, "re-arm", func() bool { _, ok := clk.NextAt(); return ok })
	// Give any second (buggy) timer a chance to appear before asserting.
	time.Sleep(20 * time.Millisecond)
	if n := clk.Armed(); n != 1 {
		t.Fatalf("armed %d timers after upsert-during-check, want 1 (single owner)", n)
	}
}

// TestResumeDuringInFlightCheckRearmsOnce covers Pause-then-Resume while a
// check from before the pause is still running: the stale check must not
// re-arm, yet the monitor must end up scheduled exactly once (Resume must not
// lose the timer to the stale worker).
func TestResumeDuringInFlightCheckRearmsOnce(t *testing.T) {
	reg := registry.New()
	chk := &scriptChecker{typ: "fake", release: make(chan struct{}), fallback: up()}
	reg.Register(chk)

	e, _, clk := newEngine(t, engine.Options{Registry: reg})
	if err := e.Upsert(testMonitor(1, "fake")); err != nil {
		t.Fatal(err)
	}

	clk.Advance(0)
	waitFor(t, "check started", func() bool { return chk.started.Load() == 1 })

	// Pause then Resume while the original check is still blocked.
	if err := e.Pause(1); err != nil {
		t.Fatal(err)
	}
	if err := e.Resume(1); err != nil {
		t.Fatal(err)
	}

	// The stale check completes now; it must neither persist nor re-arm a
	// second timer on top of the one Resume is responsible for.
	close(chk.release)
	waitFor(t, "re-arm after resume", func() bool { _, ok := clk.NextAt(); return ok })
	time.Sleep(20 * time.Millisecond)
	if n := clk.Armed(); n != 1 {
		t.Fatalf("armed %d timers after pause/resume during check, want 1", n)
	}
	if st, ok := e.StatusOf(1); !ok || st != domain.StatusPending {
		t.Fatalf("engine status = %v (ok=%v), want pending after resume", st, ok)
	}
}

// TestConcurrentLifecycleStable hammers Pause/Resume/Upsert/CheckNow against a
// live clock and asserts the engine stays consistent (no panic, no deadlock,
// every monitor still known). With `go test -race` it would also flag data
// races; without it, it still exercises the generation/timer-ownership paths
// under contention.
func TestConcurrentLifecycleStable(t *testing.T) {
	reg := registry.New()
	chk := &scriptChecker{typ: "fake", fallback: up()}
	reg.Register(chk)

	e, _, clk := newEngine(t, engine.Options{Registry: reg, Workers: 8})
	const monitors = 5
	for id := int64(1); id <= monitors; id++ {
		if err := e.Upsert(testMonitor(id, "fake")); err != nil {
			t.Fatal(err)
		}
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// A goroutine driving the clock so timers actually fire.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				clk.Advance(500 * time.Millisecond)
				time.Sleep(time.Millisecond)
			}
		}
	}()

	for id := int64(1); id <= monitors; id++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = e.CheckNow(id)
				_ = e.Pause(id)
				m := testMonitor(id, "fake")
				m.Interval = time.Duration(1+id) * time.Second
				_ = e.Upsert(m)
				_ = e.Resume(id)
			}
		}()
	}

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()

	for id := int64(1); id <= monitors; id++ {
		if _, ok := e.StatusOf(id); !ok {
			t.Fatalf("monitor %d lost after concurrent churn", id)
		}
	}
}

// passiveScriptChecker is a scriptChecker that also declares
// registry.PassiveChecker, so the engine treats it as fed by the push endpoint
// and its watchdog rather than by timers.
type passiveScriptChecker struct{ scriptChecker }

func (*passiveScriptChecker) Passive() {}

// TestUpsertToPassiveDuringCheckStopsScheduling covers a monitor converted to a
// PASSIVE type while a check is in flight — the one path that reaches the
// completion handler with a timer still to re-arm.
//
// The re-arm block tested only removed/paused/stopping, and fire() likewise, so
// the scheduler went on polling a push monitor forever. pushcheck.Check reports
// UP unconditionally, so those synthetic beats fight the watchdog and mask a
// push monitor that has genuinely stopped reporting.
func TestUpsertToPassiveDuringCheckStopsScheduling(t *testing.T) {
	reg := registry.New()
	active := &scriptChecker{typ: "fake", release: make(chan struct{}), fallback: up()}
	reg.Register(active)
	reg.Register(&passiveScriptChecker{scriptChecker{typ: "passive", fallback: up()}})

	e, _, clk := newEngine(t, engine.Options{Registry: reg})
	m := testMonitor(1, "fake")
	if err := e.Upsert(m); err != nil {
		t.Fatal(err)
	}
	clk.Advance(0) // fire; the check blocks inside the checker
	waitFor(t, "check started", func() bool { return active.started.Load() == 1 })

	// Convert to the passive type mid-check. The monitor stays ACTIVE, so only
	// the passive flag can stop the schedule.
	m.Type = "passive"
	if err := e.Upsert(m); err != nil {
		t.Fatal(err)
	}

	// The stale check completes and must not re-arm anything.
	close(active.release)
	waitFor(t, "check finished", func() bool { return active.current.Load() == 0 })
	time.Sleep(30 * time.Millisecond) // let a buggy re-arm appear before asserting
	if n := clk.Armed(); n != 0 {
		t.Fatalf("armed %d timers for a passive monitor, want 0 (it is fed by the push endpoint)", n)
	}
	if got := active.started.Load(); got != 1 {
		t.Fatalf("active checker ran %d times, want 1 (no further polling after the switch)", got)
	}
}

// TestUpsertChangingTheTargetDiscardsTheInFlightResult covers a config edit
// that lands while a check is already running.
//
// The generation was only advanced when a monitor was paused, resumed, removed
// or deactivated, so an edit that changed the CHECK ITSELF left the in-flight
// result valid: the answer from the OLD url was written as this monitor's
// heartbeat and could drive its status and its alert, even though the monitor
// no longer pointed there.
func TestUpsertChangingTheTargetDiscardsTheInFlightResult(t *testing.T) {
	reg := registry.New()
	chk := &scriptChecker{typ: "fake", release: make(chan struct{}), fallback: down()}
	reg.Register(chk)

	e, store, clk := newEngine(t, engine.Options{Registry: reg})
	m := testMonitor(1, "fake")
	m.Config = json.RawMessage(`{"url":"https://old.example"}`)
	if err := e.Upsert(m); err != nil {
		t.Fatal(err)
	}
	clk.Advance(0)
	waitFor(t, "check started", func() bool { return chk.started.Load() == 1 })

	// Re-point the monitor while its check is in flight.
	m.Config = json.RawMessage(`{"url":"https://new.example"}`)
	if err := e.Upsert(m); err != nil {
		t.Fatal(err)
	}

	close(chk.release)
	waitFor(t, "re-arm", func() bool { _, ok := clk.NextAt(); return ok })
	time.Sleep(20 * time.Millisecond)

	if beats := store.Heartbeats(); len(beats) != 0 {
		t.Fatalf("the old target's result was recorded as a heartbeat: %+v", beats)
	}
	if st, _ := e.StatusOf(1); st == domain.StatusDown {
		t.Fatal("the old target's failure drove the re-pointed monitor DOWN")
	}
}

// The complement, and the reason the check is on the config rather than on
// "any Upsert": a rename or an interval change leaves the running check
// perfectly valid, so its result must still count.
func TestUpsertLeavingTheTargetAloneKeepsTheInFlightResult(t *testing.T) {
	reg := registry.New()
	chk := &scriptChecker{typ: "fake", release: make(chan struct{}), fallback: up()}
	reg.Register(chk)

	e, store, clk := newEngine(t, engine.Options{Registry: reg})
	m := testMonitor(1, "fake")
	if err := e.Upsert(m); err != nil {
		t.Fatal(err)
	}
	clk.Advance(0)
	waitFor(t, "check started", func() bool { return chk.started.Load() == 1 })

	m.Name = "renamed"
	m.Interval = 30 * time.Second
	if err := e.Upsert(m); err != nil {
		t.Fatal(err)
	}

	close(chk.release)
	waitHeartbeats(t, store, 1) // the result still counts
}

// panicChecker stands in for any checker, or any protocol library behind one,
// hitting a nil map or a slice bound on a malformed response.
type panicChecker struct{ started atomic.Int32 }

func (p *panicChecker) Type() string                 { return "panicky" }
func (*panicChecker) Validate(json.RawMessage) error { return nil }
func (p *panicChecker) Check(context.Context, json.RawMessage) (domain.CheckResult, error) {
	p.started.Add(1)
	panic("malformed response from the monitored target")
}

// TestAPanickingCheckerIsContained
//
// checker.Check runs on a worker goroutine, and an unrecovered panic in ANY
// goroutine ends the Go process. So a single panicking check used to terminate
// the whole monitoring system — and the input that triggers one comes from the
// monitored target, which is by definition a system that misbehaves.
//
// Before the fix this test did not fail, it CRASHED the test binary.
func TestAPanickingCheckerIsContained(t *testing.T) {
	reg := registry.New()
	chk := &panicChecker{}
	reg.Register(chk)

	e, store, _ := newEngine(t, engine.Options{Registry: reg})
	if err := e.Upsert(testMonitor(1, "panicky")); err != nil {
		t.Fatal(err)
	}
	if err := e.CheckNow(1); err != nil {
		t.Fatalf("CheckNow: %v", err)
	}

	// The panic becomes an ordinary DOWN, so the heartbeat, the state machine
	// and the re-arm all run exactly as they do for a refused connection.
	hbs := waitHeartbeats(t, store, 1)
	if hbs[0].Status != domain.StatusDown {
		t.Fatalf("status = %s, want down", hbs[0].Status)
	}
	if !strings.Contains(hbs[0].Message, "panicked") {
		t.Fatalf("message = %q, want it to name the panic", hbs[0].Message)
	}

	// And the monitor is not wedged. Containment that leaves the job marked
	// running forever is a quieter version of the same outage: this monitor
	// would never be checked again.
	checkNowAndWait(t, e, store, 1, 2)
	if got := chk.started.Load(); got != 2 {
		t.Fatalf("the checker ran %d times, want 2 — the job was left running", got)
	}
}

// A passive monitor's status must come from the row, not from the engine.
//
// The engine never checks a push monitor, so nothing here writes j.status for
// one after Upsert created the job — the push recorder writes the database row
// instead and never touches e.jobs. Answering StatusOf with the frozen value
// meant every reader that overlays it (decorateStatus, the badge, the public
// status page, /metrics) replaced the true row status with whatever the monitor
// was when its job was created. A push monitor the watchdog had tripped DOWN
// read as healthy everywhere.
func TestStatusOfDeclinesToAnswerForAPassiveMonitor(t *testing.T) {
	reg := registry.New()
	reg.Register(&scriptChecker{typ: "fake", fallback: up()})
	reg.Register(&passiveChecker{scriptChecker{typ: "push"}})
	e, _, _ := newEngine(t, engine.Options{Registry: reg})

	if err := e.Upsert(testMonitor(1, "fake")); err != nil {
		t.Fatalf("Upsert(active): %v", err)
	}
	if err := e.Upsert(testMonitor(2, "push")); err != nil {
		t.Fatalf("Upsert(passive): %v", err)
	}

	if _, ok := e.StatusOf(1); !ok {
		t.Error("the engine declines to report on a monitor it actually checks")
	}
	if st, ok := e.StatusOf(2); ok {
		t.Errorf("the engine reported %q for a passive monitor; it never updates that "+
			"value, so every caller would overlay it onto the row that holds the truth", st)
	}
}

// An outage that begins under suppression must still be announced once the
// suppression lifts.
//
// This was the sharpest hole in the alerting path. The dispatcher drops a
// suppressed DOWN before processDown runs, so no outage is tracked, no resend
// timer is armed and no escalation is scheduled — and the engine emits nothing
// further while a monitor stays DOWN, because staying DOWN is not a transition.
// A monitor that failed inside a maintenance window was therefore never alerted
// on, not even an hour later when the window had closed and it was still down.
func TestAnOutageThatStartsInMaintenanceIsAnnouncedWhenTheWindowCloses(t *testing.T) {
	reg := registry.New()
	chk := &scriptChecker{typ: "fake", fallback: down()}
	reg.Register(chk)

	var inWindow atomic.Bool
	inWindow.Store(true)
	rec := &eventRecorder{}
	e, _, clk := newEngine(t, engine.Options{
		Registry:    reg,
		Maintenance: engine.MaintenanceFunc(func(int64, time.Time) bool { return inWindow.Load() }),
		OnEvent:     rec.record,
	})

	if err := e.Upsert(testMonitor(1, "fake")); err != nil {
		t.Fatal(err)
	}
	// First check: the monitor goes DOWN inside the window.
	clk.Advance(2 * time.Minute)
	waitFor(t, "the first DOWN", func() bool { return len(rec.all()) >= 1 })
	if ev := rec.all()[0]; !ev.Suppressed {
		t.Fatalf("the first DOWN was not suppressed (%+v); this test cannot prove anything", ev)
	}

	// Still down, still in the window: nothing new to say.
	clk.Advance(2 * time.Minute)
	clk.Advance(2 * time.Minute)
	if n := len(rec.all()); n != 1 {
		t.Fatalf("events while still suppressed = %d, want 1", n)
	}

	// The window closes. The monitor is still down.
	inWindow.Store(false)
	clk.Advance(2 * time.Minute)
	waitFor(t, "the re-announcement", func() bool { return len(rec.all()) >= 2 })

	ev := rec.all()[1]
	if ev.Suppressed {
		t.Errorf("the re-announcement is still marked suppressed: %+v", ev)
	}
	if ev.To != domain.StatusDown {
		t.Errorf("re-announcement To = %q, want down (the dispatcher routes on it)", ev.To)
	}

	// And it is announced ONCE, not on every subsequent check.
	clk.Advance(2 * time.Minute)
	clk.Advance(2 * time.Minute)
	if n := len(rec.all()); n != 2 {
		t.Errorf("events after the window closed = %d, want 2 — the outage must be announced once, not per check", n)
	}
}

// A PASSIVE parent still suppresses its children, from the row.
//
// StatusOf declines to answer for a passive job because its in-memory status is
// frozen — but `suppression` reads the job map directly, so simply skipping a
// passive parent would have been the other half-fix: a push-monitored gateway
// going down would then page for every child instead of once for the parent.
// The row is the authority for a push monitor, and the engine holds the monitor
// it was last Upserted with.
func TestAPassiveParentStillSuppressesItsChildren(t *testing.T) {
	reg := registry.New()
	reg.Register(&scriptChecker{typ: "fake", fallback: down()})
	reg.Register(&passiveChecker{scriptChecker{typ: "push"}})
	rec := &eventRecorder{}
	e, _, clk := newEngine(t, engine.Options{Registry: reg, OnEvent: rec.record})

	parent := testMonitor(1, "push")
	parent.Status = domain.StatusDown
	if err := e.Upsert(parent); err != nil {
		t.Fatal(err)
	}
	child := testMonitor(2, "fake")
	pid := parent.ID
	child.ParentID = &pid
	if err := e.Upsert(child); err != nil {
		t.Fatal(err)
	}

	clk.Advance(2 * time.Minute)
	waitFor(t, "the child's DOWN", func() bool { return len(rec.all()) >= 1 })
	ev := rec.all()[0]
	if !ev.Suppressed || ev.SuppressReason != "parent-down" {
		t.Errorf("child event = %+v, want suppressed by parent-down: a push parent that is "+
			"DOWN must still silence its children", ev)
	}
}
