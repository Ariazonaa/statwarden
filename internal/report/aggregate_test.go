package report

import (
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
)

// TestDowntimeLogOngoingClosesAtRangeEnd reproduces the push-monitor case: a
// single DOWN heartbeat with no recovery and no further beats. The outage is
// still open at the end of the data, so its End must run to the end of the
// report window (which the service caps at min(to, now)), not merely to that
// lone heartbeat — otherwise the reported duration collapses to ~0 despite the
// outage still being in progress.
func TestDowntimeLogOngoingClosesAtRangeEnd(t *testing.T) {
	to := base.Add(time.Hour)
	beats := []domain.Heartbeat{
		hb(0, domain.StatusDown, -1, true), // lone DOWN beat; no later heartbeat
	}
	log := DowntimeLog(beats, base, to, nil)
	if len(log) != 1 {
		t.Fatalf("downtime count = %d, want 1: %+v", len(log), log)
	}
	d := log[0]
	if !d.Ongoing {
		t.Fatalf("outage should be ongoing: %+v", d)
	}
	if !d.End.Equal(to) {
		t.Fatalf("ongoing outage End = %v, want range end %v", d.End, to)
	}
	if d.Duration != time.Hour {
		t.Fatalf("ongoing outage Duration = %v, want %v (must not collapse to ~0)", d.Duration, time.Hour)
	}
}

// TestDowntimeLogOngoingWithLaterBeatStillReachesEnd guards that even when a
// later (still DOWN) heartbeat exists, the ongoing outage is closed at the
// report end rather than at that intermediate heartbeat.
func TestDowntimeLogOngoingWithLaterBeatStillReachesEnd(t *testing.T) {
	to := base.Add(time.Hour)
	beats := []domain.Heartbeat{
		hb(0, domain.StatusDown, -1, true),
		hb(120, domain.StatusDown, -1, false), // last beat well before `to`
	}
	log := DowntimeLog(beats, base, to, nil)
	if len(log) != 1 || !log[0].Ongoing {
		t.Fatalf("want one ongoing outage, got %+v", log)
	}
	if !log[0].End.Equal(to) || log[0].Duration != time.Hour {
		t.Fatalf("ongoing outage = %+v, want End %v / Duration %v", log[0], to, time.Hour)
	}
}

// The latency average is rounded, not floored.
//
// Integer division biased it consistently downward: three checks at 10ms, 10ms
// and 9ms average 9.67ms and were reported as 9ms — below every value but one in
// the set. Avg is the only statistic here that is not a sample lifted verbatim
// from the data, so it is also the only one that can be off by rounding at all.
func TestLatencyAverageIsRoundedNotTruncated(t *testing.T) {
	to := base.Add(time.Hour)
	beats := []domain.Heartbeat{
		hb(0, domain.StatusUp, 10, false),
		hb(1, domain.StatusUp, 10, false),
		hb(2, domain.StatusUp, 9, false),
	}
	if got := LatencyPercentiles(beats, base, to).Avg; got != 10 {
		t.Fatalf("Avg = %d, want 10 (29/3 = 9.67 rounds to 10, truncation gives 9)", got)
	}

	// Rounding down is still correct when that is the nearer integer.
	beats = []domain.Heartbeat{
		hb(0, domain.StatusUp, 10, false),
		hb(1, domain.StatusUp, 10, false),
		hb(2, domain.StatusUp, 1, false),
	}
	if got := LatencyPercentiles(beats, base, to).Avg; got != 7 {
		t.Fatalf("Avg = %d, want 7 (21/3 = 7 exactly)", got)
	}
}
