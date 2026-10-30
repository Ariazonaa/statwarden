package notify

import (
	"strings"
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
)

// flappingEvent is what the engine emits as a flapping summary: a state→state
// event carrying the payload, never a transition.
func flappingEvent(id int64, name string, ended bool, status domain.MonitorStatus) domain.StateChange {
	return domain.StateChange{
		MonitorID:   id,
		MonitorName: name,
		From:        status,
		To:          status,
		Flapping: &domain.Flapping{
			Ended:   ended,
			Changes: 5,
			Window:  30 * time.Minute,
			Status:  status,
		},
	}
}

// The onset summary is delivered as its own kind and says both how bad it is
// and that alerts are now paused — silence afterwards must not read as "fixed".
func TestFlappingOnsetIsDelivered(t *testing.T) {
	h := newHarness(t, Options{})
	ch := h.addChannel(t, "c")
	h.assign(1, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API"})

	h.d.HandleStateChange(flappingEvent(1, "API", false, domain.StatusDown))
	h.drain(t)

	if ch.Count() != 1 {
		t.Fatalf("flapping deliveries = %d, want 1 — the summary was dropped", ch.Count())
	}
	got := ch.Sent()[0]
	if got.Kind != domain.EventKindFlapping {
		t.Fatalf("kind = %q, want %q", got.Kind, domain.EventKindFlapping)
	}
	for _, want := range []string{"5 times", "30m", "paused"} {
		if !strings.Contains(got.Body, want) {
			t.Errorf("body %q does not mention %q", got.Body, want)
		}
	}
	if strings.Contains(got.Body, "30m0s") {
		t.Errorf("body prints the raw duration: %q", got.Body)
	}
	// A summary is not an outage: nothing may be tracked for a later resolve.
	h.d.mu.Lock()
	tracked := h.d.down[1] != nil
	h.d.mu.Unlock()
	if tracked {
		t.Fatal("a flapping summary registered an outage")
	}
}

// The settled summary is its own kind, so a channel can template it separately.
func TestFlappingSettledIsDeliveredAsItsOwnKind(t *testing.T) {
	h := newHarness(t, Options{})
	ch := h.addChannel(t, "c")
	h.assign(1, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API"})

	h.d.HandleStateChange(flappingEvent(1, "API", true, domain.StatusUp))
	h.drain(t)

	if ch.Count() != 1 {
		t.Fatalf("settled deliveries = %d, want 1", ch.Count())
	}
	if got := ch.Sent()[0]; got.Kind != domain.EventKindFlappingStable || got.Status != domain.StatusUp {
		t.Fatalf("delivery = %s/%s, want %s/up", got.Kind, got.Status, domain.EventKindFlappingStable)
	}
}

// A maintenance window silences the summary like any other alert.
func TestSuppressedFlappingIsNotDelivered(t *testing.T) {
	h := newHarness(t, Options{})
	ch := h.addChannel(t, "c")
	h.assign(1, assignSpec{id: 1, typ: "c"})

	ev := flappingEvent(1, "API", false, domain.StatusDown)
	ev.Suppressed, ev.SuppressReason = true, "maintenance"
	h.d.HandleStateChange(ev)
	h.drain(t)
	if ch.Count() != 0 {
		t.Fatalf("deliveries = %d, want 0 during maintenance", ch.Count())
	}
}

// The heart of the dispatcher side: an UP held back because the monitor is
// flapping must NOT forget an outage that was already announced. Otherwise the
// recovery the engine sends once the flapping settles finds nobody who was told
// DOWN, and the channels that heard the outage never hear it end.
func TestAHeldBackUpKeepsTheAnnouncedOutageOpen(t *testing.T) {
	h := newHarness(t, Options{})
	ch := h.addChannel(t, "c")
	h.assign(1, assignSpec{id: 1, typ: "c"})
	h.store.SetMonitorSettings(1, MonitorSettings{Name: "API"})

	h.d.HandleStateChange(downEvent(1, "API"))
	h.drain(t)

	held := upEvent(1, "API")
	held.Suppressed, held.SuppressReason = true, domain.SuppressFlapping
	h.d.HandleStateChange(held)
	h.drain(t)

	h.d.HandleStateChange(upEvent(1, "API")) // the owed recovery
	h.drain(t)

	sent := ch.Sent()
	if len(sent) != 2 || sent[0].Kind != domain.EventKindDown || sent[1].Kind != domain.EventKindUp {
		kinds := make([]domain.NotificationEventKind, 0, len(sent))
		for _, s := range sent {
			kinds = append(kinds, s.Kind)
		}
		t.Fatalf("deliveries = %v, want [down up] — the held-back UP lost the outage", kinds)
	}
}

// Other suppression reasons keep their old meaning: a maintenance-suppressed UP
// still closes the outage without a message.
func TestAMaintenanceSuppressedUpStillClosesTheOutage(t *testing.T) {
	h := newHarness(t, Options{})
	h.addChannel(t, "c")
	h.assign(1, assignSpec{id: 1, typ: "c"})

	h.d.HandleStateChange(downEvent(1, "API"))
	h.drain(t)
	ev := upEvent(1, "API")
	ev.Suppressed, ev.SuppressReason = true, "maintenance"
	h.d.HandleStateChange(ev)
	h.drain(t)

	h.d.mu.Lock()
	tracked := h.d.down[1] != nil
	h.d.mu.Unlock()
	if tracked {
		t.Fatal("a maintenance-suppressed UP left the outage open")
	}
}
