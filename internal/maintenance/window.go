package maintenance

import (
	"fmt"
	"time"
)

// Window is the evaluator's view of a maintenance_windows row. It is a plain
// value type on purpose: the api package converts its own MaintenanceWindow
// into this, so this package stays free of API and storage dependencies.
//
// AppliesToAll is the window's SCOPE, and it is a field rather than something
// inferred from MonitorIDs being empty.
//
// The old convention — no ids means every monitor — was safe only while a human
// was the only thing that could change the set. maintenance_monitors cascades on
// a monitor delete, so destroying the last monitor a window covered emptied the
// set, and an emptied set read as the fleet-wide sentinel. The retention sweep
// does that on a timer, roughly two weeks after somebody binned the monitor: a
// window scoped to one machine silently began suppressing alerts for everything
// and announcing itself on every public status page, with no action by anyone.
//
// Scope now changes only when someone changes it. See migration 0019.
type Window struct {
	ID           int64
	Title        string
	Active       bool
	Strategy     string
	StartsAt     *time.Time
	EndsAt       *time.Time
	Recurrence   *Recurrence
	Timezone     string
	AppliesToAll bool
	MonitorIDs   []int64
}

// Validate checks a window end to end, including its timezone and recurrence.
// Errors name the offending field so the API can surface them in a 400. An
// unknown timezone fails here, at write time, rather than degrading to a
// silent UTC fallback when the window is later evaluated.
func (w Window) Validate() error {
	if w.Timezone == "" {
		return fmt.Errorf("timezone is required")
	}
	if _, err := time.LoadLocation(w.Timezone); err != nil {
		return fmt.Errorf("timezone %q is not a known IANA location", w.Timezone)
	}
	switch w.Strategy {
	case StrategySingle:
		if w.StartsAt == nil {
			return fmt.Errorf("starts_at is required for strategy %q", StrategySingle)
		}
		if w.EndsAt == nil {
			return fmt.Errorf("ends_at is required for strategy %q", StrategySingle)
		}
		if !w.EndsAt.After(*w.StartsAt) {
			return fmt.Errorf("ends_at must be after starts_at")
		}
	case StrategyRecurring:
		if w.Recurrence == nil {
			return fmt.Errorf("recurrence is required for strategy %q", StrategyRecurring)
		}
		if err := w.Recurrence.Validate(); err != nil {
			return err
		}
		if w.StartsAt != nil && w.EndsAt != nil && !w.EndsAt.After(*w.StartsAt) {
			return fmt.Errorf("ends_at must be after starts_at")
		}
	default:
		return fmt.Errorf("strategy must be %q or %q, got %q", StrategySingle, StrategyRecurring, w.Strategy)
	}
	return nil
}

// compiled is a window with its timezone resolved and its recurrence unpacked,
// so that evaluation is pure arithmetic with no allocation or lookup.
type compiled struct {
	win      Window
	loc      *time.Location
	monitors map[int64]struct{}

	// recurring fields
	weekdays [7]bool
	hour     int
	minute   int
	dur      time.Duration
	// lookback is how many days back an occurrence may have started and
	// still be running, i.e. how far the scan has to walk.
	lookback int
}

// compile validates the window and resolves everything evaluation needs.
func compile(w Window) (compiled, error) {
	if err := w.Validate(); err != nil {
		return compiled{}, fmt.Errorf("maintenance window %d: %w", w.ID, err)
	}
	loc, err := time.LoadLocation(w.Timezone)
	if err != nil {
		return compiled{}, fmt.Errorf("maintenance window %d: %w", w.ID, err)
	}
	c := compiled{win: w, loc: loc}
	// A nil c.monitors means "every monitor" (see coversMonitor). It is built
	// from the SCOPE flag, so a window that lost its last monitor to a cascade
	// stays scoped — and covers nothing — instead of widening to the fleet.
	if !w.AppliesToAll {
		c.monitors = make(map[int64]struct{}, len(w.MonitorIDs))
		for _, id := range w.MonitorIDs {
			c.monitors[id] = struct{}{}
		}
	}
	if w.Strategy == StrategyRecurring {
		for _, wd := range w.Recurrence.Weekdays {
			c.weekdays[wd] = true
		}
		c.hour, c.minute, _ = parseClock(w.Recurrence.Start)
		c.dur = time.Duration(w.Recurrence.DurationMinutes) * time.Minute
		// An occurrence starting d days ago is still running iff its
		// duration exceeds d days. +1 covers the same-day case and the
		// hour-of-day offset.
		c.lookback = w.Recurrence.DurationMinutes/(24*60) + 1
	}
	return c, nil
}

// covers reports whether the window is in effect at at.
func (c compiled) covers(at time.Time) bool {
	if !c.win.Active {
		return false
	}
	switch c.win.Strategy {
	case StrategySingle:
		// Half-open [starts_at, ends_at): a window ending at 03:00 does
		// not suppress the check that runs exactly at 03:00.
		return !at.Before(*c.win.StartsAt) && at.Before(*c.win.EndsAt)
	case StrategyRecurring:
		// starts_at / ends_at, when present, bound the series.
		if c.win.StartsAt != nil && at.Before(*c.win.StartsAt) {
			return false
		}
		if c.win.EndsAt != nil && !at.Before(*c.win.EndsAt) {
			return false
		}
		return c.coversRecurring(at)
	}
	return false
}

// coversRecurring walks back over the candidate start days and asks whether any
// occurrence that began on one of them is still running at at.
//
// DST, deliberately:
//
//   - Occurrences are anchored to LOCAL WALL CLOCK. A 02:00 window is 02:00
//     local on every occurrence, on both sides of a DST change; it is not
//     pinned to a fixed UTC offset.
//   - Duration is ELAPSED time, not wall-clock arithmetic. A 120-minute window
//     always covers 120 real minutes, so on an autumn-back day it ends at a
//     wall-clock time one hour earlier than usual and on a spring-forward day
//     one hour later. Alerts stay suppressed for exactly as long as asked.
//   - Spring forward, when the local start time does not exist (02:00 on the
//     Berlin March transition): time.Date normalises the gap forward, so the
//     occurrence begins at the instant the clock jumps — 03:00 local — and
//     still runs for its full duration. It fires; it neither panics nor
//     silently disappears.
//   - Autumn back, when the local start time happens twice: startOn picks the
//     first (pre-transition) 02:00, so the window opens at the earlier of the
//     two. It fires once, not twice.
func (c compiled) coversRecurring(at time.Time) bool {
	local := at.In(c.loc)
	for back := 0; back <= c.lookback; back++ {
		y, m, d := local.AddDate(0, 0, -back).Date()
		// Take the weekday from midday, which no transition can shift
		// across a date boundary.
		if !c.weekdays[time.Date(y, m, d, 12, 0, 0, 0, c.loc).Weekday()] {
			continue
		}
		start := c.startOn(y, m, d)
		if !at.Before(start) && at.Before(start.Add(c.dur)) {
			return true
		}
	}
	return false
}

// foldProbes are the DST fold sizes in use worldwide (Lord Howe shifts 30
// minutes, almost everywhere else 60, a few historical zones 120).
var foldProbes = [...]time.Duration{2 * time.Hour, time.Hour, 30 * time.Minute}

// startOn resolves the occurrence's local start time on the given calendar day
// to an instant, picking the EARLIEST instant with that wall clock when the
// day has a DST fold and the time therefore happens twice. time.Date alone
// does not settle this — its documentation says the choice in an ambiguous
// hour "is not guaranteed" — so the earlier instant is selected explicitly and
// the behaviour cannot drift with the Go runtime.
func (c compiled) startOn(y int, m time.Month, d int) time.Time {
	start := time.Date(y, m, d, c.hour, c.minute, 0, 0, c.loc)
	for _, back := range foldProbes {
		cand := start.Add(-back)
		cy, cm, cd := cand.Date()
		if cy == y && cm == m && cd == d && cand.Hour() == c.hour && cand.Minute() == c.minute {
			return cand
		}
	}
	return start
}

// appliesTo reports whether the window covers this monitor.
//
// A nil c.monitors means the window is fleet-wide, and that is now built from
// the AppliesToAll flag rather than from the id set being empty — so a SCOPED
// window whose last monitor was destroyed covers nothing, instead of widening
// to everything. See Window.AppliesToAll.
func (c compiled) appliesTo(monitorID int64) bool {
	if c.monitors == nil {
		return true
	}
	_, ok := c.monitors[monitorID]
	return ok
}
