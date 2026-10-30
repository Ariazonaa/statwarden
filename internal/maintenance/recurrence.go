// Package maintenance models maintenance windows and decides whether a given
// monitor is inside one at a given instant. It is the single source of truth
// for that question: the monitor engine uses it to suppress alerts, and the
// public status page uses it to decide which windows are in effect. Evaluation
// works from an in-memory Snapshot, so answering costs no database round trip.
package maintenance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// Strategy values stored in maintenance_windows.strategy.
const (
	StrategySingle    = "single"
	StrategyRecurring = "recurring"
)

// maxDurationMinutes is seven days. A recurring occurrence longer than that
// would overlap the next occurrence of the same weekday, which makes "the
// window" ill-defined, so it is rejected at write time.
const maxDurationMinutes = 7 * 24 * 60

// Recurrence is the payload stored in maintenance_windows.recurrence for
// strategy "recurring":
//
//	{"weekdays": [1,2,3], "start": "02:00", "duration_minutes": 120}
//
// Weekdays are time.Weekday values (0 = Sunday). Start is a local wall-clock
// time in the window's own timezone; the occurrence runs for DurationMinutes
// of elapsed time from there and may cross midnight.
type Recurrence struct {
	Weekdays        []time.Weekday `json:"weekdays"`
	Start           string         `json:"start"`
	DurationMinutes int            `json:"duration_minutes"`
}

// recurrenceWire mirrors Recurrence without its UnmarshalJSON method, breaking
// the recursion in the strict decoder below.
type recurrenceWire struct {
	Weekdays        []time.Weekday `json:"weekdays"`
	Start           string         `json:"start"`
	DurationMinutes int            `json:"duration_minutes"`
}

// UnmarshalJSON decodes strictly: unknown fields are an error rather than
// being silently dropped, so a typo like "duration" cannot produce a window
// that quietly never fires.
func (r *Recurrence) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var wire recurrenceWire
	if err := dec.Decode(&wire); err != nil {
		return fmt.Errorf("recurrence: %w", err)
	}
	*r = Recurrence(wire)
	return nil
}

// ParseRecurrence decodes and validates a raw recurrence payload.
func ParseRecurrence(raw []byte) (*Recurrence, error) {
	var rec Recurrence
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, err
	}
	if err := rec.Validate(); err != nil {
		return nil, err
	}
	return &rec, nil
}

// Validate reports the first problem with the recurrence, naming the offending
// field so the API can return it verbatim in a 400.
func (r Recurrence) Validate() error {
	if len(r.Weekdays) == 0 {
		return fmt.Errorf("recurrence.weekdays must list at least one weekday")
	}
	var seen [7]bool
	for _, wd := range r.Weekdays {
		if wd < time.Sunday || wd > time.Saturday {
			return fmt.Errorf("recurrence.weekdays contains %d: must be 0..6 (0 = Sunday)", int(wd))
		}
		if seen[wd] {
			return fmt.Errorf("recurrence.weekdays contains duplicate weekday %d", int(wd))
		}
		seen[wd] = true
	}
	if _, _, err := parseClock(r.Start); err != nil {
		return err
	}
	if r.DurationMinutes <= 0 {
		return fmt.Errorf("recurrence.duration_minutes must be greater than 0")
	}
	if r.DurationMinutes > maxDurationMinutes {
		return fmt.Errorf("recurrence.duration_minutes must not exceed %d (7 days); a longer window would overlap the next occurrence of the same weekday", maxDurationMinutes)
	}
	return nil
}

// parseClock accepts exactly HH:MM in 00:00..23:59. time.Parse is deliberately
// not used: it also accepts single-digit hours and trailing garbage variants
// that we do not want to store.
func parseClock(s string) (hour, min int, err error) {
	if len(s) != 5 || s[2] != ':' {
		return 0, 0, fmt.Errorf("recurrence.start must be HH:MM, got %q", s)
	}
	for i, c := range []byte(s) {
		if i == 2 {
			continue
		}
		if c < '0' || c > '9' {
			return 0, 0, fmt.Errorf("recurrence.start must be HH:MM, got %q", s)
		}
	}
	hour = int(s[0]-'0')*10 + int(s[1]-'0')
	min = int(s[3]-'0')*10 + int(s[4]-'0')
	if hour > 23 || min > 59 {
		return 0, 0, fmt.Errorf("recurrence.start must be within 00:00..23:59, got %q", s)
	}
	return hour, min, nil
}
