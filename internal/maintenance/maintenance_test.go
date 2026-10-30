package maintenance

import (
	"encoding/json"
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }

func utc(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, time.UTC)
}

// recurringWindow is a valid recurring window in Europe/Berlin unless the test
// overrides fields.
func recurringWindow(weekdays []time.Weekday, start string, mins int) Window {
	return Window{
		ID:       1,
		Title:    "nightly",
		Active:   true,
		Strategy: StrategyRecurring,
		Timezone: "Europe/Berlin",
		// Fleet-wide, stated explicitly. Scope is no longer inferred from an
		// empty id list — see Window.AppliesToAll — so a fixture that wants to
		// cover every monitor has to say so.
		AppliesToAll: true,
		Recurrence:   &Recurrence{Weekdays: weekdays, Start: start, DurationMinutes: mins},
	}
}

func TestRecurrenceValidate(t *testing.T) {
	tests := []struct {
		name    string
		rec     Recurrence
		wantErr bool
	}{
		{"ok", Recurrence{Weekdays: []time.Weekday{time.Monday}, Start: "02:00", DurationMinutes: 120}, false},
		{"ok all weekdays", Recurrence{Weekdays: []time.Weekday{0, 1, 2, 3, 4, 5, 6}, Start: "00:00", DurationMinutes: 1}, false},
		{"ok max duration", Recurrence{Weekdays: []time.Weekday{0}, Start: "23:59", DurationMinutes: 7 * 24 * 60}, false},
		{"empty weekdays", Recurrence{Weekdays: nil, Start: "02:00", DurationMinutes: 60}, true},
		{"weekday too high", Recurrence{Weekdays: []time.Weekday{7}, Start: "02:00", DurationMinutes: 60}, true},
		{"weekday negative", Recurrence{Weekdays: []time.Weekday{-1}, Start: "02:00", DurationMinutes: 60}, true},
		{"duplicate weekday", Recurrence{Weekdays: []time.Weekday{2, 2}, Start: "02:00", DurationMinutes: 60}, true},
		{"start empty", Recurrence{Weekdays: []time.Weekday{0}, Start: "", DurationMinutes: 60}, true},
		{"start single digit hour", Recurrence{Weekdays: []time.Weekday{0}, Start: "2:00", DurationMinutes: 60}, true},
		{"start with seconds", Recurrence{Weekdays: []time.Weekday{0}, Start: "02:00:00", DurationMinutes: 60}, true},
		{"start hour 24", Recurrence{Weekdays: []time.Weekday{0}, Start: "24:00", DurationMinutes: 60}, true},
		{"start minute 60", Recurrence{Weekdays: []time.Weekday{0}, Start: "02:60", DurationMinutes: 60}, true},
		{"start non numeric", Recurrence{Weekdays: []time.Weekday{0}, Start: "ab:cd", DurationMinutes: 60}, true},
		{"duration zero", Recurrence{Weekdays: []time.Weekday{0}, Start: "02:00", DurationMinutes: 0}, true},
		{"duration negative", Recurrence{Weekdays: []time.Weekday{0}, Start: "02:00", DurationMinutes: -5}, true},
		{"duration over seven days", Recurrence{Weekdays: []time.Weekday{0}, Start: "02:00", DurationMinutes: 7*24*60 + 1}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.rec.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("Validate() = nil, want error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestRecurrenceUnmarshalStrict(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr bool
		want    Recurrence
	}{
		{
			name: "ok",
			raw:  `{"weekdays":[1,2,3],"start":"02:00","duration_minutes":120}`,
			want: Recurrence{Weekdays: []time.Weekday{1, 2, 3}, Start: "02:00", DurationMinutes: 120},
		},
		{"unknown field", `{"weekdays":[1],"start":"02:00","duration":120}`, true, Recurrence{}},
		{"weekdays wrong type", `{"weekdays":"monday","start":"02:00","duration_minutes":10}`, true, Recurrence{}},
		{"not an object", `[1,2,3]`, true, Recurrence{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got Recurrence
			err := json.Unmarshal([]byte(tc.raw), &got)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Unmarshal(%s) = nil, want error", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("Unmarshal(%s) = %v", tc.raw, err)
			}
			if got.Start != tc.want.Start || got.DurationMinutes != tc.want.DurationMinutes || len(got.Weekdays) != len(tc.want.Weekdays) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseRecurrenceRejectsInvalidPayload(t *testing.T) {
	if _, err := ParseRecurrence([]byte(`{"weekdays":[],"start":"02:00","duration_minutes":60}`)); err == nil {
		t.Fatal("ParseRecurrence() = nil error for empty weekdays, want error")
	}
	rec, err := ParseRecurrence([]byte(`{"weekdays":[6],"start":"23:00","duration_minutes":60}`))
	if err != nil {
		t.Fatalf("ParseRecurrence() = %v", err)
	}
	if rec.DurationMinutes != 60 || rec.Weekdays[0] != time.Saturday {
		t.Fatalf("got %+v", rec)
	}
}

func TestWindowValidate(t *testing.T) {
	start := utc(2026, 7, 1, 10, 0)
	end := utc(2026, 7, 1, 12, 0)
	tests := []struct {
		name    string
		win     Window
		wantErr bool
	}{
		{"single ok", Window{Active: true, Strategy: StrategySingle, Timezone: "UTC", StartsAt: &start, EndsAt: &end}, false},
		{"single missing starts_at", Window{Strategy: StrategySingle, Timezone: "UTC", EndsAt: &end}, true},
		{"single missing ends_at", Window{Strategy: StrategySingle, Timezone: "UTC", StartsAt: &start}, true},
		{"single ends before starts", Window{Strategy: StrategySingle, Timezone: "UTC", StartsAt: &end, EndsAt: &start}, true},
		{"recurring ok", recurringWindow([]time.Weekday{1}, "02:00", 60), false},
		{"recurring missing recurrence", Window{Strategy: StrategyRecurring, Timezone: "UTC"}, true},
		{"unknown strategy", Window{Strategy: "weekly", Timezone: "UTC"}, true},
		{"empty timezone", Window{Strategy: StrategySingle, StartsAt: &start, EndsAt: &end}, true},
		{"unknown timezone", Window{Strategy: StrategySingle, Timezone: "Mars/Olympus", StartsAt: &start, EndsAt: &end}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.win.Validate()
			if tc.wantErr != (err != nil) {
				t.Fatalf("Validate() = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// TestSnapshotInMaintenance is the core occurrence table. Every case pins a
// single window and an instant, so a regression in weekday handling, midnight
// crossing, DST anchoring or monitor scoping flips exactly one row.
func TestSnapshotInMaintenance(t *testing.T) {
	singleStart := utc(2026, 7, 20, 22, 0)
	singleEnd := utc(2026, 7, 21, 2, 0)
	single := Window{
		ID: 10, Title: "db upgrade", Active: true, Strategy: StrategySingle,
		Timezone: "UTC", StartsAt: &singleStart, EndsAt: &singleEnd,
		AppliesToAll: true,
	}

	// 2026-07-20 is a Monday. Berlin is UTC+2 in July.
	monday0200 := recurringWindow([]time.Weekday{time.Monday}, "02:00", 120)

	// Friday 23:00 + 4h, crossing midnight into Saturday.
	// 2026-07-24 is a Friday.
	fridayNight := recurringWindow([]time.Weekday{time.Friday}, "23:00", 240)

	// Sunday 02:00 + 2h in Berlin, evaluated across both DST transitions.
	// 2026-03-29 springs forward (02:00 -> 03:00), 2026-10-25 falls back.
	sundayDST := recurringWindow([]time.Weekday{time.Sunday}, "02:00", 120)

	// Six-day occurrence starting Monday: exercises the multi-day lookback.
	longRun := recurringWindow([]time.Weekday{time.Monday}, "00:00", 6*24*60)

	scoped := single
	scoped.ID = 11
	scoped.AppliesToAll = false
	scoped.MonitorIDs = []int64{7, 9}

	inactive := single
	inactive.ID = 12
	inactive.Active = false

	inactiveRecurring := monday0200
	inactiveRecurring.ID = 13
	inactiveRecurring.Active = false

	// A recurring window whose series ended before the occurrence under test.
	expired := monday0200
	expired.ID = 14
	expired.EndsAt = ptr(utc(2026, 7, 1, 0, 0))

	tests := []struct {
		name    string
		win     Window
		monitor int64
		at      time.Time
		want    bool
	}{
		// --- single ---
		{"single before", single, 1, utc(2026, 7, 20, 21, 59), false},
		{"single at start is inside", single, 1, utc(2026, 7, 20, 22, 0), true},
		{"single during", single, 1, utc(2026, 7, 21, 0, 0), true},
		{"single at end is outside", single, 1, utc(2026, 7, 21, 2, 0), false},
		{"single after", single, 1, utc(2026, 7, 21, 3, 0), false},

		// --- recurring, right and wrong weekday ---
		// Monday 02:00 Berlin == Monday 00:00 UTC in July.
		{"recurring monday before start", monday0200, 1, utc(2026, 7, 20, 23, 59), false},
		{"recurring monday at start", monday0200, 1, utc(2026, 7, 20, 0, 0), true},
		{"recurring monday during", monday0200, 1, utc(2026, 7, 20, 1, 30), true},
		{"recurring monday at end is outside", monday0200, 1, utc(2026, 7, 20, 2, 0), false},
		{"recurring wrong weekday (tuesday)", monday0200, 1, utc(2026, 7, 21, 1, 0), false},
		{"recurring wrong weekday (sunday)", monday0200, 1, utc(2026, 7, 19, 1, 0), false},

		// --- midnight crossing: Fri 23:00 Berlin -> Sat 03:00 Berlin ---
		{"crossing before", fridayNight, 1, utc(2026, 7, 24, 20, 59), false},
		{"crossing at friday start", fridayNight, 1, utc(2026, 7, 24, 21, 0), true},
		{"crossing just before midnight local", fridayNight, 1, utc(2026, 7, 24, 21, 59), true},
		{"crossing after midnight into saturday", fridayNight, 1, utc(2026, 7, 24, 23, 30), true},
		{"crossing saturday 02:00 local", fridayNight, 1, utc(2026, 7, 25, 0, 0), true},
		{"crossing saturday at end", fridayNight, 1, utc(2026, 7, 25, 1, 0), false},
		{"crossing saturday night is not an occurrence", fridayNight, 1, utc(2026, 7, 25, 21, 30), false},

		// --- DST spring forward, Europe/Berlin 2026-03-29 ---
		// Local 02:00 does not exist; the occurrence starts when the clock
		// jumps to 03:00 CEST == 01:00 UTC and runs two real hours.
		{"spring forward before jump", sundayDST, 1, utc(2026, 3, 29, 0, 59), false},
		{"spring forward at jump", sundayDST, 1, utc(2026, 3, 29, 1, 0), true},
		{"spring forward mid window", sundayDST, 1, utc(2026, 3, 29, 2, 30), true},
		{"spring forward after two real hours", sundayDST, 1, utc(2026, 3, 29, 3, 0), false},

		// --- DST autumn back, Europe/Berlin 2026-10-25 ---
		// Local 02:00 happens twice; the occurrence opens at the first one
		// (00:00 UTC, CEST) and runs two real hours, i.e. through the repeat.
		{"autumn back before first 02:00", sundayDST, 1, utc(2026, 10, 24, 23, 59), false},
		{"autumn back at first 02:00 CEST", sundayDST, 1, utc(2026, 10, 25, 0, 0), true},
		{"autumn back during repeated hour", sundayDST, 1, utc(2026, 10, 25, 1, 30), true},
		{"autumn back after two real hours", sundayDST, 1, utc(2026, 10, 25, 2, 0), false},
		// A normal (non-transition) Sunday still means 02:00 local.
		{"plain sunday 01:59 local", sundayDST, 1, utc(2026, 7, 18, 23, 59), false},
		{"plain sunday 02:00 local", sundayDST, 1, utc(2026, 7, 19, 0, 0), true},

		// --- multi-day lookback ---
		{"six day run on saturday", longRun, 1, utc(2026, 7, 25, 12, 0), true},
		{"six day run ended on sunday", longRun, 1, utc(2026, 7, 26, 12, 0), false},

		// --- scoping ---
		{"all monitors applies to any id", single, 12345, utc(2026, 7, 21, 0, 0), true},
		{"scoped applies to listed monitor", scoped, 7, utc(2026, 7, 21, 0, 0), true},
		{"scoped applies to other listed monitor", scoped, 9, utc(2026, 7, 21, 0, 0), true},
		{"scoped skips unlisted monitor", scoped, 8, utc(2026, 7, 21, 0, 0), false},

		// --- inactive ---
		{"inactive single during window", inactive, 1, utc(2026, 7, 21, 0, 0), false},
		{"inactive recurring during occurrence", inactiveRecurring, 1, utc(2026, 7, 20, 1, 30), false},
		{"recurring series already ended", expired, 1, utc(2026, 7, 20, 1, 30), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snap, err := NewSnapshot([]Window{tc.win})
			if err != nil {
				t.Fatalf("NewSnapshot() = %v", err)
			}
			if got := snap.InMaintenance(tc.monitor, tc.at); got != tc.want {
				local := tc.at.In(time.UTC)
				t.Fatalf("InMaintenance(%d, %s) = %v, want %v", tc.monitor, local.Format(time.RFC3339), got, tc.want)
			}
		})
	}
}

func TestSnapshotSkipsInvalidWindowsButKeepsRest(t *testing.T) {
	good := recurringWindow([]time.Weekday{time.Monday}, "02:00", 120)
	bad := recurringWindow([]time.Weekday{time.Monday}, "02:00", 120)
	bad.ID = 2
	bad.Timezone = "Mars/Olympus"

	snap, err := NewSnapshot([]Window{bad, good})
	if err == nil {
		t.Fatal("NewSnapshot() = nil error, want the invalid window reported")
	}
	if snap.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", snap.Len())
	}
	if !snap.InMaintenance(1, utc(2026, 7, 20, 1, 0)) {
		t.Fatal("valid window was dropped along with the invalid one")
	}
}

func TestSnapshotActiveAt(t *testing.T) {
	now := utc(2026, 7, 20, 1, 0)
	running := recurringWindow([]time.Weekday{time.Monday}, "02:00", 120)
	idle := recurringWindow([]time.Weekday{time.Thursday}, "02:00", 120)
	idle.ID = 2

	snap, err := NewSnapshot([]Window{running, idle})
	if err != nil {
		t.Fatalf("NewSnapshot() = %v", err)
	}
	active := snap.ActiveAt(now)
	if len(active) != 1 || active[0].ID != running.ID {
		t.Fatalf("ActiveAt() = %+v, want only window %d", active, running.ID)
	}
	// The engine and the status page must agree on what is in effect.
	if snap.InMaintenance(1, now) != (len(active) > 0) {
		t.Fatal("InMaintenance and ActiveAt disagree")
	}
}

func TestNilSnapshotIsSafe(t *testing.T) {
	var snap *Snapshot
	if snap.InMaintenance(1, time.Now()) {
		t.Fatal("nil snapshot reported maintenance")
	}
	if snap.ActiveAt(time.Now()) != nil || snap.Len() != 0 {
		t.Fatal("nil snapshot returned windows")
	}
}

// A scoped window that loses its last monitor must cover nothing, not
// everything.
//
// maintenance_monitors cascades on a monitor delete, so destroying the last
// monitor a window covered emptied its id set — and an empty set used to be the
// fleet-wide sentinel. The retention sweep does that automatically, roughly two
// weeks after somebody binned the monitor, so a window titled "planned DB
// maintenance, Sunday 02:00" silently began suppressing alerts for EVERY monitor
// and announcing itself on every public status page. Nobody performed an action
// at that moment and the window's own row was never touched.
func TestAScopedWindowThatLostItsMonitorsCoversNothing(t *testing.T) {
	start := utc(2026, 7, 20, 22, 0)
	end := utc(2026, 7, 21, 2, 0)
	inside := utc(2026, 7, 20, 23, 0)

	scoped := Window{
		ID: 1, Title: "db upgrade", Active: true, Strategy: StrategySingle,
		Timezone: "UTC", StartsAt: &start, EndsAt: &end,
		AppliesToAll: false,
		MonitorIDs:   []int64{7},
	}
	snap, err := NewSnapshot([]Window{scoped})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	if !snap.InMaintenance(7, inside) {
		t.Fatal("the scoped window does not cover its own monitor; this test proves nothing")
	}
	if snap.InMaintenance(99, inside) {
		t.Fatal("the scoped window already covers an unrelated monitor")
	}

	// The cascade fires: monitor 7 is destroyed and its row goes with it.
	cascaded := scoped
	cascaded.MonitorIDs = nil

	snap, err = NewSnapshot([]Window{cascaded})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	if snap.InMaintenance(99, inside) {
		t.Error("a window whose scope was emptied by the cascade now suppresses alerts " +
			"for an unrelated monitor — it widened to the whole fleet on its own")
	}
	// ActiveAt is deliberately a TIME query — it answers "which windows are
	// running now" and leaves scope to the caller — so the emptied window still
	// appears there. What matters is that no caller can read it as covering a
	// monitor: InMaintenance says no above, and the public page filters on
	// AppliesToAll (see statuspages_public.go).
	for _, w := range snap.ActiveAt(inside) {
		if w.AppliesToAll {
			t.Error("the emptied window reports itself as fleet-wide")
		}
	}

	// And a window that genuinely IS fleet-wide still is.
	all := scoped
	all.AppliesToAll = true
	all.MonitorIDs = nil
	snap, err = NewSnapshot([]Window{all})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	if !snap.InMaintenance(99, inside) {
		t.Error("a deliberately fleet-wide window stopped covering everything")
	}
}
