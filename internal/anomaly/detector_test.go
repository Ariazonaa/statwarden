package anomaly_test

import (
	"testing"

	"github.com/Ariazonaa/statwarden/internal/anomaly"
)

// step is one observation and the flags Observe is expected to return for it.
// Only the booleans are asserted; the exact Score/Baseline are left to the
// EWMA maths and covered indirectly by the flags.
type step struct {
	value     float64
	warming   bool
	anomalous bool
	raised    bool
	cleared   bool
	active    bool
}

// A stable-ish warmup that leaves a baseline around 100 with non-zero variance,
// reused by several cases. With MinSamples=5 these five are all warmup.
var warmup = []step{
	{value: 100, warming: true},
	{value: 110, warming: true},
	{value: 90, warming: true},
	{value: 105, warming: true},
	{value: 95, warming: true},
}

func seq(steps ...[]step) []step {
	var out []step
	for _, s := range steps {
		out = append(out, s...)
	}
	return out
}

func TestDetector(t *testing.T) {
	cfg := anomaly.Config{Sensitivity: 3, MinSamples: 5, Consecutive: 3, Alpha: 0.3}

	cases := []struct {
		name  string
		cfg   anomaly.Config
		steps []step
	}{
		{
			// During warmup nothing is ever flagged — not even a gross spike —
			// because there is not yet a baseline to judge against.
			name: "baseline warmup: no scoring until MinSamples",
			cfg:  cfg,
			steps: []step{
				{value: 100, warming: true},
				{value: 5000, warming: true}, // huge, still warmup
				{value: 100, warming: true},
				{value: 100, warming: true},
				{value: 100, warming: true},
			},
		},
		{
			// After warmup a sustained spike raises exactly on the Consecutive-th
			// anomalous check, not before.
			name: "spike detection: raises on the Nth consecutive anomaly",
			cfg:  cfg,
			steps: seq(warmup, []step{
				{value: 900, anomalous: true},                             // 1st
				{value: 900, anomalous: true},                             // 2nd
				{value: 900, anomalous: true, raised: true, active: true}, // 3rd → raised
				{value: 900, anomalous: true, active: true},               // stays active, no re-raise
			}),
		},
		{
			// A single spike below the Consecutive threshold never raises.
			name: "insufficient consecutive: two spikes do not raise",
			cfg:  cfg,
			steps: seq(warmup, []step{
				{value: 900, anomalous: true},
				{value: 900, anomalous: true},
				{value: 100}, // back to baseline before the 3rd → resets, no raise
			}),
		},
		{
			// Once active, the first check back at baseline clears the anomaly.
			name: "recovery: clears when latency returns to baseline",
			cfg:  cfg,
			steps: seq(warmup, []step{
				{value: 900, anomalous: true},
				{value: 900, anomalous: true},
				{value: 900, anomalous: true, raised: true, active: true},
				{value: 100, cleared: true}, // back at baseline → cleared
				{value: 100},                // stays normal, no repeat clear
			}),
		},
		{
			// After a clear the detector can raise again on a fresh spike run —
			// the edges retrigger.
			name: "re-arm: raises again after a recovery",
			cfg:  cfg,
			steps: seq(warmup, []step{
				{value: 900, anomalous: true},
				{value: 900, anomalous: true},
				{value: 900, anomalous: true, raised: true, active: true},
				{value: 100, cleared: true},
				{value: 900, anomalous: true},
				{value: 900, anomalous: true},
				{value: 900, anomalous: true, raised: true, active: true},
			}),
		},
		{
			// Faster-than-usual checks are never anomalous; only slowness is.
			name: "fast checks are never anomalous",
			cfg:  cfg,
			steps: seq(warmup, []step{
				{value: 5},
				{value: 1},
				{value: 50},
			}),
		},
		{
			// With a longer warmup, a short burst never leaves warmup and so
			// never raises regardless of how anomalous it looks.
			name: "insufficient samples: never leaves warmup",
			cfg:  anomaly.Config{Sensitivity: 3, MinSamples: 10, Consecutive: 3, Alpha: 0.3},
			steps: []step{
				{value: 100, warming: true},
				{value: 900, warming: true},
				{value: 100, warming: true},
				{value: 900, warming: true},
				{value: 100, warming: true},
				{value: 900, warming: true},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := anomaly.New(tc.cfg)
			const id = int64(1)
			for i, s := range tc.steps {
				got := d.Observe(id, s.value)
				if got.Warming != s.warming ||
					got.Anomalous != s.anomalous ||
					got.Raised != s.raised ||
					got.Cleared != s.cleared ||
					got.Active != s.active {
					t.Fatalf("step %d (value=%.0f):\n  got  warming=%v anomalous=%v raised=%v cleared=%v active=%v\n  want warming=%v anomalous=%v raised=%v cleared=%v active=%v",
						i, s.value,
						got.Warming, got.Anomalous, got.Raised, got.Cleared, got.Active,
						s.warming, s.anomalous, s.raised, s.cleared, s.active)
				}
			}
		})
	}
}

// TestDetectorZeroVarianceBaseline pins the edge case of a perfectly flat
// baseline: a jump above it scores +Inf and is treated as anomalous, so a
// stall is still caught even when historical latency never varied.
func TestDetectorZeroVarianceBaseline(t *testing.T) {
	d := anomaly.New(anomaly.Config{Sensitivity: 3, MinSamples: 3, Consecutive: 2, Alpha: 0.3})
	const id = int64(7)
	for i := 0; i < 3; i++ {
		if got := d.Observe(id, 100); !got.Warming {
			t.Fatalf("warmup step %d: Warming=false, want true", i)
		}
	}
	if got := d.Observe(id, 100); got.Anomalous {
		t.Fatalf("steady value flagged anomalous against flat baseline: %+v", got)
	}
	if got := d.Observe(id, 500); !got.Anomalous {
		t.Fatalf("jump above flat baseline not anomalous: %+v", got)
	}
	got := d.Observe(id, 500)
	if !got.Raised || !got.Active {
		t.Fatalf("second jump did not raise: %+v", got)
	}
}

// TestDetectorForget drops a monitor's baseline so it warms up again.
func TestDetectorForget(t *testing.T) {
	d := anomaly.New(anomaly.Config{MinSamples: 2})
	const id = int64(3)
	d.Observe(id, 100) // warmup 1
	d.Observe(id, 100) // warmup 2
	if got := d.Observe(id, 100); got.Warming {
		t.Fatalf("observation past MinSamples=2 still warming: %+v", got)
	}
	d.Forget(id)
	if got := d.Observe(id, 100); !got.Warming {
		t.Fatalf("after Forget the baseline did not reset to warmup: %+v", got)
	}
}

// TestDetectorDefaults verifies New fills in the package defaults for a zero
// Config, and that the resulting detector needs a full DefaultMinSamples warmup.
func TestDetectorDefaults(t *testing.T) {
	d := anomaly.New(anomaly.Config{})
	const id = int64(1)
	for i := 0; i < anomaly.DefaultMinSamples; i++ {
		if got := d.Observe(id, 100); !got.Warming {
			t.Fatalf("observation %d: Warming=false, want warmup of %d samples", i, anomaly.DefaultMinSamples)
		}
	}
	if got := d.Observe(id, 100); got.Warming {
		t.Fatalf("observation past DefaultMinSamples still warming: %+v", got)
	}
}
