package notify

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// QuietHours is a daily do-not-disturb window during which the dispatcher
// does not deliver alerts (deliveries are logged as skipped; digest buffers
// are held until the window ends). Start and End are wall-clock times in
// "HH:MM" 24-hour format, evaluated in Location. A window whose end is before
// its start spans midnight (e.g. 22:00 → 07:00).
type QuietHours struct {
	Start string
	End   string
	// Location defaults to time.UTC.
	Location *time.Location
}

// Validate checks the window definition. An identical start and end is
// rejected — use a nil *QuietHours to disable quiet hours.
func (q QuietHours) Validate() error {
	start, err := parseWallClock(q.Start)
	if err != nil {
		return fmt.Errorf("quiet hours: start: %w", err)
	}
	end, err := parseWallClock(q.End)
	if err != nil {
		return fmt.Errorf("quiet hours: end: %w", err)
	}
	if start == end {
		return fmt.Errorf("quiet hours: start and end are both %s (empty window)", q.Start)
	}
	return nil
}

// Contains reports whether t falls inside the quiet window. The start minute
// is inclusive, the end minute exclusive.
func (q QuietHours) Contains(t time.Time) bool {
	start, err1 := parseWallClock(q.Start)
	end, err2 := parseWallClock(q.End)
	if err1 != nil || err2 != nil || start == end {
		return false // invalid windows never suppress (fail open + Validate at setup)
	}
	loc := q.Location
	if loc == nil {
		loc = time.UTC
	}
	local := t.In(loc)
	minute := local.Hour()*60 + local.Minute()
	if start < end {
		return minute >= start && minute < end
	}
	// Window spans midnight.
	return minute >= start || minute < end
}

// parseWallClock parses "HH:MM" into minutes since midnight.
func parseWallClock(s string) (int, error) {
	hh, mm, ok := strings.Cut(s, ":")
	if !ok {
		return 0, fmt.Errorf("%q is not a HH:MM time", s)
	}
	h, err1 := strconv.Atoi(hh)
	m, err2 := strconv.Atoi(mm)
	if err1 != nil || err2 != nil {
		return 0, fmt.Errorf("%q is not a HH:MM time", s)
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("%q is out of range", s)
	}
	return h*60 + m, nil
}
