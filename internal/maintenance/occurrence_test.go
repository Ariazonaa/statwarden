package maintenance

import (
	"testing"
	"time"
)

func TestNextOccurrence(t *testing.T) {
	// 2026-03-11 is a Wednesday.
	wed := utc(2026, 3, 11, 12, 0)

	t.Run("single window ahead and running", func(t *testing.T) {
		w := Window{
			ID: 1, Active: true, Strategy: StrategySingle, Timezone: "UTC", AppliesToAll: true,
			StartsAt: ptr(utc(2026, 3, 12, 2, 0)), EndsAt: ptr(utc(2026, 3, 12, 4, 0)),
		}
		for _, at := range []time.Time{wed, utc(2026, 3, 12, 3, 0)} {
			s, e, ok, err := NextOccurrence(w, at)
			if err != nil || !ok || !s.Equal(*w.StartsAt) || !e.Equal(*w.EndsAt) {
				t.Errorf("at %v: got %v–%v ok=%v err=%v, want the window itself", at, s, e, ok, err)
			}
		}
		if _, _, ok, _ := NextOccurrence(w, utc(2026, 3, 12, 4, 0)); ok {
			t.Error("a finished one-off still has an occurrence")
		}
	})

	t.Run("recurring picks the next concrete occurrence", func(t *testing.T) {
		// Mondays and Fridays 02:00 UTC for two hours.
		w := recurringWindow([]time.Weekday{time.Monday, time.Friday}, "02:00", 120)
		w.Timezone = "UTC"
		s, e, ok, err := NextOccurrence(w, wed)
		if err != nil || !ok {
			t.Fatalf("ok=%v err=%v, want the Friday occurrence", ok, err)
		}
		if want := utc(2026, 3, 13, 2, 0); !s.Equal(want) || !e.Equal(want.Add(2*time.Hour)) {
			t.Errorf("got %v–%v, want %v–%v", s, e, want, want.Add(2*time.Hour))
		}
	})

	t.Run("recurring reports the occurrence in effect, across midnight", func(t *testing.T) {
		// Tuesdays 23:00 for three hours, so it is still running at 01:00 Wednesday.
		w := recurringWindow([]time.Weekday{time.Tuesday}, "23:00", 180)
		w.Timezone = "UTC"
		at := utc(2026, 3, 11, 1, 0)
		s, e, ok, err := NextOccurrence(w, at)
		if err != nil || !ok || !s.Equal(utc(2026, 3, 10, 23, 0)) || !e.Equal(utc(2026, 3, 11, 2, 0)) {
			t.Errorf("got %v–%v ok=%v err=%v, want the running Tuesday occurrence", s, e, ok, err)
		}
	})

	t.Run("recurring honours the local timezone", func(t *testing.T) {
		// 02:00 Berlin on a Friday in March (CET, UTC+1) is 01:00 UTC.
		w := recurringWindow([]time.Weekday{time.Friday}, "02:00", 60)
		s, _, ok, err := NextOccurrence(w, wed)
		if err != nil || !ok || !s.Equal(utc(2026, 3, 13, 1, 0)) {
			t.Errorf("got %v ok=%v err=%v, want 2026-03-13 01:00 UTC", s, ok, err)
		}
	})

	t.Run("a series that ends before its next occurrence has none", func(t *testing.T) {
		w := recurringWindow([]time.Weekday{time.Monday}, "02:00", 60)
		w.Timezone = "UTC"
		w.EndsAt = ptr(utc(2026, 3, 14, 0, 0)) // Saturday, before the next Monday
		if s, _, ok, _ := NextOccurrence(w, wed); ok {
			t.Errorf("got an occurrence at %v after the series ended", s)
		}
	})

	t.Run("a series that has not started yet begins at its first occurrence", func(t *testing.T) {
		w := recurringWindow([]time.Weekday{time.Monday}, "02:00", 60)
		w.Timezone = "UTC"
		w.StartsAt = ptr(utc(2026, 4, 1, 0, 0)) // a Wednesday, three weeks out
		s, _, ok, err := NextOccurrence(w, wed)
		if err != nil || !ok || !s.Equal(utc(2026, 4, 6, 2, 0)) {
			t.Errorf("got %v ok=%v err=%v, want the first Monday after the series starts", s, ok, err)
		}
	})

	t.Run("an inactive window has no occurrence", func(t *testing.T) {
		w := recurringWindow([]time.Weekday{time.Wednesday}, "00:00", 1440)
		w.Active = false
		if _, _, ok, _ := NextOccurrence(w, wed); ok {
			t.Error("an inactive window reported an occurrence")
		}
	})

	t.Run("agrees with the evaluator on what is in effect", func(t *testing.T) {
		w := recurringWindow([]time.Weekday{time.Sunday, time.Wednesday}, "11:30", 90)
		c, err := compile(w)
		if err != nil {
			t.Fatal(err)
		}
		for at := utc(2026, 3, 8, 0, 0); at.Before(utc(2026, 3, 16, 0, 0)); at = at.Add(15 * time.Minute) {
			s, e, ok, err := NextOccurrence(w, at)
			if err != nil {
				t.Fatal(err)
			}
			running := ok && !at.Before(s) && at.Before(e)
			if running != c.covers(at) {
				t.Fatalf("at %v: NextOccurrence says running=%v, evaluator says %v", at, running, c.covers(at))
			}
		}
	})
}
