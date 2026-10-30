package notify

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/notify/fake"
	"github.com/Ariazonaa/statwarden/internal/notify/registry"
	"github.com/Ariazonaa/statwarden/internal/testutil"
)

// ---------------------------------------------------------------------------
// Test harness
// ---------------------------------------------------------------------------

type harness struct {
	d     *Dispatcher
	store *MemStore
	clk   *fakeClock
	// outboxClockSkew accumulates across expireOutboxLeases calls.
	outboxClockSkew time.Duration
	reg             *registry.Registry
}

func newHarness(t *testing.T, opts Options) *harness {
	t.Helper()
	store := NewMemStore()
	clk := newFakeClock()
	reg := registry.New()
	opts.Store = store
	opts.Clock = clk
	opts.Registry = reg
	d := New(opts)
	d.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), testutil.Wait(2*time.Second))
		defer cancel()
		_ = d.Stop(ctx)
	})
	return &harness{d: d, store: store, clk: clk, reg: reg}
}

// addChannel registers a fake provider and returns it.
func (h *harness) addChannel(t *testing.T, name string) *fake.Notifier {
	t.Helper()
	n := fake.New(name)
	h.reg.Register(n)
	return n
}

// assign wires channels to a monitor at the given escalation delays.
func (h *harness) assign(monitorID int64, chans ...assignSpec) {
	var as []Assignment
	for _, c := range chans {
		as = append(as, Assignment{
			Notification:    Notification{ID: c.id, Name: c.typ, Type: c.typ, Active: true},
			EscalationAfter: c.escalation,
		})
	}
	h.store.SetAssignments(monitorID, as...)
}

type assignSpec struct {
	id         int64
	typ        string
	escalation time.Duration
}

// expireOutboxLeases makes every stored row due again, standing in for the
// passage of time. Rows are leased on insert, so without this a sweep in the
// same instant correctly finds nothing to do.
//
// Each call moves the store's clock FURTHER forward. A fixed instant works
// once and then silently stops working: claiming a row re-leases it to
// that instant plus the lease, which is already past the frozen clock, so
// every later sweep finds nothing and a test that expects several retries
// quietly gets one.
func (h *harness) expireOutboxLeases() {
	h.outboxClockSkew += 24 * time.Hour
	at := time.Now().Add(h.outboxClockSkew)
	h.store.SetOutboxClock(func() time.Time { return at })
}

// drain flushes the dispatch queue: a sentinel task runs only after every
// task already queued has completed.
func (h *harness) drain(t *testing.T) {
	t.Helper()
	drainDispatcher(t, h.d)
}

// drainDispatcher waits until everything accepted so far has been processed. It
// takes a bare Dispatcher so tests that build one without the harness can use it.
func drainDispatcher(t *testing.T, d *Dispatcher) {
	t.Helper()
	// Events now reach the dispatch queue via the recorder goroutine, so a
	// marker enqueued directly could overtake an event that has been accepted
	// but not yet recorded. Wait for the recorder to catch up first.
	d.inflight.Wait()
	done := make(chan struct{})
	d.enqueue(func(context.Context) { close(done) })
	select {
	case <-done:
	case <-time.After(testutil.Wait(2 * time.Second)):
		t.Fatal("dispatcher did not drain in time")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testutil.Wait(2 * time.Second))
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func downEvent(id int64, name string) domain.StateChange {
	return domain.StateChange{MonitorID: id, MonitorName: name, From: domain.StatusPending, To: domain.StatusDown}
}

func upEvent(id int64, name string) domain.StateChange {
	return domain.StateChange{MonitorID: id, MonitorName: name, From: domain.StatusDown, To: domain.StatusUp}
}

// anomalyEvent is what the engine emits for a response-time anomaly: up→up with
// the payload attached, never a DOWN.
func anomalyEvent(id int64, name string, cleared bool) domain.StateChange {
	return domain.StateChange{
		MonitorID:   id,
		MonitorName: name,
		From:        domain.StatusUp,
		To:          domain.StatusUp,
		Message:     "response time 1.4s is 4.2σ above baseline 180ms",
		Anomaly: &domain.LatencyAnomaly{
			Cleared:    cleared,
			Latency:    1400 * time.Millisecond,
			Baseline:   180 * time.Millisecond,
			Deviations: 4.2,
		},
	}
}

// ---------------------------------------------------------------------------
// Response-time anomalies
// ---------------------------------------------------------------------------

// A latency anomaly must actually be delivered.
//
// processStateChange switched on the transition only, so an up→up event fell into
// the "state change without alert" arm: the anomaly was detected, logged, written
// to the outbox and then deleted undelivered. Nothing in this package referenced
// the payload at all, and there was no event kind for it.
func TestLatencyAnomalyIsDelivered(t *testing.T) {
	h := newHarness(t, Options{})
	ch := h.addChannel(t, "c")
	h.assign(1, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API"})

	h.d.HandleStateChange(anomalyEvent(1, "API", false))
	h.drain(t)

	if ch.Count() != 1 {
		t.Fatalf("anomaly deliveries = %d, want 1 — the alert was dropped", ch.Count())
	}
	got := ch.Sent()[0]
	if got.Kind != domain.EventKindLatencyAnomaly {
		t.Fatalf("kind = %q, want %q", got.Kind, domain.EventKindLatencyAnomaly)
	}
	// It must not read as an outage: an anomaly means the monitor is still up.
	if got.Status != domain.StatusUp {
		t.Fatalf("status = %q, want %q", got.Status, domain.StatusUp)
	}
	if !strings.Contains(got.Body, "still UP") {
		t.Fatalf("body does not say the monitor is still up: %q", got.Body)
	}
	if !strings.Contains(got.Body, "1.4s") || !strings.Contains(got.Body, "180ms") {
		t.Fatalf("body carries neither the latency nor the baseline: %q", got.Body)
	}
	if pending := h.store.PendingOutbox(); len(pending) != 0 {
		t.Fatalf("a delivered anomaly left %+v", pending)
	}
}

// The recovery edge is its own kind, so a channel can template it separately.
func TestLatencyRecoveryIsDeliveredAsItsOwnKind(t *testing.T) {
	h := newHarness(t, Options{})
	ch := h.addChannel(t, "c")
	h.assign(1, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API"})

	h.d.HandleStateChange(anomalyEvent(1, "API", true))
	h.drain(t)

	if ch.Count() != 1 {
		t.Fatalf("recovery deliveries = %d, want 1", ch.Count())
	}
	if got := ch.Sent()[0]; got.Kind != domain.EventKindLatencyNormal {
		t.Fatalf("kind = %q, want %q", got.Kind, domain.EventKindLatencyNormal)
	}
}

// An anomaly must not make a channel eligible for a recovery notification: the
// monitor was never down, so there is no outage to resolve. Were the anomaly to
// land in downState.notified, the next genuine recovery would resolve an outage
// nobody had been told about — the defect the per-channel tracking exists to
// prevent.
func TestAnomalyDoesNotRegisterAnOutage(t *testing.T) {
	h := newHarness(t, Options{})
	ch := h.addChannel(t, "c")
	h.assign(1, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API"})

	h.d.HandleStateChange(anomalyEvent(1, "API", false))
	h.drain(t)

	h.d.mu.Lock()
	tracked := h.d.down[1] != nil
	h.d.mu.Unlock()
	if tracked {
		t.Fatal("an anomaly registered an outage; the monitor is UP")
	}

	// A recovery with no tracked outage alerts nobody, so the count stays at the
	// single anomaly message.
	h.d.HandleStateChange(upEvent(1, "API"))
	h.drain(t)
	if ch.Count() != 1 {
		t.Fatalf("deliveries = %d, want 1 — a recovery was sent for an outage that "+
			"never happened", ch.Count())
	}
}

// Maintenance silences a slow-response alert for the same reason it silences a
// DOWN, and the event is finished with rather than retried.
func TestSuppressedAnomalyIsNotDelivered(t *testing.T) {
	h := newHarness(t, Options{})
	ch := h.addChannel(t, "c")
	h.assign(1, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API"})

	ev := anomalyEvent(1, "API", false)
	ev.Suppressed, ev.SuppressReason = true, "maintenance"
	h.d.HandleStateChange(ev)
	h.drain(t)

	if ch.Count() != 0 {
		t.Fatalf("a suppressed anomaly was delivered %d time(s)", ch.Count())
	}
	if pending := h.store.PendingOutbox(); len(pending) != 0 {
		t.Fatalf("a suppressed anomaly was kept for retry: %+v", pending)
	}
}

// ---------------------------------------------------------------------------
// Fan-out isolation
// ---------------------------------------------------------------------------

func TestFanOutIsolation(t *testing.T) {
	h := newHarness(t, Options{})
	good := h.addChannel(t, "good")
	slow := h.addChannel(t, "slow")
	block := make(chan struct{})
	slow.BlockOn(block)

	h.assign(1, assignSpec{id: 1, typ: "good"}, assignSpec{id: 2, typ: "slow"})
	h.d.HandleStateChange(downEvent(1, "API"))

	// The good channel must deliver while the slow one is still blocked —
	// proving a slow provider does not hold up the others.
	waitFor(t, func() bool { return good.Count() == 1 })
	if slow.Count() != 0 {
		t.Fatal("slow channel should not have completed yet")
	}
	close(block) // let the slow one finish
	waitFor(t, func() bool { return slow.Count() == 1 })

	h.drain(t)
	if got := len(h.store.Deliveries()); got != 2 {
		t.Fatalf("audit log entries = %d, want 2", got)
	}
}

func TestFanOutFailureIsolation(t *testing.T) {
	h := newHarness(t, Options{})
	good := h.addChannel(t, "good")
	bad := h.addChannel(t, "bad")
	bad.FailWith(errors.New("boom"))

	h.assign(1, assignSpec{id: 1, typ: "good"}, assignSpec{id: 2, typ: "bad"})
	h.d.HandleStateChange(downEvent(1, "API"))
	h.drain(t)

	if good.Count() != 1 {
		t.Fatalf("good deliveries = %d, want 1", good.Count())
	}
	// The failure is recorded, and the good channel is unaffected.
	var success, failed int
	for _, d := range h.store.Deliveries() {
		switch d.Status {
		case DeliverySuccess:
			success++
		case DeliveryFailed:
			failed++
			if d.Error == "" {
				t.Fatal("failed delivery must record an error")
			}
		}
	}
	if success != 1 || failed != 1 {
		t.Fatalf("success=%d failed=%d, want 1/1", success, failed)
	}
}

// ---------------------------------------------------------------------------
// Suppression
// ---------------------------------------------------------------------------

func TestSuppressedEventNotDelivered(t *testing.T) {
	h := newHarness(t, Options{})
	ch := h.addChannel(t, "c")
	h.assign(1, assignSpec{id: 1, typ: "c"})

	ev := downEvent(1, "API")
	ev.Suppressed = true
	ev.SuppressReason = "maintenance"
	h.d.HandleStateChange(ev)
	h.drain(t)

	if ch.Count() != 0 {
		t.Fatalf("suppressed event delivered %d times, want 0", ch.Count())
	}
	if len(h.store.Deliveries()) != 0 {
		t.Fatal("suppressed event must not produce audit entries")
	}
}

// ---------------------------------------------------------------------------
// Resend interval
// ---------------------------------------------------------------------------

func TestResendWhileDown(t *testing.T) {
	h := newHarness(t, Options{})
	ch := h.addChannel(t, "c")
	h.assign(1, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API", ResendInterval: 3 * time.Minute})

	h.d.HandleStateChange(downEvent(1, "API"))
	h.drain(t)
	if ch.Count() != 1 {
		t.Fatalf("initial deliveries = %d, want 1", ch.Count())
	}
	if ch.Sent()[0].Reminder {
		t.Fatal("first alert must not be a reminder")
	}

	// Each resend interval fires one reminder.
	for i := 2; i <= 3; i++ {
		h.clk.Advance(3 * time.Minute)
		h.drain(t)
		if ch.Count() != i {
			t.Fatalf("after %d intervals: deliveries = %d, want %d", i-1, ch.Count(), i)
		}
		last := ch.Sent()[ch.Count()-1]
		if !last.Reminder {
			t.Fatal("resend must be marked Reminder")
		}
	}

	// Recovery stops the reminders.
	h.d.HandleStateChange(upEvent(1, "API"))
	h.drain(t)
	upCount := ch.Count()
	h.clk.Advance(3 * time.Minute)
	h.drain(t)
	if ch.Count() != upCount {
		t.Fatal("reminders must stop after recovery")
	}
}

// ---------------------------------------------------------------------------
// Escalation
// ---------------------------------------------------------------------------

func TestEscalation(t *testing.T) {
	h := newHarness(t, Options{})
	primary := h.addChannel(t, "primary")
	pager := h.addChannel(t, "pager")
	h.assign(1,
		assignSpec{id: 1, typ: "primary", escalation: 0},
		assignSpec{id: 2, typ: "pager", escalation: 5 * time.Minute},
	)
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API"})

	h.d.HandleStateChange(downEvent(1, "API"))
	h.drain(t)
	if primary.Count() != 1 {
		t.Fatalf("primary deliveries = %d, want 1", primary.Count())
	}
	if pager.Count() != 0 {
		t.Fatal("pager must not fire before its escalation delay")
	}

	h.clk.Advance(5 * time.Minute)
	h.drain(t)
	if pager.Count() != 1 {
		t.Fatalf("pager deliveries = %d, want 1 after escalation", pager.Count())
	}
	if !pager.Sent()[0].Escalation {
		t.Fatal("escalation delivery must be marked Escalation")
	}

	// Recovery reaches both levels that were alerted.
	h.d.HandleStateChange(upEvent(1, "API"))
	h.drain(t)
	if primary.Sent()[primary.Count()-1].Kind != domain.EventKindUp {
		t.Fatal("primary should receive the up event")
	}
	if pager.Sent()[pager.Count()-1].Kind != domain.EventKindUp {
		t.Fatal("pager should receive the up event (it was escalated)")
	}
}

// An escalation that reached nobody must be retried, like every other alert.
//
// It used to be enqueued directly with its outcome discarded, so it was the one
// alert path with no second chance: the level was flagged attempted even though
// nothing was delivered, AfterFunc had already fired, and Restore treats an
// elapsed level as done — so one 503 from the on-call provider lost the page for
// good, while the level-0 alert for the SAME outage was retried ten times.
//
// The channel is also not resolved to on recovery until it has actually heard the
// DOWN, which is the second half of the contract and is asserted at the end.
func TestAFailedEscalationIsRetriedFromTheOutbox(t *testing.T) {
	h := newHarness(t, Options{})
	primary := h.addChannel(t, "primary")
	pager := h.addChannel(t, "pager")
	h.assign(1,
		assignSpec{id: 1, typ: "primary", escalation: 0},
		assignSpec{id: 2, typ: "pager", escalation: 5 * time.Minute},
	)
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API"})

	h.d.HandleStateChange(downEvent(1, "API"))
	h.drain(t)
	if primary.Count() != 1 {
		t.Fatalf("primary deliveries = %d, want 1", primary.Count())
	}
	if pending := h.store.PendingOutbox(); len(pending) != 0 {
		t.Fatalf("the delivered DOWN left %d outbox rows: %+v", len(pending), pending)
	}

	// The on-call provider is down when the escalation fires.
	pager.FailWith(errors.New("pagerduty 503"))
	h.clk.Advance(5 * time.Minute)
	h.drain(t)
	if pager.Calls() != 1 {
		t.Fatalf("escalation send attempts = %d, want 1", pager.Calls())
	}
	if pager.Count() != 0 {
		t.Fatal("the failing provider must not count as reached")
	}
	pending := h.store.PendingOutbox()
	if len(pending) != 1 || pending[0].Kind != OutboxEscalation {
		t.Fatalf("a failed escalation left %+v, want one %s row — the page is lost",
			pending, OutboxEscalation)
	}

	// It recovers; the sweeper replays the escalation.
	pager.FailWith(nil)
	h.expireOutboxLeases()
	h.d.sweepOutbox(context.Background())
	h.drain(t)
	if pager.Count() != 1 {
		t.Fatalf("escalation deliveries after retry = %d, want 1", pager.Count())
	}
	if !pager.Sent()[0].Escalation {
		t.Fatal("the retried delivery must still be marked Escalation")
	}
	if pending := h.store.PendingOutbox(); len(pending) != 0 {
		t.Fatalf("outbox still holds %+v after a successful escalation retry", pending)
	}

	// Having finally heard the DOWN, the pager is resolved to on recovery.
	h.d.HandleStateChange(upEvent(1, "API"))
	h.drain(t)
	if last := pager.Sent()[pager.Count()-1]; last.Kind != domain.EventKindUp {
		t.Fatalf("pager's last event = %s, want %s", last.Kind, domain.EventKindUp)
	}
}

// A second run of the same level must not re-alert a channel that already heard
// it. Per-channel tracking is what replaced the level flag, so this pins that the
// replacement actually dedupes — otherwise the retry above would double-page
// everybody on a level where only one channel had failed.
func TestReplayedEscalationDoesNotRepeatItself(t *testing.T) {
	h := newHarness(t, Options{})
	pager := h.addChannel(t, "pager")
	h.assign(1, assignSpec{id: 2, typ: "pager", escalation: 5 * time.Minute})
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API"})

	h.d.HandleStateChange(downEvent(1, "API"))
	h.drain(t)
	h.clk.Advance(5 * time.Minute)
	h.drain(t)
	if pager.Count() != 1 {
		t.Fatalf("escalation deliveries = %d, want 1", pager.Count())
	}

	// Replay the same level by hand, as a lapsed lease would.
	if _, err := h.store.EnqueueOutbox(context.Background(), OutboxRecord{
		Kind:      OutboxEscalation,
		MonitorID: 1,
		Payload:   mustJSON(t, escalationEvent{MonitorID: 1, Delay: 5 * time.Minute}),
	}, 0); err != nil {
		t.Fatalf("EnqueueOutbox: %v", err)
	}
	h.d.sweepOutbox(context.Background())
	h.drain(t)

	if pager.Count() != 1 {
		t.Fatalf("escalation deliveries after replay = %d, want 1 — the replay "+
			"re-paged a channel that had already heard it", pager.Count())
	}
	if pending := h.store.PendingOutbox(); len(pending) != 0 {
		t.Fatalf("a replay with nothing left to do kept %+v", pending)
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func TestEscalationNotSentAfterRecovery(t *testing.T) {
	h := newHarness(t, Options{})
	h.addChannel(t, "primary")
	pager := h.addChannel(t, "pager")
	h.assign(1,
		assignSpec{id: 1, typ: "primary", escalation: 0},
		assignSpec{id: 2, typ: "pager", escalation: 5 * time.Minute},
	)
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API"})

	h.d.HandleStateChange(downEvent(1, "API"))
	h.drain(t)
	// Recovers before the escalation delay elapses.
	h.d.HandleStateChange(upEvent(1, "API"))
	h.drain(t)

	h.clk.Advance(5 * time.Minute)
	h.drain(t)
	if pager.Count() != 0 {
		t.Fatalf("pager fired %d times, want 0 (recovered before escalation)", pager.Count())
	}
}

// ---------------------------------------------------------------------------
// Mute / snooze
// ---------------------------------------------------------------------------

func TestMuteSuppressesThenExpires(t *testing.T) {
	h := newHarness(t, Options{})
	ch := h.addChannel(t, "c")
	h.assign(1, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API"}) // no resend

	h.d.Mute(1, h.clk.Now().Add(5*time.Minute))
	h.d.HandleStateChange(downEvent(1, "API"))
	h.drain(t)
	if ch.Count() != 0 {
		t.Fatalf("muted monitor delivered %d, want 0", ch.Count())
	}

	// Past the mute expiry the RECOVERY is still not announced: the mute ate the
	// DOWN, so "back online" would be the first this channel ever heard of the
	// outage. (This assertion is inverted from what it used to be — delivering it
	// was the bug.)
	h.clk.Advance(6 * time.Minute)
	h.d.HandleStateChange(upEvent(1, "API"))
	h.drain(t)
	if ch.Count() != 0 {
		t.Fatalf("recovery for a muted outage delivered %d, want 0", ch.Count())
	}

	// Alerting itself HAS resumed, which is what the mute expiry is about; a
	// fresh outage is the honest way to show it.
	h.d.HandleStateChange(downEvent(1, "API"))
	h.drain(t)
	if ch.Count() != 1 {
		t.Fatalf("after mute expiry a new outage delivered %d, want 1", ch.Count())
	}
}

// ---------------------------------------------------------------------------
// Quiet hours
// ---------------------------------------------------------------------------

func TestQuietHoursSuppression(t *testing.T) {
	quiet := &QuietHours{Start: "09:00", End: "17:00"}
	h := newHarness(t, Options{QuietHours: quiet})
	ch := h.addChannel(t, "c")
	h.assign(1, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API"})

	// Default fake clock is 12:00 — inside the window.
	h.d.HandleStateChange(downEvent(1, "API"))
	h.drain(t)
	if ch.Count() != 0 {
		t.Fatalf("quiet-hours alert delivered %d, want 0", ch.Count())
	}

	// Move outside the window. The recovery is still not announced — quiet hours
	// ate the DOWN, so this channel was never told there was an outage to
	// resolve. (Inverted from the old expectation; delivering it was the bug.)
	h.clk.SetTime(time.Date(2026, 7, 20, 18, 0, 0, 0, time.UTC))
	h.d.HandleStateChange(upEvent(1, "API"))
	h.drain(t)
	if ch.Count() != 0 {
		t.Fatalf("recovery for a quiet-hours outage delivered %d, want 0", ch.Count())
	}

	// A fresh alert outside the window does get through.
	h.d.HandleStateChange(downEvent(1, "API"))
	h.drain(t)
	if ch.Count() != 1 {
		t.Fatalf("outside quiet hours deliveries = %d, want 1", ch.Count())
	}
}

// A DOWN that no provider accepted must not produce a recovery either: the
// level counts as alerted only once its message actually went out.
func TestRecoveryNotSentWhenTheDownDeliveryFailed(t *testing.T) {
	h := newHarness(t, Options{})
	ch := h.addChannel(t, "c")
	h.assign(1, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API"})

	ch.FailWith(errors.New("provider unreachable"))
	h.d.HandleStateChange(downEvent(1, "API"))
	h.drain(t)
	if ch.Count() != 0 {
		t.Fatalf("a failing provider recorded %d deliveries", ch.Count())
	}

	ch.FailWith(nil) // the provider comes back
	h.d.HandleStateChange(upEvent(1, "API"))
	h.drain(t)
	if ch.Count() != 0 {
		t.Fatalf("recovery delivered %d times for an outage nobody was told about, want 0", ch.Count())
	}
}

// ---------------------------------------------------------------------------
// Digest batching
// ---------------------------------------------------------------------------

func TestDigestBatching(t *testing.T) {
	h := newHarness(t, Options{Digest: DigestOptions{Interval: 10 * time.Minute}})
	ch := h.addChannel(t, "c")
	h.assign(1, assignSpec{id: 1, typ: "c"})
	h.assign(2, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API"}) // no resend
	h.store.SetMonitorSettings(2, MonitorSettings{Name: "web"})

	h.d.HandleStateChange(downEvent(1, "API"))
	h.drain(t)
	h.d.HandleStateChange(downEvent(2, "web"))
	h.drain(t)

	if ch.Count() != 0 {
		t.Fatalf("digest mode delivered %d immediately, want 0", ch.Count())
	}
	if got := h.d.pendingDigestItems(1); got != 2 {
		t.Fatalf("pending digest items = %d, want 2", got)
	}

	h.clk.Advance(10 * time.Minute)
	h.drain(t)
	if ch.Count() != 1 {
		t.Fatalf("digest deliveries = %d, want 1", ch.Count())
	}
	digest := ch.Sent()[0]
	if digest.Kind != domain.EventKindDigest {
		t.Fatalf("delivered kind = %s, want digest", digest.Kind)
	}
	if len(digest.Items) != 2 {
		t.Fatalf("digest items = %d, want 2", len(digest.Items))
	}
}

// ---------------------------------------------------------------------------
// Cert expiry & test send
// ---------------------------------------------------------------------------

func TestCertExpiryDelivery(t *testing.T) {
	h := newHarness(t, Options{})
	ch := h.addChannel(t, "c")
	h.assign(1, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "web", URL: "https://web.example.com"})

	h.d.HandleCertExpiry(CertExpiryEvent{
		MonitorID: 1, MonitorName: "web", Issuer: "Let's Encrypt",
		ValidTo: h.clk.Now().Add(7 * 24 * time.Hour), DaysRemaining: 7,
	})
	h.drain(t)

	if ch.Count() != 1 {
		t.Fatalf("cert expiry deliveries = %d, want 1", ch.Count())
	}
	ev := ch.Sent()[0]
	if ev.Kind != domain.EventKindCertExpiry || ev.CertDaysRemaining != 7 {
		t.Fatalf("unexpected cert event: %+v", ev)
	}
}

func TestTestSend(t *testing.T) {
	h := newHarness(t, Options{})
	ch := h.addChannel(t, "c")

	err := h.d.TestSend(context.Background(), Notification{ID: 9, Name: "c", Type: "c", Active: true})
	if err != nil {
		t.Fatalf("TestSend error = %v", err)
	}
	if ch.Count() != 1 || ch.Sent()[0].Kind != domain.EventKindTest {
		t.Fatalf("test send not delivered: count=%d", ch.Count())
	}
	if len(h.store.Deliveries()) != 1 {
		t.Fatal("test send must be audit-logged")
	}
}

func TestTestSendFailureRecorded(t *testing.T) {
	h := newHarness(t, Options{})
	ch := h.addChannel(t, "c")
	ch.FailWith(errors.New("nope"))

	err := h.d.TestSend(context.Background(), Notification{ID: 9, Name: "c", Type: "c"})
	if err == nil {
		t.Fatal("TestSend must return the provider error")
	}
	got := h.store.Deliveries()
	if len(got) != 1 || got[0].Status != DeliveryFailed {
		t.Fatalf("expected one failed delivery, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Retries
// ---------------------------------------------------------------------------

func TestSendRetries(t *testing.T) {
	h := newHarness(t, Options{SendAttempts: 3, RetryDelay: 0})
	ch := h.addChannel(t, "c")
	ch.FailWith(errors.New("transient"))

	err := h.d.TestSend(context.Background(), Notification{ID: 1, Name: "c", Type: "c"})
	if err == nil {
		t.Fatal("expected failure after retries")
	}
	if ch.Calls() != 3 {
		t.Fatalf("Send called %d times, want 3 (attempts)", ch.Calls())
	}
	got := h.store.Deliveries()
	if len(got) != 1 || got[0].Attempts != 3 {
		t.Fatalf("delivery attempts = %+v, want 3", got)
	}
}

// ---------------------------------------------------------------------------
// Unknown provider
// ---------------------------------------------------------------------------

func TestUnknownProviderRecordedAsFailure(t *testing.T) {
	h := newHarness(t, Options{})
	h.assign(1, assignSpec{id: 1, typ: "ghost"}) // never registered
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API"})

	h.d.HandleStateChange(downEvent(1, "API"))
	h.drain(t)

	got := h.store.Deliveries()
	if len(got) != 1 || got[0].Status != DeliveryFailed {
		t.Fatalf("unknown provider should record a failed delivery, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Restart adoption (Dispatcher.Restore)
// ---------------------------------------------------------------------------

// wasAlerted records the delivery a previous process would have made, which is
// where Restore now reads who was told from.
//
// Seeding it is not test bookkeeping for its own sake: escalation arithmetic
// says who was DUE, the audit log says who was REACHED, and the whole point of
// the change is that those are different. A fixture that leaves the log empty
// is modelling a process that alerted nobody.
func (h *harness) wasAlerted(monitorID int64, notificationIDs ...int64) {
	for _, id := range notificationIDs {
		_ = h.store.RecordDelivery(context.Background(), Delivery{
			NotificationID: id,
			MonitorID:      monitorID,
			EventKind:      domain.EventKindDown,
			Status:         DeliverySuccess,
		})
	}
}

// The regression this whole method exists for: a restart during an outage used
// to swallow the recovery alert. The tracking map is in-memory, the engine emits
// nothing while a monitor merely STAYS down, so nothing ever rebuilt it, and
// processRecovery dropped a recovery it had no outage for.
func TestRestoreAdoptsOutageSoRecoveryStillAlerts(t *testing.T) {
	h := newHarness(t, Options{})
	primary := h.addChannel(t, "primary")
	h.assign(1, assignSpec{id: 1, typ: "primary"})
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API"})
	// The monitor went down a week ago, before this process existed, and that
	// process did alert the channel.
	h.store.SetOpenOutages(Outage{MonitorID: 1, Since: h.clk.Now().Add(-7 * 24 * time.Hour)})
	h.wasAlerted(1, 1)

	if err := h.d.Restore(context.Background()); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	h.drain(t)
	// Adopting must not alert by itself — the operator was told a week ago.
	if primary.Count() != 0 {
		t.Fatalf("Restore delivered %d notifications, want 0", primary.Count())
	}

	h.d.HandleStateChange(upEvent(1, "API"))
	h.drain(t)
	if primary.Count() != 1 {
		t.Fatalf("recovery deliveries = %d, want 1", primary.Count())
	}
	sent := primary.Sent()[0]
	if sent.Kind != domain.EventKindUp {
		t.Fatalf("kind = %q, want %q", sent.Kind, domain.EventKindUp)
	}
	if sent.DownSince == nil {
		t.Fatal("recovery must carry the adopted outage start")
	}
	if got := h.clk.Now().Sub(*sent.DownSince); got != 7*24*time.Hour {
		t.Fatalf("reported outage duration = %s, want 168h", got)
	}
}

// The complement: with no outage adopted and none seen live, a recovery stays
// silent. That is correct — nobody was alerted — and is precisely the state
// Restore exists to keep the dispatcher out of after a restart.
func TestRecoveryWithoutTrackedOutageStaysSilent(t *testing.T) {
	h := newHarness(t, Options{})
	primary := h.addChannel(t, "primary")
	h.assign(1, assignSpec{id: 1, typ: "primary"})

	h.d.HandleStateChange(upEvent(1, "API"))
	h.drain(t)
	if primary.Count() != 0 {
		t.Fatalf("deliveries = %d, want 0", primary.Count())
	}
}

// An escalation level whose delay elapsed while the process was away was
// already alerted by the previous process: it must not fire again, but it must
// still be resolved. A level still ahead fires at its REMAINING delay.
func TestRestoreMarksElapsedEscalationsFired(t *testing.T) {
	h := newHarness(t, Options{})
	primary := h.addChannel(t, "primary")
	pager := h.addChannel(t, "pager")
	late := h.addChannel(t, "late")
	h.assign(1,
		assignSpec{id: 1, typ: "primary", escalation: 0},
		assignSpec{id: 2, typ: "pager", escalation: 5 * time.Minute},
		assignSpec{id: 3, typ: "late", escalation: 2 * time.Hour},
	)
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API"})
	// Down for an hour: the 5-minute level has passed, the 2-hour one has not.
	// The previous process reached the two levels that had come due.
	h.store.SetOpenOutages(Outage{MonitorID: 1, Since: h.clk.Now().Add(-time.Hour)})
	h.wasAlerted(1, 1, 2)

	if err := h.d.Restore(context.Background()); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	h.drain(t)
	if pager.Count() != 0 || late.Count() != 0 {
		t.Fatal("Restore must not re-alert any escalation level")
	}

	// One more hour reaches the 2-hour mark measured from the outage start.
	h.clk.Advance(time.Hour)
	h.drain(t)
	if late.Count() != 1 {
		t.Fatalf("late deliveries = %d, want 1 at the remaining delay", late.Count())
	}
	if !late.Sent()[0].Escalation {
		t.Fatal("the delayed level must be marked as an escalation")
	}

	h.d.HandleStateChange(upEvent(1, "API"))
	h.drain(t)
	for name, ch := range map[string]*fake.Notifier{"primary": primary, "pager": pager, "late": late} {
		if ch.Count() == 0 || ch.Sent()[ch.Count()-1].Kind != domain.EventKindUp {
			t.Fatalf("%s should receive the recovery", name)
		}
	}
}

// TestStopWhileEnqueueWaitsForRoomDoesNotPanic covers shutdown against a full
// queue.
//
// enqueue waits for room WITHOUT holding d.mu — it cannot hold it, the dispatch
// loop takes the same mutex — so a Stop that closed the task channel underneath
// that waiting send is a "send on closed channel" panic, not merely a race.
// Shutdown is signalled on its own channel for exactly this reason and the task
// channel is never closed.
func TestStopWhileEnqueueWaitsForRoomDoesNotPanic(t *testing.T) {
	h := newHarness(t, Options{QueueSize: 1})

	blocked := make(chan struct{})
	release := make(chan struct{})
	// Occupy the dispatch loop so nothing drains the queue behind us.
	h.d.enqueue(func(context.Context) {
		close(blocked)
		<-release
	})
	<-blocked
	h.d.enqueue(func(context.Context) {}) // fills the single buffer slot

	// This one finds the queue full and parks in the slow path.
	waiting := make(chan struct{})
	go func() {
		defer close(waiting)
		h.d.enqueue(func(context.Context) {})
	}()
	// Give that goroutine a moment to reach the blocking send; the test is about
	// what happens to a send that is already parked when shutdown begins.
	time.Sleep(50 * time.Millisecond)

	stopped := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		stopped <- h.d.Stop(ctx)
	}()

	// Wait until Stop has actually begun before releasing the loop. Without this
	// the loop can drain the buffer first, the parked sender completes normally
	// and the shutdown hazard is never exercised — the ordering that matters is
	// "shutdown happens WHILE a sender is parked".
	waitFor(t, func() bool {
		h.d.mu.Lock()
		defer h.d.mu.Unlock()
		return h.d.stopped
	})

	close(release) // now let the occupied loop finish and drain
	select {
	case <-waiting:
	case <-time.After(testutil.Wait(3 * time.Second)):
		t.Fatal("a parked enqueue never returned after shutdown")
	}
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(testutil.Wait(3 * time.Second)):
		t.Fatal("Stop did not return")
	}
}

// Stop must still DRAIN what was already accepted rather than discarding it:
// those events were taken on the promise of being delivered.
func TestStopDrainsAcceptedEvents(t *testing.T) {
	h := newHarness(t, Options{})
	ch := h.addChannel(t, "c")
	h.assign(1, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API"})

	h.d.HandleStateChange(downEvent(1, "API"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := h.d.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if ch.Count() != 1 {
		t.Fatalf("deliveries = %d, want the accepted event drained on shutdown", ch.Count())
	}
}

// TestRecoveryReachesOnlyTheChannelsThatHeard is the per-channel regression.
//
// The alerted set used to be keyed by ESCALATION DELAY, which conflated a level
// with the channels sitting on it. Two channels on the same level with only one
// delivery succeeding meant both were later told "back online" — and for one of
// them that was the first it ever heard of the outage.
func TestRecoveryReachesOnlyTheChannelsThatHeard(t *testing.T) {
	h := newHarness(t, Options{})
	good := h.addChannel(t, "good")
	broken := h.addChannel(t, "broken")
	broken.FailWith(errors.New("provider unreachable"))
	// Same level (0), two channels.
	h.assign(1, assignSpec{id: 1, typ: "good"}, assignSpec{id: 2, typ: "broken"})
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API"})

	h.d.HandleStateChange(downEvent(1, "API"))
	h.drain(t)
	if good.Count() != 1 {
		t.Fatalf("the working channel got %d DOWN alerts, want 1", good.Count())
	}

	// The broken channel comes back before the recovery.
	broken.FailWith(nil)
	h.d.HandleStateChange(upEvent(1, "API"))
	h.drain(t)

	if good.Count() != 2 {
		t.Fatalf("the channel that heard the DOWN got %d messages, want 2 (down + up)", good.Count())
	}
	if broken.Count() != 0 {
		t.Fatalf("a channel that never received the DOWN was told it was resolved (%d messages)",
			broken.Count())
	}
}

// A channel ASSIGNED DURING the outage must not be resolved to either: it never
// heard the DOWN, so "back online" would be its first and only message.
func TestChannelAddedDuringOutageIsNotResolvedTo(t *testing.T) {
	h := newHarness(t, Options{})
	original := h.addChannel(t, "original")
	late := h.addChannel(t, "late")
	h.assign(1, assignSpec{id: 1, typ: "original"})
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API"})

	h.d.HandleStateChange(downEvent(1, "API"))
	h.drain(t)

	// The operator wires up a second channel while the monitor is still down.
	h.assign(1, assignSpec{id: 1, typ: "original"}, assignSpec{id: 2, typ: "late"})

	h.d.HandleStateChange(upEvent(1, "API"))
	h.drain(t)

	if original.Count() != 2 {
		t.Fatalf("the original channel got %d messages, want 2 (down + up)", original.Count())
	}
	if late.Count() != 0 {
		t.Fatalf("a channel added mid-outage was resolved to (%d messages)", late.Count())
	}
}

// The resend reminder follows the same rule as the recovery.
func TestResendReachesOnlyTheChannelsThatHeard(t *testing.T) {
	h := newHarness(t, Options{})
	good := h.addChannel(t, "good")
	broken := h.addChannel(t, "broken")
	broken.FailWith(errors.New("provider unreachable"))
	h.assign(1, assignSpec{id: 1, typ: "good"}, assignSpec{id: 2, typ: "broken"})
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API", ResendInterval: 3 * time.Minute})

	h.d.HandleStateChange(downEvent(1, "API"))
	h.drain(t)
	broken.FailWith(nil) // it recovers, but it still never heard the DOWN

	h.clk.Advance(3 * time.Minute)
	h.drain(t)
	if good.Count() != 2 {
		t.Fatalf("the reminded channel got %d messages, want 2", good.Count())
	}
	if broken.Count() != 0 {
		t.Fatalf("a channel that never heard the DOWN got a reminder (%d messages)", broken.Count())
	}
}

// hookedStore runs a hook when Assignments is called. processResend reads
// d.down, then calls MonitorSettings and Assignments, then re-takes the lock to
// re-arm — so this hook lands the test squarely in that window.
type hookedStore struct {
	*MemStore
	onAssignments func()
}

func (s *hookedStore) Assignments(ctx context.Context, monitorID int64) ([]Assignment, error) {
	if s.onAssignments != nil {
		hook := s.onAssignments
		s.onAssignments = nil // once
		hook()
	}
	return s.MemStore.Assignments(ctx, monitorID)
}

// A resend task that outlives its own outage must not re-arm anything.
//
// processResend captured the downState, then released the lock for two store
// calls, then re-armed the timer on the captured state after checking only that
// SOME entry existed for the monitor. If the outage had been replaced in the
// meantime — a recovery followed by a fresh DOWN — it armed a second chain on the
// orphaned state while the new outage kept the one it armed for itself. Both then
// fired and each re-armed one, so the duplicate reminders persisted for the rest
// of the outage.
//
// Reachable in production because the resend timer calls enqueue directly while a
// state change goes through submit → recordLoop → an outbox INSERT first, so the
// two do not queue in event order.
func TestResendDoesNotRearmAnOutageItNoLongerBelongsTo(t *testing.T) {
	base := NewMemStore()
	clk := newFakeClock()
	reg := registry.New()
	reg.Register(fake.New("c"))
	store := &hookedStore{MemStore: base}

	d := New(Options{Store: store, Clock: clk, Registry: reg})
	d.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), testutil.Wait(2*time.Second))
		defer cancel()
		_ = d.Stop(ctx)
	})

	base.SetAssignments(1, Assignment{
		Notification: Notification{ID: 1, Name: "c", Type: "c", Active: true},
	})
	base.SetMonitorSettings(1, MonitorSettings{Name: "API", ResendInterval: 3 * time.Minute})

	d.HandleStateChange(downEvent(1, "API"))
	d.inflight.Wait()
	waitFor(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.down[1] != nil
	})

	d.mu.Lock()
	st1 := d.down[1]
	firstTimer := st1.resendTimer
	d.mu.Unlock()

	// Swap the tracked outage for a different one mid-processResend, exactly as a
	// recovery plus a new DOWN would. st1's timer is deliberately left alone: the
	// point is that st1 is orphaned while a task still holds it.
	var st2 *downState
	store.onAssignments = func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		st2 = &downState{
			since:    clk.Now(),
			notified: map[int64]bool{},
		}
		st2.resendTimer = clk.AfterFunc(3*time.Minute, func() {})
		d.down[1] = st2
	}

	clk.Advance(3 * time.Minute) // fires st1's reminder, which runs processResend
	drainDispatcher(t, d)

	d.mu.Lock()
	orphanRearmed := st1.resendTimer != firstTimer
	st2Clobbered := st2 != nil && d.down[1] == st2 && st2.resendTimer == nil
	d.mu.Unlock()

	if orphanRearmed {
		t.Fatal("processResend armed a new reminder on an outage that had already " +
			"been replaced, leaving two chains for one monitor")
	}
	if st2Clobbered {
		t.Fatal("the live outage lost its own reminder timer")
	}
	// One chain, and it belongs to the live outage.
	if live := clk.LiveTimers(); live != 1 {
		t.Fatalf("live reminder timers = %d, want exactly 1 (the live outage's)", live)
	}
}

// ---------------------------------------------------------------------------
// Durable hand-off (the outbox)
// ---------------------------------------------------------------------------

// An event is recorded before it is queued and cleared once it is delivered,
// so a completed alert leaves nothing behind to be retried.
func TestDeliveredEventLeavesNoOutboxRow(t *testing.T) {
	h := newHarness(t, Options{})
	ch := h.addChannel(t, "c")
	h.assign(1, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API"})

	h.d.HandleStateChange(downEvent(1, "API"))
	h.drain(t)
	if ch.Count() != 1 {
		t.Fatalf("deliveries = %d, want 1", ch.Count())
	}
	if pending := h.store.PendingOutbox(); len(pending) != 0 {
		t.Fatalf("a delivered event left %d outbox rows: %+v", len(pending), pending)
	}
}

// downRecord is the outbox row a DOWN event produces, as HandleStateChange
// would write it. Tests that simulate "a previous process left this behind"
// insert it with a zero lease, i.e. already due.
func downRecord(t *testing.T, id int64, name string) OutboxRecord {
	t.Helper()
	payload, err := json.Marshal(downEvent(id, name))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return OutboxRecord{Kind: OutboxStateChange, MonitorID: id, Payload: payload}
}

// TestEventSurvivesAFullQueue is the reason the outbox exists.
//
// The queue is bounded on purpose, so past the bound an event used to be
// dropped outright after a short wait — at precisely the moment a fleet-wide
// incident is unfolding and the alerts matter most. It is now still recorded,
// and the retry sweep delivers it.
func TestEventSurvivesAFullQueue(t *testing.T) {
	h := newHarness(t, Options{QueueSize: 1})
	ch := h.addChannel(t, "c")
	h.assign(7, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(7, MonitorSettings{Name: "API"})

	// Occupy the dispatch loop and fill the single buffer slot behind it.
	blocked, release := make(chan struct{}), make(chan struct{})
	h.d.enqueue(func(context.Context) { close(blocked); <-release })
	<-blocked
	h.d.enqueue(func(context.Context) {})

	// This event cannot reach the queue. It must still be recorded.
	h.d.HandleStateChange(downEvent(7, "API"))
	waitFor(t, func() bool { return len(h.store.PendingOutbox()) == 1 })
	close(release)
	h.drain(t)

	// The sweep is what actually rescues it. Its lease was taken on insert, so
	// move past it the way the passage of time would.
	h.expireOutboxLeases()
	h.d.sweepOutbox(context.Background())
	h.drain(t)
	if ch.Count() != 1 {
		t.Fatalf("the rescued event was delivered %d times, want 1", ch.Count())
	}
	if pending := h.store.PendingOutbox(); len(pending) != 0 {
		t.Fatalf("outbox still holds %+v after a successful retry", pending)
	}
}

// A row left behind by a crash is picked up by the first sweep — there is no
// separate recovery path, an unfinished row IS the recovery signal.
func TestSweepDeliversWhatAPreviousProcessLeftBehind(t *testing.T) {
	h := newHarness(t, Options{})
	ch := h.addChannel(t, "c")
	h.assign(3, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(3, MonitorSettings{Name: "API"})

	// Simulate the crash: the event was accepted, nothing completed it, and its
	// lease died with the process.
	if _, err := h.store.EnqueueOutbox(context.Background(), downRecord(t, 3, "API"), 0); err != nil {
		t.Fatalf("EnqueueOutbox: %v", err)
	}

	h.d.sweepOutbox(context.Background())
	h.drain(t)
	if ch.Count() != 1 {
		t.Fatalf("orphaned event delivered %d times, want 1", ch.Count())
	}
	if pending := h.store.PendingOutbox(); len(pending) != 0 {
		t.Fatalf("outbox still holds %+v", pending)
	}
}

// A FAILED delivery must leave its row behind.
//
// This is the defect that made the first version of the outbox a decoration:
// the row was deleted as soon as processing returned, and processing returns
// just the same when every provider refused the message. The attempt budget and
// every retry guarantee rested on a row that was already gone.
func TestFailedDeliveryKeepsTheOutboxRow(t *testing.T) {
	h := newHarness(t, Options{})
	ch := h.addChannel(t, "c")
	ch.FailWith(errors.New("provider down"))
	h.assign(9, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(9, MonitorSettings{Name: "API"})

	h.d.HandleStateChange(downEvent(9, "API"))
	h.drain(t)
	if pending := h.store.PendingOutbox(); len(pending) != 1 {
		t.Fatalf("a failed delivery left %d outbox rows, want 1 — the event is lost", len(pending))
	}

	// And once the provider recovers, the retry actually delivers it.
	ch.FailWith(nil)
	h.expireOutboxLeases()
	h.d.sweepOutbox(context.Background())
	h.drain(t)
	if ch.Count() != 1 {
		t.Fatalf("deliveries after recovery = %d, want 1", ch.Count())
	}
	if pending := h.store.PendingOutbox(); len(pending) != 0 {
		t.Fatalf("outbox still holds %+v after a successful retry", pending)
	}
}

// A partial fan-out counts as unfinished: one reachable channel out of two is
// not delivery, and the row has to stay so the other one is tried again.
func TestPartialFanOutKeepsTheOutboxRow(t *testing.T) {
	h := newHarness(t, Options{})
	good := h.addChannel(t, "good")
	bad := h.addChannel(t, "bad")
	bad.FailWith(errors.New("provider down"))
	h.assign(11, assignSpec{id: 1, typ: "good"}, assignSpec{id: 2, typ: "bad"})
	h.store.SetMonitorSettings(11, MonitorSettings{Name: "API"})

	h.d.HandleStateChange(downEvent(11, "API"))
	h.drain(t)
	if good.Count() != 1 {
		t.Fatalf("the reachable channel got %d messages, want 1", good.Count())
	}
	if pending := h.store.PendingOutbox(); len(pending) != 1 {
		t.Fatalf("a partial fan-out left %d rows, want 1", len(pending))
	}
}

// A deliberate non-delivery is not a failure. Muting a monitor means the alert
// is not wanted; keeping the row would retry until the attempt budget ran out
// and then log a GIVING UP error about an alert nobody ever asked for.
func TestDeliberateNonDeliveryClearsTheOutboxRow(t *testing.T) {
	t.Run("muted", func(t *testing.T) {
		h := newHarness(t, Options{})
		h.addChannel(t, "c")
		h.assign(12, assignSpec{id: 1, typ: "c"})
		h.store.SetMonitorSettings(12, MonitorSettings{Name: "API"})
		h.d.Mute(12, h.clk.Now().Add(time.Hour))

		h.d.HandleStateChange(downEvent(12, "API"))
		h.drain(t)
		if pending := h.store.PendingOutbox(); len(pending) != 0 {
			t.Fatalf("a muted monitor left %d outbox rows: %+v", len(pending), pending)
		}
	})

	t.Run("no channels assigned", func(t *testing.T) {
		h := newHarness(t, Options{})
		h.store.SetMonitorSettings(12, MonitorSettings{Name: "API"})

		h.d.HandleStateChange(downEvent(12, "API"))
		h.drain(t)
		if pending := h.store.PendingOutbox(); len(pending) != 0 {
			t.Fatalf("an unassigned monitor left %d outbox rows: %+v", len(pending), pending)
		}
	})
}

// A fresh row is claimed by the process that wrote it.
//
// Inserted unleased it would be immediately due, so a sweep landing between the
// INSERT and the delivery would take an event its own process is still holding
// — and send it twice.
func TestFreshRowIsNotSweptFromUnderItsOwner(t *testing.T) {
	h := newHarness(t, Options{QueueSize: 4})
	ch := h.addChannel(t, "c")
	h.assign(13, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(13, MonitorSettings{Name: "API"})

	// Hold the dispatch loop so the event is recorded but not yet delivered —
	// exactly the window a sweep must not reach into.
	blocked, release := make(chan struct{}), make(chan struct{})
	h.d.enqueue(func(context.Context) { close(blocked); <-release })
	<-blocked

	h.d.HandleStateChange(downEvent(13, "API"))
	waitFor(t, func() bool { return len(h.store.PendingOutbox()) == 1 })

	h.d.sweepOutbox(context.Background())
	close(release)
	h.drain(t)

	if ch.Count() != 1 {
		t.Fatalf("deliveries = %d, want exactly 1 — the sweeper duplicated an in-flight event", ch.Count())
	}
}

// A retry runs on the dispatch goroutine, not on the sweeper's.
//
// Everything the dispatch loop touches — the outage map, the digest buffer, the
// escalation timers — is reached without a lock in places because that loop is
// single-threaded. A sweeper that called into it directly would race all of it.
func TestRetryRunsOnTheDispatchLoop(t *testing.T) {
	h := newHarness(t, Options{QueueSize: 8})
	ch := h.addChannel(t, "c")
	h.assign(14, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(14, MonitorSettings{Name: "API"})
	if _, err := h.store.EnqueueOutbox(context.Background(), downRecord(t, 14, "API"), 0); err != nil {
		t.Fatalf("EnqueueOutbox: %v", err)
	}

	// Occupy the dispatch loop. If the sweep delivers anyway, it ran the event
	// on its own goroutine.
	blocked, release := make(chan struct{}), make(chan struct{})
	h.d.enqueue(func(context.Context) { close(blocked); <-release })
	<-blocked

	h.d.sweepOutbox(context.Background())
	if got := ch.Count(); got != 0 {
		t.Fatalf("the sweeper delivered %d messages while the dispatch loop was busy; "+
			"the retry bypassed the loop", got)
	}
	close(release)
	h.drain(t)
	if ch.Count() != 1 {
		t.Fatalf("deliveries after the loop freed up = %d, want 1", ch.Count())
	}
}

// Certificate-expiry and maintenance notices go through the outbox too. They
// travel the same bounded queue as a DOWN and were just as droppable.
func TestNonStateChangeEventsAreRecorded(t *testing.T) {
	t.Run("cert expiry", func(t *testing.T) {
		h := newHarness(t, Options{})
		ch := h.addChannel(t, "c")
		ch.FailWith(errors.New("provider down"))
		h.assign(15, assignSpec{id: 1, typ: "c"})
		h.store.SetMonitorSettings(15, MonitorSettings{Name: "API"})

		h.d.HandleCertExpiry(CertExpiryEvent{MonitorID: 15, MonitorName: "API", DaysRemaining: 3})
		h.drain(t)
		pending := h.store.PendingOutbox()
		if len(pending) != 1 || pending[0].Kind != OutboxCertExpiry {
			t.Fatalf("pending = %+v, want one cert_expiry row", pending)
		}

		ch.FailWith(nil)
		h.expireOutboxLeases()
		h.d.sweepOutbox(context.Background())
		h.drain(t)
		if ch.Count() != 1 {
			t.Fatalf("retried cert expiry delivered %d times, want 1", ch.Count())
		}
	})

	t.Run("maintenance", func(t *testing.T) {
		h := newHarness(t, Options{})
		ch := h.addChannel(t, "c")
		ch.FailWith(errors.New("provider down"))
		h.assign(16, assignSpec{id: 1, typ: "c"})
		h.store.SetMonitorSettings(16, MonitorSettings{Name: "API"})

		h.d.HandleMaintenance(16, "API", true, h.clk.Now())
		h.drain(t)
		pending := h.store.PendingOutbox()
		if len(pending) != 1 || pending[0].Kind != OutboxMaintenance {
			t.Fatalf("pending = %+v, want one maintenance row", pending)
		}

		ch.FailWith(nil)
		h.expireOutboxLeases()
		h.d.sweepOutbox(context.Background())
		h.drain(t)
		if ch.Count() != 1 {
			t.Fatalf("retried maintenance notice delivered %d times, want 1", ch.Count())
		}
	})
}

// A leased row is not retried underneath a delivery that is merely slow.
func TestClaimedRowIsNotRetriedWhileLeased(t *testing.T) {
	h := newHarness(t, Options{})
	ch := h.addChannel(t, "c")
	ch.FailWith(errors.New("provider down"))
	h.assign(4, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(4, MonitorSettings{Name: "API"})

	if _, err := h.store.EnqueueOutbox(context.Background(), downRecord(t, 4, "API"), 0); err != nil {
		t.Fatalf("EnqueueOutbox: %v", err)
	}
	h.d.sweepOutbox(context.Background())
	h.drain(t)
	first := len(h.store.Deliveries())
	if first == 0 {
		t.Fatal("the first sweep delivered nothing, so the rest of this test proves nothing")
	}
	// The row must still be there — a failed delivery keeps it — and the next
	// sweep must leave it alone because its claim has not lapsed.
	if pending := h.store.PendingOutbox(); len(pending) != 1 {
		t.Fatalf("pending after a failed retry = %d, want 1", len(pending))
	}
	h.d.sweepOutbox(context.Background())
	h.drain(t)
	if got := len(h.store.Deliveries()); got != first {
		t.Fatalf("a leased row was retried immediately: %d attempts, want %d", got, first)
	}
}

// An event that keeps failing is eventually given up on — but loudly, and only
// after the attempt budget, never silently.
func TestOutboxGivesUpAfterTheAttemptBudget(t *testing.T) {
	h := newHarness(t, Options{})
	ch := h.addChannel(t, "c")
	ch.FailWith(errors.New("provider down"))
	h.assign(5, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(5, MonitorSettings{Name: "API"})
	if _, err := h.store.EnqueueOutbox(context.Background(), downRecord(t, 5, "API"), 0); err != nil {
		t.Fatalf("EnqueueOutbox: %v", err)
	}

	// Drive the lease clock forward so every sweep finds the row due again.
	now := time.Now()
	h.store.SetOutboxClock(func() time.Time { return now })
	for range outboxMaxAttempts + 1 {
		h.d.sweepOutbox(context.Background())
		h.drain(t)
		now = now.Add(outboxLease + time.Minute)
	}
	if pending := h.store.PendingOutbox(); len(pending) != 0 {
		t.Fatalf("outbox still holds %+v after exceeding the attempt budget", pending)
	}
}

// TestRestoreDoesNotInventRecoveryRecipients
//
// The in-memory record of who was alerted dies with the process, and Restore
// used to rebuild it by arithmetic: every channel whose escalation delay had
// come due was assumed to have been told. Due is not the same as reached.
//
// Two channels are due here and only one was reached — the other's provider was
// failing throughout, which is precisely when an operator is watching. Marking
// it notified sends it "back online" for an outage it never heard of, which is
// the same defect the per-channel tracking elsewhere exists to prevent, and it
// came back in through the restart path.
func TestRestoreDoesNotInventRecoveryRecipients(t *testing.T) {
	h := newHarness(t, Options{})
	reached := h.addChannel(t, "reached")
	silent := h.addChannel(t, "silent")
	h.assign(1,
		assignSpec{id: 1, typ: "reached"},
		assignSpec{id: 2, typ: "silent"},
	)
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API"})
	h.store.SetOpenOutages(Outage{MonitorID: 1, Since: h.clk.Now().Add(-time.Hour)})
	// Both were due at level 0; only one actually received the DOWN.
	h.wasAlerted(1, 1)

	if err := h.d.Restore(context.Background()); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	h.drain(t)

	h.d.HandleStateChange(upEvent(1, "API"))
	h.drain(t)

	if reached.Count() != 1 {
		t.Fatalf("the channel that WAS told got %d recoveries, want 1", reached.Count())
	}
	if silent.Count() != 0 {
		t.Fatalf("the channel that was never told got %d recoveries, want 0 — "+
			"it would be told an outage it never heard of had ended", silent.Count())
	}
}

// A channel added to the monitor DURING an outage has heard nothing, so it gets
// no recovery either. Under the old arithmetic its escalation delay of 0 was
// immediately "due" and it was marked notified on the spot.
func TestRestoreSkipsAChannelAddedDuringTheOutage(t *testing.T) {
	h := newHarness(t, Options{})
	original := h.addChannel(t, "original")
	added := h.addChannel(t, "added")
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API"})
	h.store.SetOpenOutages(Outage{MonitorID: 1, Since: h.clk.Now().Add(-time.Hour)})
	h.wasAlerted(1, 1) // only the original existed when the outage began

	// By the time we restart, the operator has attached a second channel.
	h.assign(1,
		assignSpec{id: 1, typ: "original"},
		assignSpec{id: 2, typ: "added"},
	)

	if err := h.d.Restore(context.Background()); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	h.drain(t)
	h.d.HandleStateChange(upEvent(1, "API"))
	h.drain(t)

	if original.Count() != 1 {
		t.Fatalf("original recoveries = %d, want 1", original.Count())
	}
	if added.Count() != 0 {
		t.Fatalf("a channel attached during the outage got %d recoveries, want 0",
			added.Count())
	}
}

// A channel that was told DOWN and then told UP is settled: a later restart
// must not resurrect it as awaiting a recovery.
func TestRestoreIgnoresAChannelAlreadyResolved(t *testing.T) {
	h := newHarness(t, Options{})
	ch := h.addChannel(t, "c")
	h.assign(1, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API"})
	h.wasAlerted(1, 1)
	_ = h.store.RecordDelivery(context.Background(), Delivery{
		NotificationID: 1, MonitorID: 1,
		EventKind: domain.EventKindUp, Status: DeliverySuccess,
	})
	// A NEW outage, with no DOWN delivered for it yet.
	h.store.SetOpenOutages(Outage{MonitorID: 1, Since: h.clk.Now().Add(-time.Minute)})

	if err := h.d.Restore(context.Background()); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	h.drain(t)
	h.d.HandleStateChange(upEvent(1, "API"))
	h.drain(t)

	if ch.Count() != 0 {
		t.Fatalf("deliveries = %d, want 0: the channel's last word on this "+
			"monitor was already UP", ch.Count())
	}
}

// TestDigestFailureDoesNotEarnARecovery
//
// In digest mode the DOWN is buffered in memory and sent later as one summary.
// Buffering used to count as reaching the channel, so a flush that failed left
// the outage unannounced while the channel was recorded as having heard about
// it — and the eventual "back online" then went to someone who was never told
// there was a problem. That is worse than silence: it actively misinforms.
func TestDigestFailureDoesNotEarnARecovery(t *testing.T) {
	h := newHarness(t, Options{Digest: DigestOptions{Interval: time.Minute}})
	ch := h.addChannel(t, "c")
	h.assign(1, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API"})

	h.d.HandleStateChange(downEvent(1, "API"))
	h.drain(t)
	if ch.Count() != 0 {
		t.Fatalf("digest mode delivered %d messages immediately, want 0", ch.Count())
	}

	// The flush fails: the batch is gone and nothing was sent.
	ch.FailWith(errors.New("provider down"))
	h.clk.Advance(time.Minute)
	h.drain(t)
	ch.FailWith(nil)

	h.d.HandleStateChange(upEvent(1, "API"))
	h.drain(t)
	h.clk.Advance(time.Minute) // let any recovery digest go out
	h.drain(t)

	// In digest mode a recovery arrives INSIDE a batch, not as its own message,
	// so look at the items — checking only sent.Kind would make this test pass
	// against the very bug it is here for.
	for _, sent := range ch.Sent() {
		if sent.Kind == domain.EventKindUp {
			t.Fatal("a channel whose digest never went out was sent a recovery")
		}
		for _, item := range sent.Items {
			if item.Kind == domain.EventKindUp {
				t.Fatal("a channel whose digest never went out was sent a recovery " +
					"inside a later batch")
			}
		}
	}
}

// The other half: a digest that DOES go out earns the recovery, and the audit
// log records it per monitor so a restart can still find it. The digest's own
// row carries no monitor, so without the per-monitor rows a digest-mode
// deployment would lose every recovery across a restart.
func TestASentDigestEarnsTheRecoveryAndIsRecorded(t *testing.T) {
	h := newHarness(t, Options{Digest: DigestOptions{Interval: time.Minute}})
	ch := h.addChannel(t, "c")
	h.assign(1, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API"})

	h.d.HandleStateChange(downEvent(1, "API"))
	h.drain(t)
	h.clk.Advance(time.Minute)
	h.drain(t)

	var digests int
	for _, sent := range ch.Sent() {
		if sent.Kind == domain.EventKindDigest {
			digests++
		}
	}
	if digests != 1 {
		t.Fatalf("digest sends = %d, want 1", digests)
	}

	awaiting, err := h.store.ChannelsAwaitingRecovery(context.Background(), 1)
	if err != nil {
		t.Fatalf("ChannelsAwaitingRecovery: %v", err)
	}
	if len(awaiting) != 1 || awaiting[0] != 1 {
		t.Fatalf("awaiting = %v, want [1] — a restart could not tell that this "+
			"channel had been told", awaiting)
	}

	h.d.HandleStateChange(upEvent(1, "API"))
	h.drain(t)
	h.clk.Advance(time.Minute)
	h.drain(t)
	var sawUp bool
	for _, sent := range ch.Sent() {
		if sent.Kind == domain.EventKindDigest {
			for _, item := range sent.Items {
				if item.Kind == domain.EventKindUp {
					sawUp = true
				}
			}
		}
	}
	if !sawUp {
		t.Fatal("the channel that received the DOWN digest never got the recovery")
	}
}

// TestRetryOnlyChasesTheChannelsThatMissedIt
//
// Retrying a DOWN from the outbox re-ran processDown, which built a fresh
// outage state each time. So every attempt re-delivered to the channels that
// had already succeeded: one permanently broken provider turned into up to ten
// duplicate DOWN messages on every working channel — alert spam produced by the
// mechanism that exists to prevent missing alerts.
func TestRetryOnlyChasesTheChannelsThatMissedIt(t *testing.T) {
	h := newHarness(t, Options{})
	good := h.addChannel(t, "good")
	bad := h.addChannel(t, "bad")
	bad.FailWith(errors.New("provider down"))
	h.assign(1, assignSpec{id: 1, typ: "good"}, assignSpec{id: 2, typ: "bad"})
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API"})

	h.d.HandleStateChange(downEvent(1, "API"))
	h.drain(t)
	if good.Count() != 1 {
		t.Fatalf("good channel got %d messages on the first attempt, want 1", good.Count())
	}

	// Three sweeps with the bad channel still broken.
	for range 3 {
		h.expireOutboxLeases()
		h.d.sweepOutbox(context.Background())
		h.drain(t)
	}
	if good.Count() != 1 {
		t.Fatalf("the working channel received %d DOWN messages, want 1: the "+
			"retries for a different channel were sent to it as well", good.Count())
	}

	// And when the broken one recovers, it gets the alert it missed — once.
	bad.FailWith(nil)
	h.expireOutboxLeases()
	h.d.sweepOutbox(context.Background())
	h.drain(t)
	if bad.Count() != 1 {
		t.Fatalf("the recovered channel got %d messages, want exactly 1", bad.Count())
	}
	if good.Count() != 1 {
		t.Fatalf("good channel = %d, want still 1", good.Count())
	}
	// Everyone has now heard, so the event is finished and the row is gone.
	if pending := h.store.PendingOutbox(); len(pending) != 0 {
		t.Fatalf("outbox still holds %+v once every channel has been reached", pending)
	}
}

// A retry must not move the outage start. The recovery reports the duration
// from it, and taking the clock at retry time would shorten a half-hour outage
// to whatever happened after the last failed attempt.
func TestRetryDoesNotShortenTheReportedOutage(t *testing.T) {
	h := newHarness(t, Options{})
	ch := h.addChannel(t, "c")
	ch.FailWith(errors.New("provider down"))
	h.assign(2, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(2, MonitorSettings{Name: "API"})

	// The event is delivered LATE, by the sweeper, with no prior state in this
	// process — what happens when a full queue drops it, or when it outlives a
	// restart. This is the path where the outage start has to come from the
	// event rather than from the clock.
	began := h.clk.Now()
	ev := downEvent(2, "API")
	ev.At = began
	payload, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := h.store.EnqueueOutbox(context.Background(), OutboxRecord{
		Kind: OutboxStateChange, MonitorID: 2, Payload: payload,
	}, 0); err != nil {
		t.Fatalf("EnqueueOutbox: %v", err)
	}

	h.clk.Advance(time.Hour)
	ch.FailWith(nil)
	h.d.sweepOutbox(context.Background())
	h.drain(t)

	h.d.HandleStateChange(upEvent(2, "API"))
	h.drain(t)

	var recovery *domain.NotificationEvent
	for i, sent := range ch.Sent() {
		if sent.Kind == domain.EventKindUp {
			recovery = &ch.Sent()[i]
		}
	}
	if recovery == nil {
		t.Fatal("no recovery was delivered")
	}
	if recovery.DownSince == nil {
		t.Fatal("the recovery carries no outage start")
	}
	if !recovery.DownSince.Equal(began) {
		t.Fatalf("reported outage start = %s, want %s — the retry moved it forward",
			recovery.DownSince, began)
	}
}

// TestStopTerminatesEvenWhenItGivesUp
//
// Shutdown runs in two steps — drain the recorder, then drain the dispatch
// loop. Returning from the first step on a deadline without telling the loop to
// stop left it running for the life of the process and left `done` unclosed,
// which then hung the next caller. A shutdown path that can hang is worse than
// one that is merely slow.
func TestStopTerminatesEvenWhenItGivesUp(t *testing.T) {
	h := newHarness(t, Options{})

	// A context that is already over: Stop cannot complete either step.
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.d.Stop(dead); err == nil {
		t.Fatal("Stop with a dead context returned nil, want the context error")
	}

	// The loop must still be on its way out, and a second caller must not be
	// left waiting on it.
	second := make(chan error, 1)
	go func() {
		ctx, c2 := context.WithTimeout(context.Background(), testutil.Wait(2*time.Second))
		defer c2()
		second <- h.d.Stop(ctx)
	}()
	select {
	case <-second:
	case <-time.After(testutil.Wait(3 * time.Second)):
		t.Fatal("a second Stop never returned: the first left the loop running")
	}

	select {
	case <-h.d.done:
	case <-time.After(testutil.Wait(2 * time.Second)):
		t.Fatal("the dispatch loop is still running after Stop gave up")
	}
}

// Stopping twice on the happy path is a no-op, not a panic on a closed channel.
func TestStopIsIdempotent(t *testing.T) {
	h := newHarness(t, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), testutil.Wait(2*time.Second))
	defer cancel()
	if err := h.d.Stop(ctx); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := h.d.Stop(ctx); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

// TestADigestedRecoveryIsRecordedToo
//
// Restore reads the delivery log to decide who is still waiting for a recovery.
// The digest path recorded only its DOWN items, so a recovery that went out
// inside a batch left the channel's last recorded word as "down" — and a
// restart during the NEXT outage would treat it as already told and resolve to
// it, which is exactly the defect ChannelsAwaitingRecovery exists to prevent,
// arriving by a different route.
func TestADigestedRecoveryIsRecordedToo(t *testing.T) {
	h := newHarness(t, Options{Digest: DigestOptions{Interval: time.Minute}})
	h.addChannel(t, "c")
	h.assign(1, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API"})

	h.d.HandleStateChange(downEvent(1, "API"))
	h.drain(t)
	h.clk.Advance(time.Minute)
	h.drain(t)

	awaiting, err := h.store.ChannelsAwaitingRecovery(context.Background(), 1)
	if err != nil {
		t.Fatalf("ChannelsAwaitingRecovery: %v", err)
	}
	if len(awaiting) != 1 {
		t.Fatalf("after the DOWN digest, awaiting = %v, want [1]", awaiting)
	}

	h.d.HandleStateChange(upEvent(1, "API"))
	h.drain(t)
	h.clk.Advance(time.Minute)
	h.drain(t)

	awaiting, err = h.store.ChannelsAwaitingRecovery(context.Background(), 1)
	if err != nil {
		t.Fatalf("ChannelsAwaitingRecovery: %v", err)
	}
	if len(awaiting) != 0 {
		t.Fatalf("after the recovery digest, awaiting = %v, want none — a restart "+
			"during the next outage would resolve to a channel that had not "+
			"been told about it", awaiting)
	}
}

// panicNotifier stands in for any of the nineteen providers hitting a bug on a
// malformed response from the endpoint it POSTs to.
type panicNotifier struct{}

func (panicNotifier) Type() string                   { return "panicky" }
func (panicNotifier) Validate(json.RawMessage) error { return nil }
func (panicNotifier) Send(context.Context, json.RawMessage, domain.NotificationEvent) error {
	panic("malformed response from the notification provider")
}

// TestAPanickingProviderIsContained
//
// fanOut runs sends in an errgroup, which does not recover. A panicking
// provider therefore ended the process — after delivering to the channels that
// came before it, so the visible result was a half-delivered alert and a
// monitoring system that had simply vanished.
//
// Before the fix this test did not fail, it CRASHED the test binary.
func TestAPanickingProviderIsContained(t *testing.T) {
	h := newHarness(t, Options{})
	h.reg.Register(panicNotifier{})
	good := h.addChannel(t, "good")
	h.assign(1, assignSpec{id: 1, typ: "panicky"}, assignSpec{id: 2, typ: "good"})
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API"})

	h.d.HandleStateChange(downEvent(1, "API"))
	h.drain(t)

	// The healthy channel is unaffected by its neighbour.
	if good.Count() != 1 {
		t.Fatalf("the working channel got %d messages, want 1", good.Count())
	}

	// The panic is recorded as a FAILED delivery, not swallowed. That matters
	// beyond bookkeeping: a swallowed panic would mark the channel notified and
	// later resolve a recovery to somebody who never heard about the outage.
	var failed bool
	for _, d := range h.store.Deliveries() {
		if d.NotificationID == 1 {
			if d.Status != DeliveryFailed {
				t.Fatalf("the panicking provider was recorded as %q, want failed", d.Status)
			}
			if !strings.Contains(d.Error, "panicked") {
				t.Fatalf("recorded error = %q, want it to name the panic", d.Error)
			}
			failed = true
		}
	}
	if !failed {
		t.Fatal("the panicking provider produced no delivery record at all")
	}

	// Unfinished, so the outbox keeps it and retries — the same treatment any
	// other provider failure gets.
	if pending := h.store.PendingOutbox(); len(pending) != 1 {
		t.Fatalf("outbox holds %d rows, want 1: a panicking provider must not "+
			"count as delivered", len(pending))
	}
}

// TestTheDispatchLoopSurvivesAPanickingTask
//
// The loop is single-threaded and everything the dispatcher does runs on it. A
// panic in one task used to end the process; containing the LOOP instead of the
// task would have been barely better — notifications silently off for the life
// of the process, which is harder to notice than a crash.
func TestTheDispatchLoopSurvivesAPanickingTask(t *testing.T) {
	h := newHarness(t, Options{})
	ch := h.addChannel(t, "c")
	h.assign(1, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API"})

	h.d.enqueue(func(context.Context) { panic("a task went wrong") })

	// The next event must still be delivered.
	h.d.HandleStateChange(downEvent(1, "API"))
	h.drain(t)
	if ch.Count() != 1 {
		t.Fatalf("deliveries after a panicking task = %d, want 1: the loop died", ch.Count())
	}
}

// TestStopFlushesTheDigestBuffer pins that a GRACEFUL shutdown delivers what the
// digest buffer still holds.
//
// The buffer lives only in memory and gives up its outbox row the moment an
// event is buffered — deliver returns outcomeDone there deliberately, because
// keeping the row would make a retry re-add an event the buffer still has and
// list the outage twice. The documented price is that a CRASH loses the batch.
//
// A graceful Stop used to pay that price too, and worse: stopDispatchLoop stops
// the digest timer, so nothing flushed, and the batch vanished without even the
// per-monitor "a DOWN alert was lost" line that a failed send emits. Shutdown
// has a deadline in seconds and a working dispatch loop — there is time to send.
func TestStopFlushesTheDigestBuffer(t *testing.T) {
	h := newHarness(t, Options{Digest: DigestOptions{Interval: time.Hour}})
	ch := h.addChannel(t, "c")
	h.assign(1, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API"})

	h.d.HandleStateChange(downEvent(1, "API"))
	h.drain(t)
	if ch.Count() != 0 {
		t.Fatalf("digest mode delivered %d immediately, want 0", ch.Count())
	}
	if got := h.d.pendingDigestItems(1); got != 1 {
		t.Fatalf("pending digest items = %d, want 1", got)
	}

	// Well before the hour is up, so only the shutdown flush can deliver this.
	ctx, cancel := context.WithTimeout(context.Background(), testutil.Wait(5*time.Second))
	defer cancel()
	if err := h.d.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if ch.Count() != 1 {
		t.Fatalf("deliveries after Stop = %d, want 1 — the buffered DOWN was dropped", ch.Count())
	}
	sent := ch.Sent()[0]
	if sent.Kind != domain.EventKindDigest || len(sent.Items) != 1 {
		t.Fatalf("delivered %s with %d items, want one digest carrying one item", sent.Kind, len(sent.Items))
	}
	if sent.Items[0].Kind != domain.EventKindDown || sent.Items[0].MonitorID != 1 {
		t.Fatalf("digest carried %s for monitor %d, want the DOWN for 1",
			sent.Items[0].Kind, sent.Items[0].MonitorID)
	}
}

// TestStopFlushesTheDigestBufferDuringQuietHours pins the `final` flag: the
// shutdown flush must not defer.
//
// The ordinary flush re-arms its timer and keeps the batch when quiet hours are
// active. Doing that on the way down re-arms a timer on a dispatcher that is
// about to stop, and the batch goes with it — so a do-not-disturb window would
// silently swallow every outage that happened to be buffered at shutdown.
// Sending a few hours inside the window is the lesser harm.
func TestStopFlushesTheDigestBufferDuringQuietHours(t *testing.T) {
	h := newHarness(t, Options{
		Digest: DigestOptions{Interval: time.Hour},
		// A window covering the whole day, so it is active whatever the harness
		// clock says.
		QuietHours: &QuietHours{Start: "00:00", End: "23:59", Location: time.UTC},
	})
	ch := h.addChannel(t, "c")
	h.assign(1, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API"})

	h.d.HandleStateChange(downEvent(1, "API"))
	h.drain(t)
	if got := h.d.pendingDigestItems(1); got != 1 {
		t.Fatalf("pending digest items = %d, want 1", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), testutil.Wait(5*time.Second))
	defer cancel()
	if err := h.d.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if ch.Count() != 1 {
		t.Fatalf("deliveries after Stop = %d, want 1 — quiet hours deferred the shutdown flush", ch.Count())
	}
}

// ---------------------------------------------------------------------------
// Forget (a monitor leaves the scheduler)
// ---------------------------------------------------------------------------

// Deleting a monitor stops its checks, which means the recovery event that
// would have closed an open outage can never arrive. Without Forget the
// downState outlives the monitor, and its resend timer keeps announcing an
// outage on something that is in the bin.
func TestForgetStopsReminders(t *testing.T) {
	h := newHarness(t, Options{})
	ch := h.addChannel(t, "c")
	h.assign(1, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API", ResendInterval: 3 * time.Minute})

	h.d.HandleStateChange(downEvent(1, "API"))
	h.drain(t)
	if ch.Count() != 1 {
		t.Fatalf("initial deliveries = %d, want 1", ch.Count())
	}

	h.d.Forget(1)
	h.clk.Advance(3 * time.Minute)
	h.drain(t)
	if ch.Count() != 1 {
		t.Fatalf("deliveries = %d after the resend interval, want 1: a binned monitor must not keep reminding", ch.Count())
	}
}

// The tombstone is the half that clearing the maps could not provide.
//
// processDown re-creates d.down[id] and re-arms its resend timer with no
// liveness check, so an event still in the dispatch queue — or replayed from
// the durable outbox minutes later — rebuilt everything Forget had removed, and
// nothing could ever clear it again.
func TestAnEventInFlightCannotResurrectAForgottenMonitor(t *testing.T) {
	h := newHarness(t, Options{})
	ch := h.addChannel(t, "c")
	h.assign(2, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(2, MonitorSettings{Name: "API", ResendInterval: 3 * time.Minute})

	h.d.Forget(2)
	// The event arrives AFTER the delete, which is exactly the outbox-replay
	// shape and the queued-task shape.
	h.d.HandleStateChange(downEvent(2, "API"))
	h.drain(t)

	if ch.Count() != 0 {
		t.Fatalf("deliveries = %d for a deleted monitor, want 0", ch.Count())
	}
	// And no timer was armed, so it cannot start reminding either.
	h.clk.Advance(3 * time.Minute)
	h.drain(t)
	if ch.Count() != 0 {
		t.Fatalf("deliveries = %d after the resend interval, want 0", ch.Count())
	}
	// The outbox row is completed rather than retried forever.
	if pending := h.store.PendingOutbox(); len(pending) != 0 {
		t.Errorf("outbox still holds %+v for a deleted monitor; it would be replayed every sweep", pending)
	}
}

// Reset is what a restore and a resume call: same clearing, no tombstone.
func TestResetLetsAMonitorAlertAgain(t *testing.T) {
	h := newHarness(t, Options{})
	ch := h.addChannel(t, "c")
	h.assign(3, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(3, MonitorSettings{Name: "API"})

	h.d.HandleStateChange(downEvent(3, "API"))
	h.drain(t)
	if ch.Count() != 1 {
		t.Fatalf("initial deliveries = %d, want 1", ch.Count())
	}

	h.d.Forget(3) // binned
	h.d.Reset(3)  // restored

	h.d.HandleStateChange(downEvent(3, "API"))
	h.drain(t)
	if ch.Count() != 2 {
		t.Fatalf("deliveries = %d after the monitor came back and failed again, want 2: "+
			"the alert was swallowed by stale state", ch.Count())
	}
}

// A mute is a snooze on one monitor. Surviving a delete and a restore, it would
// silence a monitor for a reason nobody can reconstruct.
func TestResetLiftsTheMute(t *testing.T) {
	h := newHarness(t, Options{})
	ch := h.addChannel(t, "c")
	h.assign(1, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API"})

	h.d.Mute(1, h.clk.Now().Add(24*time.Hour))
	h.d.HandleStateChange(downEvent(1, "API"))
	h.drain(t)
	if ch.Count() != 0 {
		t.Fatalf("a muted monitor delivered %d alerts, want 0", ch.Count())
	}

	h.d.Reset(1)
	h.d.HandleStateChange(downEvent(1, "API"))
	h.drain(t)
	if ch.Count() != 1 {
		t.Fatalf("deliveries = %d after Reset, want 1: the mute outlived the monitor", ch.Count())
	}
}

// Forget and Reset are called on every delete and every resume, and almost
// none of them are mid-outage.
func TestForgetAndResetOnAMonitorWithNoState(t *testing.T) {
	h := newHarness(t, Options{})
	h.d.Forget(42)
	h.d.Forget(42)
	h.d.Reset(43)
	h.d.Reset(43)
}

// A failed "back online" alert has to survive its retry.
//
// processRecovery deleted d.down[id] BEFORE delivering and still returned
// outcomeRetry on a fan-out failure. The retry then found no tracked outage,
// logged "recovery without a tracked outage: no resolve notification sent",
// returned outcomeDone — and completeOutbox deleted the row. Any transient
// provider error therefore lost the resolve permanently, and the machinery
// built to retry it was what threw it away. No test covered a failed UP
// delivery; the two outbox tests above both use DOWN.
func TestAFailedRecoveryAlertIsActuallyRetried(t *testing.T) {
	h := newHarness(t, Options{})
	ch := h.addChannel(t, "c")
	h.assign(4, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(4, MonitorSettings{Name: "API"})

	h.d.HandleStateChange(downEvent(4, "API"))
	h.drain(t)
	if ch.Count() != 1 {
		t.Fatalf("DOWN deliveries = %d, want 1", ch.Count())
	}

	// The provider fails exactly while the recovery is being announced.
	ch.FailWith(errors.New("provider down"))
	h.d.HandleStateChange(upEvent(4, "API"))
	h.drain(t)
	if pending := h.store.PendingOutbox(); len(pending) != 1 {
		t.Fatalf("a failed recovery left %d outbox rows, want 1 — the resolve is lost", len(pending))
	}

	// The provider comes back and the sweep retries. This is the step that used
	// to delete the row without sending anything.
	ch.FailWith(nil)
	before := ch.Count()
	h.expireOutboxLeases()
	h.d.sweepOutbox(context.Background())
	h.drain(t)
	if ch.Count() != before+1 {
		t.Fatalf("deliveries after the retry = %d, want %d: the resolve was never sent",
			ch.Count(), before+1)
	}
	last := ch.Sent()[ch.Count()-1]
	if last.Status != domain.StatusUp {
		t.Errorf("the retried message is %q, want the recovery", last.Status)
	}
	if pending := h.store.PendingOutbox(); len(pending) != 0 {
		t.Errorf("outbox still holds %+v after a successful resolve", pending)
	}
}

// A half-resolved outage must not silence the next one.
//
// Keeping the downState alive so the resolve can be retried creates a second
// hazard: if the monitor goes down again before the resolve ever succeeds, the
// old `notified` set is still full, and processDown's resumed path would target
// nobody. The new outage has to start clean.
func TestANewOutageAfterAnUndeliveredResolveStillAlerts(t *testing.T) {
	h := newHarness(t, Options{})
	ch := h.addChannel(t, "c")
	h.assign(5, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(5, MonitorSettings{Name: "API"})

	h.d.HandleStateChange(downEvent(5, "API"))
	h.drain(t)

	ch.FailWith(errors.New("provider down"))
	h.d.HandleStateChange(upEvent(5, "API")) // resolve never gets out
	h.drain(t)
	ch.FailWith(nil)

	before := ch.Count()
	h.d.HandleStateChange(downEvent(5, "API"))
	h.drain(t)
	if ch.Count() != before+1 {
		t.Fatalf("deliveries for the SECOND outage = %d, want %d: it inherited the "+
			"previous outage's notified set and was swallowed", ch.Count()-before, 1)
	}
	if last := ch.Sent()[ch.Count()-1]; last.Status != domain.StatusDown {
		t.Errorf("the second outage delivered %q, want down", last.Status)
	}
}
