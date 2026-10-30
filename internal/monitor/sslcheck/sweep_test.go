package sslcheck

import (
	"context"
	"testing"
	"time"
)

// TestSweeperResetsOnCertReplacement guards the renewal edge case: when a
// monitor's certificate is swapped for a *different* one that is itself
// already inside the warning window, the de-duplication keyed on the previous
// (tighter) threshold must not swallow the new certificate's warnings. The
// sweep identifies certificates by ValidTo, so a changed expiry resets state.
func TestSweeperResetsOnCertReplacement(t *testing.T) {
	src := &memSource{}
	var events []Event
	now := testNow
	sw := NewSweeper(SweeperOptions{
		Source:  src,
		OnEvent: func(ev Event) { events = append(events, ev) },
		Now:     func() time.Time { return now },
	})

	day := 24 * time.Hour
	record := func(validTo time.Time) {
		src.records = []CertRecord{{MonitorID: 1, MonitorName: "web", ValidTo: validTo}}
	}
	sweep := func() {
		t.Helper()
		if err := sw.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	// Old certificate is nearly expired: alerts at the tight 3-day threshold.
	oldValidTo := now.Add(2 * day)
	record(oldValidTo)
	sweep()
	if len(events) != 1 || events[0].Threshold != 3 {
		t.Fatalf("initial events = %+v, want one 3-day event", events)
	}

	// Operator replaces it with a *different* certificate that is already
	// inside the 21-day window (a fresh 20-day cert). The dedup state from the
	// old cert (threshold 3) must not suppress the new cert's 21-day warning.
	newValidTo := now.Add(20 * day)
	record(newValidTo)
	sweep()
	sweep() // idempotent: the new cert alerts once, not repeatedly.
	if len(events) != 2 {
		t.Fatalf("events after cert replacement = %+v, want a fresh warning for the new cert", events)
	}
	if events[1].Threshold != 21 || events[1].ValidTo != newValidTo {
		t.Fatalf("replacement event = %+v, want a 21-day event for the new cert (ValidTo %v)",
			events[1], newValidTo)
	}
}
