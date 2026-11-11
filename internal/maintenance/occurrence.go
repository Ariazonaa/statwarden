package maintenance

import "time"

// NextOccurrence returns the occurrence of w that is in effect at at, or else
// the next one to start after it. ok is false when the window has none left:
// it is inactive, a one-off that has ended, or a series whose bounds close
// before another occurrence begins.
//
// It exists for the public status page, which has to say WHEN a window runs.
// A recurring series has no single start and end — its starts_at/ends_at are
// the series bounds, often absent — so announcing the stored row told a
// visitor "maintenance scheduled" with no time at all, forever, even after the
// series had no occurrence left before its end.
//
// An occurrence is clipped to the series bounds exactly as covers clips the
// instant, so "running now" by this function and "in effect" by the evaluator
// are the same answer.
func NextOccurrence(w Window, at time.Time) (start, end time.Time, ok bool, err error) {
	c, err := compile(w)
	if err != nil {
		return time.Time{}, time.Time{}, false, err
	}
	if !w.Active {
		return time.Time{}, time.Time{}, false, nil
	}
	switch w.Strategy {
	case StrategySingle:
		if !at.Before(*w.EndsAt) {
			return time.Time{}, time.Time{}, false, nil
		}
		return *w.StartsAt, *w.EndsAt, true, nil
	case StrategyRecurring:
		// A series that has not begun yet is searched from its first instant;
		// before that, nothing can be in effect.
		from := at
		if w.StartsAt != nil && w.StartsAt.After(from) {
			from = *w.StartsAt
		}
		local := from.In(c.loc)
		// From lookback days back (an occurrence that started then may still be
		// running) to a week ahead (every listed weekday recurs within seven
		// days), oldest first, so the first match is the earliest.
		for day := -c.lookback; day <= 7; day++ {
			y, m, d := local.AddDate(0, 0, day).Date()
			if !c.weekdays[time.Date(y, m, d, 12, 0, 0, 0, c.loc).Weekday()] {
				continue
			}
			s := c.startOn(y, m, d)
			e := s.Add(c.dur)
			if w.StartsAt != nil && s.Before(*w.StartsAt) {
				s = *w.StartsAt
			}
			if w.EndsAt != nil && e.After(*w.EndsAt) {
				e = *w.EndsAt
			}
			if !e.After(s) || !e.After(at) {
				continue // clipped away by the bounds, or already over
			}
			return s, e, true, nil
		}
	}
	return time.Time{}, time.Time{}, false, nil
}
