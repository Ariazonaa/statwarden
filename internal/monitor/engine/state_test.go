package engine

import (
	"testing"

	"github.com/Ariazonaa/statwarden/internal/domain"
)

func TestTransition(t *testing.T) {
	up := domain.StatusUp
	down := domain.StatusDown
	pending := domain.StatusPending

	tests := []struct {
		name        string
		prev        domain.MonitorStatus
		retries     int
		maxRetries  int
		up          bool
		wantStatus  domain.MonitorStatus
		wantRetries int
		wantChanged bool
	}{
		// Success paths.
		{"up stays up", up, 0, 3, true, up, 0, false},
		{"pending recovers to up", pending, 2, 3, true, up, 0, true},
		{"down recovers to up", down, 4, 3, true, up, 0, true},
		{"initial pending goes up on first success", pending, 0, 3, true, up, 0, true},

		// Failure with retries: UP → PENDING → … → DOWN.
		{"first failure enters pending", up, 0, 2, false, pending, 1, true},
		{"second failure stays pending", pending, 1, 2, false, pending, 2, false},
		{"failure beyond maxRetries goes down", pending, 2, 2, false, down, 3, true},
		{"down stays down", down, 3, 2, false, down, 3, false},

		// maxRetries = 0: single failure goes straight to DOWN.
		{"no retries goes straight down", up, 0, 0, false, down, 1, true},

		// Brand-new monitor (DB default 'pending', retries 0).
		{"new monitor first failure stays pending", pending, 0, 1, false, pending, 1, false},
		{"new monitor exceeds retries", pending, 1, 1, false, down, 2, true},
		{"new monitor no retries goes down", pending, 0, 0, false, down, 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, retries, changed := Transition(tt.prev, tt.retries, tt.maxRetries, tt.up)
			if status != tt.wantStatus || retries != tt.wantRetries || changed != tt.wantChanged {
				t.Fatalf("Transition(%s, %d, %d, up=%v) = (%s, %d, %v), want (%s, %d, %v)",
					tt.prev, tt.retries, tt.maxRetries, tt.up,
					status, retries, changed, tt.wantStatus, tt.wantRetries, tt.wantChanged)
			}
		})
	}
}

// TestTransitionFullCycle walks the canonical UP → PENDING → DOWN → UP story
// end to end with maxRetries = 2.
func TestTransitionFullCycle(t *testing.T) {
	status := domain.StatusUp
	retries := 0

	step := func(up bool, wantStatus domain.MonitorStatus, wantChanged bool) {
		t.Helper()
		var changed bool
		status, retries, changed = Transition(status, retries, 2, up)
		if status != wantStatus || changed != wantChanged {
			t.Fatalf("step(up=%v) = (%s, changed=%v), want (%s, %v)", up, status, changed, wantStatus, wantChanged)
		}
	}

	step(false, domain.StatusPending, true) // failure 1
	step(false, domain.StatusPending, false)
	step(false, domain.StatusDown, true) // failure 3 > maxRetries
	step(false, domain.StatusDown, false)
	step(true, domain.StatusUp, true) // recovery
	if retries != 0 {
		t.Fatalf("retries after recovery = %d, want 0", retries)
	}
}

// TestStepConfirmsRecovery covers the recovery half of flapping damping: out of
// DOWN a monitor needs recoveryChecks consecutive successes, a failure in
// between starts the count over, and nothing but DOWN is affected.
func TestStepConfirmsRecovery(t *testing.T) {
	up := domain.StatusUp
	down := domain.StatusDown
	pending := domain.StatusPending

	tests := []struct {
		name          string
		prev          domain.MonitorStatus
		successes     int
		recovery      int
		up            bool
		wantStatus    domain.MonitorStatus
		wantSuccesses int
		wantChanged   bool
	}{
		{"first of three stays down", down, 0, 3, true, down, 1, false},
		{"second of three stays down", down, 1, 3, true, down, 2, false},
		{"third of three recovers", down, 2, 3, true, up, 0, true},
		{"a failure restarts the count", down, 2, 3, false, down, 0, false},
		{"one means a single success", down, 0, 1, true, up, 0, true},
		{"zero means a single success", down, 0, 0, true, up, 0, true},
		// PENDING was never announced; confirming its recovery only delays.
		{"pending recovers at once", pending, 0, 3, true, up, 0, true},
		{"up stays up", up, 0, 3, true, up, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, _, successes, changed := Step(tt.prev, 5, tt.successes, 2, tt.recovery, tt.up)
			if status != tt.wantStatus || successes != tt.wantSuccesses || changed != tt.wantChanged {
				t.Fatalf("Step(%s, successes=%d, recovery=%d, up=%v) = (%s, %d, %v), want (%s, %d, %v)",
					tt.prev, tt.successes, tt.recovery, tt.up,
					status, successes, changed, tt.wantStatus, tt.wantSuccesses, tt.wantChanged)
			}
		})
	}
}
