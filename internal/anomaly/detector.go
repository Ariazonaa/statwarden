// Package anomaly detects response-time anomalies: a monitor that is still
// answering but has become unusually SLOW, before it fails outright.
//
// It keeps a rolling latency baseline per monitor as an exponentially weighted
// moving mean and variance (an O(1)-memory EWMA), scores every check against
// that baseline in standard deviations and flags one whose latency runs a
// configurable number of deviations ABOVE it. Only slowness is anomalous;
// a check that comes back faster than usual never is.
//
// To avoid alert spam the detector is edge-triggered and hysteretic: it raises
// a sustained anomaly only after N consecutive anomalous checks, and clears it
// again the first time latency returns to baseline. While an anomaly is active
// the baseline is frozen, so a sustained spike cannot drag the baseline up to
// itself and mask both the anomaly and its recovery.
//
// The detector is safe for concurrent use by multiple engine workers.
package anomaly

import (
	"math"
	"sync"
)

// Defaults applied by New for zero-valued Config fields. They are deliberately
// conservative: a wide 3.5σ threshold with a long warmup and three consecutive
// hits, so an enabled detector is slow to cry wolf.
const (
	// DefaultSensitivity is the number of standard deviations above baseline
	// that makes a single check anomalous.
	DefaultSensitivity = 3.5
	// DefaultMinSamples is how many checks seed the baseline before any
	// scoring begins (warmup).
	DefaultMinSamples = 30
	// DefaultConsecutive is how many consecutive anomalous checks must occur
	// before a sustained anomaly is raised.
	DefaultConsecutive = 3
	// DefaultAlpha is the EWMA smoothing factor: higher adapts faster to
	// shifts, lower keeps a longer memory.
	DefaultAlpha = 0.1
)

// Config parameterises a Detector. The zero value is filled in with the
// package defaults by New.
type Config struct {
	// Sensitivity is the deviation threshold (σ) above baseline; <= 0 uses
	// DefaultSensitivity.
	Sensitivity float64
	// MinSamples is the warmup length; <= 0 uses DefaultMinSamples.
	MinSamples int
	// Consecutive is the number of consecutive anomalous checks required to
	// raise; <= 0 uses DefaultConsecutive.
	Consecutive int
	// Alpha is the EWMA smoothing factor in (0,1]; out of range uses
	// DefaultAlpha.
	Alpha float64
}

// Result reports the outcome of one Observe call.
type Result struct {
	// Warming is true while the baseline is still being seeded (fewer than
	// MinSamples observations); no anomaly is ever flagged during warmup.
	Warming bool
	// Anomalous is true when this single check exceeded the threshold.
	Anomalous bool
	// Raised is true on the check that turns a run of anomalous checks into a
	// sustained anomaly — the edge an alert should fire on.
	Raised bool
	// Cleared is true on the check that ends an active anomaly (latency back
	// at baseline) — the edge a recovery should fire on.
	Cleared bool
	// Active reports whether the monitor is currently in a sustained anomaly.
	Active bool
	// Score is how many standard deviations above baseline this check sits
	// (0 when at or below baseline; +Inf against a zero-variance baseline).
	Score float64
	// Baseline is the current rolling mean latency, in the same unit passed
	// to Observe.
	Baseline float64
}

// Detector holds the per-monitor baselines. Create with New.
type Detector struct {
	cfg    Config
	mu     sync.Mutex
	states map[int64]*state
}

// state is one monitor's rolling baseline and anomaly bookkeeping.
type state struct {
	count       int     // observations seen (for warmup)
	mean        float64 // EWMA mean
	variance    float64 // EWMA variance
	consecutive int     // consecutive anomalous checks
	active      bool    // sustained anomaly in progress
}

// New builds a Detector, substituting defaults for zero/invalid Config fields.
func New(cfg Config) *Detector {
	if cfg.Sensitivity <= 0 {
		cfg.Sensitivity = DefaultSensitivity
	}
	if cfg.MinSamples <= 0 {
		cfg.MinSamples = DefaultMinSamples
	}
	if cfg.Consecutive <= 0 {
		cfg.Consecutive = DefaultConsecutive
	}
	if cfg.Alpha <= 0 || cfg.Alpha > 1 {
		cfg.Alpha = DefaultAlpha
	}
	return &Detector{cfg: cfg, states: make(map[int64]*state)}
}

// Observe folds one check's latency into monitorID's baseline and reports how
// it was classified. value is unit-agnostic (the engine passes nanoseconds);
// Result.Baseline is reported in the same unit.
func (d *Detector) Observe(monitorID int64, value float64) Result {
	d.mu.Lock()
	defer d.mu.Unlock()

	s := d.states[monitorID]
	if s == nil {
		s = &state{}
		d.states[monitorID] = s
	}
	s.count++

	// Warmup: seed the baseline, never score.
	if s.count <= d.cfg.MinSamples {
		s.update(value, d.cfg.Alpha)
		return Result{Warming: true, Baseline: s.mean}
	}

	// Score against the baseline as it stands BEFORE folding this value in.
	score := 0.0
	if std := math.Sqrt(s.variance); std > 0 {
		if score = (value - s.mean) / std; score < 0 {
			score = 0
		}
	} else if value > s.mean {
		// A jump away from a perfectly flat baseline is unambiguously anomalous.
		score = math.Inf(1)
	}

	res := Result{Score: score, Baseline: s.mean}
	if score > d.cfg.Sensitivity {
		res.Anomalous = true
		s.consecutive++
		// Freeze the baseline while anomalous (do not fold this value in) so a
		// sustained spike cannot pull the baseline up and hide itself.
		if !s.active && s.consecutive >= d.cfg.Consecutive {
			s.active = true
			res.Raised = true
		}
	} else {
		// Back at baseline: resume adapting and end any active anomaly.
		s.consecutive = 0
		s.update(value, d.cfg.Alpha)
		res.Baseline = s.mean
		if s.active {
			s.active = false
			res.Cleared = true
		}
	}
	res.Active = s.active
	return res
}

// Forget drops a monitor's baseline (e.g. when it is removed or reset).
func (d *Detector) Forget(monitorID int64) {
	d.mu.Lock()
	delete(d.states, monitorID)
	d.mu.Unlock()
}

// update folds value into the EWMA mean and variance. The first observation
// seeds the mean; thereafter it uses the standard incremental EWMA-variance
// recurrence, which is always non-negative.
func (s *state) update(value, alpha float64) {
	if s.count == 1 {
		s.mean = value
		s.variance = 0
		return
	}
	diff := value - s.mean
	incr := alpha * diff
	s.mean += incr
	s.variance = (1 - alpha) * (s.variance + diff*incr)
}
