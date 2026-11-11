package notify

import (
	"context"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/domain"
)

// One hanging channel must not hold every other monitor's alerts.
//
// The dispatch loop ran each event to completion, fan-out included, before
// taking the next, so a provider that accepted the connection and then said
// nothing held the whole queue for its full send budget — SendAttempts ×
// SendTimeout plus retry delays, per event. During a fleet-wide incident that
// is every alert, on every channel, arriving minutes late.
func TestAHangingChannelDoesNotDelayAnotherMonitorsAlert(t *testing.T) {
	h := newHarness(t, Options{})
	slow := h.addChannel(t, "slow")
	fast := h.addChannel(t, "fast")
	release := make(chan struct{})
	slow.BlockOn(release)
	defer close(release)
	h.assign(1, assignSpec{id: 1, typ: "slow"})
	h.assign(2, assignSpec{id: 2, typ: "fast"})
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "A"})
	h.store.SetMonitorSettings(2, MonitorSettings{Name: "B"})

	h.d.HandleStateChange(downEvent(1, "A"))
	waitFor(t, func() bool { return slow.Calls() == 1 })

	h.d.HandleStateChange(downEvent(2, "B"))
	waitFor(t, func() bool { return fast.Count() == 1 })
}

// Events for ONE monitor still run in order. Running a recovery while the DOWN
// it resolves is still being delivered would find nobody marked as told, close
// the outage silently, and leave the channel that then received the DOWN
// without its "back online".
func TestEventsForOneMonitorStayInOrder(t *testing.T) {
	h := newHarness(t, Options{})
	ch := h.addChannel(t, "c")
	release := make(chan struct{})
	ch.BlockOn(release)
	h.assign(3, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(3, MonitorSettings{Name: "API"})

	h.d.HandleStateChange(downEvent(3, "API"))
	waitFor(t, func() bool { return ch.Calls() == 1 })
	h.d.HandleStateChange(upEvent(3, "API"))
	h.d.inflight.Wait()
	if got := ch.Calls(); got != 1 {
		t.Fatalf("the recovery started while the DOWN was still in flight (%d sends)", got)
	}
	close(release)
	h.drain(t)

	sent := ch.Sent()
	if len(sent) != 2 || sent[0].Kind != domain.EventKindDown || sent[1].Kind != domain.EventKindUp {
		t.Fatalf("sent = %+v, want DOWN then UP", sent)
	}
}

// An event this process is still holding must not be replayed underneath
// itself just because its lease ran out while it waited.
//
// The lease is taken when the row is written, and the wait in front of the
// delivery is unbounded: behind a hanging channel it was minutes per event. A
// sweep then found the row "abandoned" and queued it a second time, so the
// alert went out twice once the channel recovered.
func TestALapsedLeaseDoesNotReplayAnEventStillHeldHere(t *testing.T) {
	h := newHarness(t, Options{})
	ch := h.addChannel(t, "c")
	release := make(chan struct{})
	ch.BlockOn(release)
	h.assign(4, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(4, MonitorSettings{Name: "API"})

	// The first event hangs in the provider; the second waits behind it.
	h.d.HandleStateChange(downEvent(4, "API"))
	waitFor(t, func() bool { return ch.Calls() == 1 })
	h.d.HandleCertExpiry(CertExpiryEvent{MonitorID: 4, MonitorName: "API", DaysRemaining: 3})
	waitFor(t, func() bool { return len(h.store.PendingOutbox()) == 2 })

	h.expireOutboxLeases()
	h.d.sweepOutbox(context.Background())
	close(release)
	h.drain(t)

	certs := 0
	for _, ev := range ch.Sent() {
		if ev.Kind == domain.EventKindCertExpiry {
			certs++
		}
	}
	if certs != 1 {
		t.Fatalf("certificate warning delivered %d times, want 1", certs)
	}
	if pending := h.store.PendingOutbox(); len(pending) != 0 {
		t.Fatalf("outbox still holds %+v", pending)
	}
}
