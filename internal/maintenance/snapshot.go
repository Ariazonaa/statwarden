package maintenance

import (
	"errors"
	"time"
)

// Snapshot is an immutable, in-memory view of the maintenance windows, built
// once per refresh and read on every state transition. It satisfies
// engine.MaintenanceEvaluator.
//
// A nil *Snapshot is usable and reports no maintenance, so callers that have
// not loaded one yet need no special case.
type Snapshot struct {
	windows []compiled
}

// NewSnapshot compiles the given windows. Windows that fail validation are
// skipped rather than poisoning the whole snapshot — one bad row must not
// disable maintenance for everything else — and are reported in the returned
// error, which the caller should log. The returned Snapshot is always usable.
func NewSnapshot(windows []Window) (*Snapshot, error) {
	s := &Snapshot{windows: make([]compiled, 0, len(windows))}
	var errs []error
	for _, w := range windows {
		c, err := compile(w)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		s.windows = append(s.windows, c)
	}
	return s, errors.Join(errs...)
}

// InMaintenance implements engine.MaintenanceEvaluator: it reports whether the
// monitor is inside a window that is in effect at at. It only ever suppresses
// ALERTS — probing and heartbeat recording are unaffected by this package.
func (s *Snapshot) InMaintenance(monitorID int64, at time.Time) bool {
	if s == nil {
		return false
	}
	for _, c := range s.windows {
		if c.appliesTo(monitorID) && c.covers(at) {
			return true
		}
	}
	return false
}

// ActiveAt returns the windows in effect at at, in the order they were given.
// The status page uses this so that "currently in maintenance" means exactly
// the same thing there as it does in the engine.
func (s *Snapshot) ActiveAt(at time.Time) []Window {
	if s == nil {
		return nil
	}
	var out []Window
	for _, c := range s.windows {
		if c.covers(at) {
			out = append(out, c.win)
		}
	}
	return out
}

// Len is the number of compiled (valid) windows in the snapshot.
func (s *Snapshot) Len() int {
	if s == nil {
		return 0
	}
	return len(s.windows)
}
