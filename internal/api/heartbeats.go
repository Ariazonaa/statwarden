package api

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"
)

// defaultHeartbeatLimit caps a heartbeat read when the client gives no limit.
const defaultHeartbeatLimit = 100

// maxWindowBeats caps how many heartbeats one windowed read returns. The store
// now selects INSIDE the window and strides across it, so this is a bound on
// the response, not a hidden truncation of the range: a wide window comes back
// evenly sampled rather than cut off at its newest end.
const maxWindowBeats = 50000

// maxHeartbeatLimit caps ?limit= on a plain (non-windowed) read. It had no
// ceiling at all, so any authenticated viewer could ask for a monitor's entire
// retained history in one response and turn a GET into a large database read,
// a large allocation and a large gzip.
const maxHeartbeatLimit = 5000

// handleMonitorHeartbeats returns a monitor's recent heartbeats, newest first,
// with an ETag so clients can cheaply poll with If-None-Match.
//
// Three selectors, all optional and all backward-compatible:
//
//   - ?limit=N        newest N heartbeats (default 100). This is the original
//     behaviour and still applies when no window is given.
//   - ?from=&?to=     a real time window. Each bound accepts EITHER an RFC3339
//     timestamp (2006-01-02T15:04:05Z07:00) OR a bare
//     unix-seconds integer, whichever the caller finds
//     convenient. Either bound may be omitted: a missing from
//     means "since the beginning", a missing to means "until
//     now". Presence of either switches the read into window
//     mode, where the result is every heartbeat whose
//     checked_at falls in [from, to] (inclusive) rather than a
//     fixed count.
//   - ?points=P       downsample the result to at most P evenly-spaced points,
//     keeping the newest and oldest. Intended for charting a
//     wide window without shipping every raw heartbeat.
//
// ?important=true still narrows either mode to state transitions.
func (s *server) handleMonitorHeartbeats(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	if _, err := s.store.GetMonitor(r.Context(), id); storeError(w, err) {
		return
	}
	q := r.URL.Query()

	from, hasFrom, err := parseHeartbeatTime(q.Get("from"))
	if err != nil {
		writeProblemDetail(w, http.StatusBadRequest, "invalid from")
		return
	}
	to, hasTo, err := parseHeartbeatTime(q.Get("to"))
	if err != nil {
		writeProblemDetail(w, http.StatusBadRequest, "invalid to")
		return
	}
	windowed := hasFrom || hasTo
	// An omitted upper bound in window mode means "up to now"; an omitted lower
	// bound means "since the beginning". Filling the missing bound here keeps the
	// filter below unconditional.
	if windowed && !hasTo {
		to = s.now()
	}

	limit := defaultHeartbeatLimit
	if raw := q.Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = min(n, maxHeartbeatLimit)
		}
	}
	// ?important=true narrows the read to state transitions. That slice IS the
	// event log the monitor detail page renders; without it a client wanting
	// transitions would have to pull every heartbeat and filter client-side.
	importantOnly := q.Get("important") == "true"

	// How many samples a windowed read may return. ?points= is the caller asking
	// for a chart-sized series; without it the window is bounded by
	// maxWindowBeats. Either way the store selects inside the window and strides
	// across it, so the result spans the whole range.
	windowLimit := maxWindowBeats
	if raw := q.Get("points"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			windowLimit = min(n, maxWindowBeats)
		}
	}

	var beats []HeartbeatView
	if windowed {
		// An omitted lower bound means "since the beginning"; the store takes a
		// real range, so express that as the zero time rather than a flag.
		if !hasFrom {
			from = time.Time{}
		}
		beats, err = s.store.ListHeartbeatWindow(r.Context(), id, from, to, windowLimit, importantOnly)
	} else {
		beats, err = s.store.ListHeartbeats(r.Context(), id, limit, importantOnly)
	}
	if storeError(w, err) {
		return
	}
	// A plain read may still be thinned for a chart; a windowed one was already
	// sampled by the store.
	if !windowed {
		if raw := q.Get("points"); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 {
				beats = downsampleHeartbeats(beats, n)
			}
		}
	}

	// ETag derived from the newest heartbeat: while nothing new arrives the
	// tag is stable, so a repeat request returns 304 and saves bandwidth.
	etag := heartbeatETag(id, beats)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if match := r.Header.Get("If-None-Match"); match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"monitor_id": id, "heartbeats": beats})
}

// handleListEvents is the instance-wide event log: the state transitions
// (important heartbeats) of every live monitor, newest first, paged with the
// shared limit/offset envelope. The dashboard home renders it; without it the
// client would fan out one /monitors/{id}/heartbeats?important=true per
// monitor and merge them itself.
func (s *server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	p := parsePage(r)
	beats, total, err := s.store.ListImportantHeartbeats(r.Context(), p)
	if storeError(w, err) {
		return
	}
	writeList(w, beats, total, p)
}

// parseHeartbeatTime parses a from/to window bound. It accepts either an RFC3339
// timestamp or a bare unix-seconds integer so the frontend may send whichever is
// convenient. The bool reports whether raw was non-empty; err is non-nil only
// when a non-empty value is neither form.
func parseHeartbeatTime(raw string) (time.Time, bool, error) {
	if raw == "" {
		return time.Time{}, false, nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, true, nil
	}
	if secs, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return time.Unix(secs, 0).UTC(), true, nil
	}
	return time.Time{}, false, fmt.Errorf("time %q is neither RFC3339 nor unix seconds", raw)
}

// downsampleHeartbeats reduces beats to at most max evenly-spaced points,
// keeping the first (newest) and last (oldest) so a chart's endpoints stay
// truthful. It returns the input untouched when it already fits.
func downsampleHeartbeats(beats []HeartbeatView, max int) []HeartbeatView {
	n := len(beats)
	if max <= 0 || n <= max {
		return beats
	}
	if max == 1 {
		return beats[:1:1]
	}
	out := make([]HeartbeatView, 0, max)
	step := float64(n-1) / float64(max-1)
	prev := -1
	for i := 0; i < max; i++ {
		idx := int(math.Round(float64(i) * step))
		if idx <= prev {
			continue // guard against a repeated index at the boundaries
		}
		prev = idx
		out = append(out, beats[idx])
	}
	return out
}

// heartbeatETag hashes the count and the range endpoints into a weak-ish tag.
// Folding in both the newest and oldest checked-at keeps the tag stable while
// nothing changes yet distinguishes two different windows that happen to share a
// length and a newest beat.
func heartbeatETag(monitorID int64, beats []HeartbeatView) string {
	var newest, oldest int64
	if len(beats) > 0 {
		newest = beats[0].CheckedAt.UnixNano()
		oldest = beats[len(beats)-1].CheckedAt.UnixNano()
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%d:%d:%d", monitorID, len(beats), newest, oldest)))
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}
