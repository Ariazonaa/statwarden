package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// heartbeatsResponse is the decoded body of the heartbeats endpoint.
type heartbeatsResponse struct {
	MonitorID  int64           `json:"monitor_id"`
	Heartbeats []HeartbeatView `json:"heartbeats"`
}

// newMonitorForHeartbeats creates a monitor and returns its id.
func newMonitorForHeartbeats(t *testing.T, h *harness, admin *session) int64 {
	t.Helper()
	var m monitorView
	mustJSON(t, h.requestWith(http.MethodPost, "/api/v1/monitors",
		monitorRequest{Name: "web", Type: "fake", Interval: 30}, admin), &m)
	return m.ID
}

// TestHeartbeatsWindowRFC3339 pins the core of the time-based feature: a
// ?from=&to= pair given as RFC3339 selects exactly the beats inside the window,
// not a fixed count.
func TestHeartbeatsWindowRFC3339(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	id := newMonitorForHeartbeats(t, h, admin)

	now := time.Now().UTC().Truncate(time.Second)
	// Inserted oldest→newest so the store's newest-first read is well defined.
	for _, d := range []time.Duration{-10 * time.Minute, -5 * time.Minute, -2 * time.Minute, -1 * time.Minute} {
		h.store.AddHeartbeat(HeartbeatView{MonitorID: id, CheckedAt: now.Add(d), Status: "up"})
	}

	from := now.Add(-6 * time.Minute)
	to := now.Add(-90 * time.Second)
	url := fmt.Sprintf("/api/v1/monitors/%d/heartbeats?from=%s&to=%s",
		id, from.Format(time.RFC3339), to.Format(time.RFC3339))

	var got heartbeatsResponse
	mustJSON(t, h.requestWith(http.MethodGet, url, nil, admin), &got)

	// Only the -5m and -2m beats fall inside [from, to].
	if len(got.Heartbeats) != 2 {
		t.Fatalf("window returned %d beats, want 2: %+v", len(got.Heartbeats), got.Heartbeats)
	}
	for _, hb := range got.Heartbeats {
		if hb.CheckedAt.Before(from) || hb.CheckedAt.After(to) {
			t.Fatalf("beat %s falls outside [%s, %s]", hb.CheckedAt, from, to)
		}
	}
	// Newest-first ordering must survive the filter.
	if !got.Heartbeats[0].CheckedAt.After(got.Heartbeats[1].CheckedAt) {
		t.Fatalf("beats not newest-first: %s then %s",
			got.Heartbeats[0].CheckedAt, got.Heartbeats[1].CheckedAt)
	}
}

// TestHeartbeatsWindowUnixSeconds proves the documented second accepted form of
// the bounds: bare unix-seconds integers select the same window as RFC3339.
func TestHeartbeatsWindowUnixSeconds(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	id := newMonitorForHeartbeats(t, h, admin)

	now := time.Now().UTC().Truncate(time.Second)
	for _, d := range []time.Duration{-10 * time.Minute, -5 * time.Minute, -2 * time.Minute, -1 * time.Minute} {
		h.store.AddHeartbeat(HeartbeatView{MonitorID: id, CheckedAt: now.Add(d), Status: "up"})
	}

	from := now.Add(-6 * time.Minute).Unix()
	to := now.Add(-90 * time.Second).Unix()
	url := fmt.Sprintf("/api/v1/monitors/%d/heartbeats?from=%d&to=%d", id, from, to)

	var got heartbeatsResponse
	mustJSON(t, h.requestWith(http.MethodGet, url, nil, admin), &got)
	if len(got.Heartbeats) != 2 {
		t.Fatalf("unix-seconds window returned %d beats, want 2: %+v", len(got.Heartbeats), got.Heartbeats)
	}
}

// TestHeartbeatsWindowOpenUpperBound covers the documented default: with a from
// but no to, the window runs up to "now", so a future-dated beat is excluded.
func TestHeartbeatsWindowOpenUpperBound(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	id := newMonitorForHeartbeats(t, h, admin)

	now := time.Now().UTC().Truncate(time.Second)
	h.store.AddHeartbeat(HeartbeatView{MonitorID: id, CheckedAt: now.Add(-1 * time.Minute), Status: "up"})
	h.store.AddHeartbeat(HeartbeatView{MonitorID: id, CheckedAt: now.Add(5 * time.Minute), Status: "up"})

	from := now.Add(-6 * time.Minute)
	url := fmt.Sprintf("/api/v1/monitors/%d/heartbeats?from=%s", id, from.Format(time.RFC3339))

	var got heartbeatsResponse
	mustJSON(t, h.requestWith(http.MethodGet, url, nil, admin), &got)
	if len(got.Heartbeats) != 1 {
		t.Fatalf("open-ended window returned %d beats, want 1 (future beat excluded): %+v",
			len(got.Heartbeats), got.Heartbeats)
	}
	if got.Heartbeats[0].CheckedAt.After(now) {
		t.Fatalf("future beat %s leaked past the implicit now bound", got.Heartbeats[0].CheckedAt)
	}
}

// TestHeartbeatsWindowInvalidBounds pins the 400 on a bound that is neither
// RFC3339 nor unix seconds — a malformed window must not silently degrade to the
// full unfiltered read.
func TestHeartbeatsWindowInvalidBounds(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	id := newMonitorForHeartbeats(t, h, admin)

	base := fmt.Sprintf("/api/v1/monitors/%d/heartbeats", id)
	if rec := h.requestWith(http.MethodGet, base+"?from=notatime", nil, admin); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid from = %d, want 400", rec.Code)
	}
	if rec := h.requestWith(http.MethodGet, base+"?to=whenever", nil, admin); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid to = %d, want 400", rec.Code)
	}
}

// TestHeartbeatsDownsample covers ?points=: a dense series is reduced to at most
// P evenly-spaced points that still pin the newest and oldest.
func TestHeartbeatsDownsample(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	id := newMonitorForHeartbeats(t, h, admin)

	now := time.Now().UTC().Truncate(time.Second)
	// 10 beats, latency == index, inserted oldest→newest.
	for i := 0; i < 10; i++ {
		lat := i
		h.store.AddHeartbeat(HeartbeatView{
			MonitorID: id,
			CheckedAt: now.Add(time.Duration(i-10) * time.Minute),
			Status:    "up",
			LatencyMs: &lat,
		})
	}

	url := fmt.Sprintf("/api/v1/monitors/%d/heartbeats?points=3", id)
	var got heartbeatsResponse
	mustJSON(t, h.requestWith(http.MethodGet, url, nil, admin), &got)

	if len(got.Heartbeats) != 3 {
		t.Fatalf("downsample to 3 returned %d beats: %+v", len(got.Heartbeats), got.Heartbeats)
	}
	// Newest (latency 9) and oldest (latency 0) endpoints must be retained.
	if got.Heartbeats[0].LatencyMs == nil || *got.Heartbeats[0].LatencyMs != 9 {
		t.Fatalf("downsample dropped the newest beat: %+v", got.Heartbeats[0])
	}
	last := got.Heartbeats[len(got.Heartbeats)-1]
	if last.LatencyMs == nil || *last.LatencyMs != 0 {
		t.Fatalf("downsample dropped the oldest beat: %+v", last)
	}
}

// TestHeartbeatsLimitBackwardCompatible guards the promise that ?limit= keeps
// its original meaning — newest N — when no window is requested.
func TestHeartbeatsLimitBackwardCompatible(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	id := newMonitorForHeartbeats(t, h, admin)

	now := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 5; i++ {
		h.store.AddHeartbeat(HeartbeatView{
			MonitorID: id,
			CheckedAt: now.Add(time.Duration(i-5) * time.Minute),
			Status:    "up",
		})
	}

	url := fmt.Sprintf("/api/v1/monitors/%d/heartbeats?limit=2", id)
	var got heartbeatsResponse
	mustJSON(t, h.requestWith(http.MethodGet, url, nil, admin), &got)
	if len(got.Heartbeats) != 2 {
		t.Fatalf("limit=2 returned %d beats, want 2", len(got.Heartbeats))
	}
	// The two returned must be the two newest.
	want := now.Add(-1 * time.Minute)
	if !got.Heartbeats[0].CheckedAt.Equal(want) {
		t.Fatalf("newest beat = %s, want %s", got.Heartbeats[0].CheckedAt, want)
	}
}

// TestHeartbeatsWindowETagRoundTrips confirms the ETag stays stable for an
// unchanged window, so a client polling a time range still gets a cheap 304.
func TestHeartbeatsWindowETagRoundTrips(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	id := newMonitorForHeartbeats(t, h, admin)

	now := time.Now().UTC().Truncate(time.Second)
	h.store.AddHeartbeat(HeartbeatView{MonitorID: id, CheckedAt: now.Add(-2 * time.Minute), Status: "up"})
	h.store.AddHeartbeat(HeartbeatView{MonitorID: id, CheckedAt: now.Add(-1 * time.Minute), Status: "up"})

	from := now.Add(-3 * time.Minute).Unix()
	to := now.Unix()
	url := fmt.Sprintf("/api/v1/monitors/%d/heartbeats?from=%d&to=%d", id, from, to)

	rec := h.requestWith(http.MethodGet, url, nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("window GET = %d", rec.Code)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("window response carried no ETag")
	}
	if rec := h.conditionalGet(url, etag, admin); rec.Code != http.StatusNotModified {
		t.Fatalf("repeat window GET with If-None-Match = %d, want 304", rec.Code)
	}
}

// TestWideWindowSpansTheWholeRange is the truncation regression.
//
// The handler used to read the newest maxWindowBeats rows and filter them by
// time afterwards, so the cap bit BEFORE the window did. On a dense monitor the
// offered "1 year" view really showed only its most recent slice — silently,
// with no indication that anything was missing. The store now selects inside
// the window and strides across it.
func TestWideWindowSpansTheWholeRange(t *testing.T) {
	h := newExtHarness(t, nil, nil)
	admin := h.setupAdmin()
	id := h.createMonitorFake(admin)

	// A year of beats, far more than any per-response cap: one every 6 hours.
	now := time.Now().UTC()
	oldest := now.AddDate(-1, 0, 0)
	for at := oldest; at.Before(now); at = at.Add(6 * time.Hour) {
		h.store.AddHeartbeat(HeartbeatView{MonitorID: id, CheckedAt: at, Status: "up"})
	}

	qs := "?from=" + oldest.Format(time.RFC3339) + "&to=" + now.Format(time.RFC3339) + "&points=50"
	rec := h.requestWith(http.MethodGet, "/api/v1/monitors/"+itoa64(id)+"/heartbeats"+qs, nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeats = %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Heartbeats []HeartbeatView `json:"heartbeats"`
	}
	mustJSON(t, rec, &body)
	if len(body.Heartbeats) == 0 {
		t.Fatal("no heartbeats returned")
	}
	if len(body.Heartbeats) > 50 {
		t.Fatalf("returned %d beats for points=50; the response is not bounded", len(body.Heartbeats))
	}
	// The decisive assertion: the OLDEST returned beat must come from near the
	// start of the window, not from its newest end.
	oldestReturned := body.Heartbeats[len(body.Heartbeats)-1].CheckedAt
	if covered := now.Sub(oldestReturned); covered < 300*24*time.Hour {
		t.Fatalf("the window covers only %v of the requested year; it was truncated to its newest end", covered)
	}
}

// A plain (non-windowed) read caps ?limit=: it had no ceiling, so any viewer
// could pull a monitor's entire retained history in one response.
func TestPlainLimitIsCapped(t *testing.T) {
	h := newExtHarness(t, nil, nil)
	admin := h.setupAdmin()
	id := h.createMonitorFake(admin)

	now := time.Now().UTC()
	for i := range maxHeartbeatLimit + 500 {
		h.store.AddHeartbeat(HeartbeatView{
			MonitorID: id, CheckedAt: now.Add(-time.Duration(i) * time.Second), Status: "up",
		})
	}
	rec := h.requestWith(http.MethodGet,
		"/api/v1/monitors/"+itoa64(id)+"/heartbeats?limit=1000000", nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeats = %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Heartbeats []HeartbeatView `json:"heartbeats"`
	}
	mustJSON(t, rec, &body)
	if len(body.Heartbeats) > maxHeartbeatLimit {
		t.Fatalf("returned %d beats for limit=1000000, want at most %d", len(body.Heartbeats), maxHeartbeatLimit)
	}
}

// TestWindowKeepsBothEnds
//
// The endpoint promises the window's newest AND oldest point; charts drawn from
// it show the range as ending wherever the data ends. The stride alone reaches
// the oldest row only when the row count happens to be a multiple of it — 200
// rows at a limit of 20 gives a stride of 10 and stops at index 190, silently
// dropping the far end of the window.
func TestWindowKeepsBothEnds(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	id := createMonitorWithConfig(t, h, admin, `{}`)

	base := time.Now().UTC().Add(-4 * time.Hour).Truncate(time.Second)
	for i := range 200 {
		h.store.AddHeartbeat(HeartbeatView{
			MonitorID: id,
			CheckedAt: base.Add(time.Duration(i) * time.Minute),
			Status:    "up",
		})
	}
	oldest := base
	newest := base.Add(199 * time.Minute)

	got, err := h.store.ListHeartbeatWindow(t.Context(), id,
		base.Add(-time.Hour), newest.Add(time.Hour), 20, false)
	if err != nil {
		t.Fatalf("ListHeartbeatWindow: %v", err)
	}
	if len(got) < 2 {
		t.Fatalf("got %d points", len(got))
	}
	// Both ends AND the cap. Keeping the oldest by simply adding it on top of
	// the stride returns limit+1 rows, which trades one broken half of the
	// contract for the other; the integration suite caught exactly that. The
	// stride is sized over the gaps between the endpoints so both hold.
	if len(got) > 20 {
		t.Fatalf("got %d points for a limit of 20", len(got))
	}
	if !got[0].CheckedAt.Equal(newest) {
		t.Fatalf("first point = %s, want the newest %s", got[0].CheckedAt, newest)
	}
	if last := got[len(got)-1].CheckedAt; !last.Equal(oldest) {
		t.Fatalf("last point = %s, want the oldest %s — the window does not "+
			"actually reach its own start", last, oldest)
	}
}

// The endpoint samples down to at most `points`, at every size and shape of
// window. A table rather than one case, because the stride arithmetic has
// boundaries — a limit of 1, a row count one under and one over the limit, and
// a count that divides evenly — and each of them is a place to be off by one.
func TestWindowRespectsItsLimitAtEveryScale(t *testing.T) {
	for _, tc := range []struct{ rows, limit int }{
		{rows: 1, limit: 1}, {rows: 1, limit: 20},
		{rows: 19, limit: 20}, {rows: 20, limit: 20}, {rows: 21, limit: 20},
		{rows: 200, limit: 20}, {rows: 200, limit: 1}, {rows: 200, limit: 2},
		{rows: 1000, limit: 7}, {rows: 100, limit: 100},
	} {
		t.Run(fmt.Sprintf("%d rows into %d points", tc.rows, tc.limit), func(t *testing.T) {
			h := newHarness(t)
			admin := h.setupAdmin()
			id := createMonitorWithConfig(t, h, admin, `{}`)
			base := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
			for i := range tc.rows {
				h.store.AddHeartbeat(HeartbeatView{
					MonitorID: id,
					CheckedAt: base.Add(time.Duration(i) * time.Minute),
					Status:    "up",
				})
			}
			got, err := h.store.ListHeartbeatWindow(t.Context(), id,
				base.Add(-time.Hour), base.Add(time.Duration(tc.rows)*time.Minute), tc.limit, false)
			if err != nil {
				t.Fatalf("ListHeartbeatWindow: %v", err)
			}
			if len(got) > tc.limit {
				t.Fatalf("got %d points for a limit of %d", len(got), tc.limit)
			}
			if len(got) == 0 {
				t.Fatal("got no points at all")
			}
			// Whenever there is room for two, the window reaches both ends.
			if tc.limit >= 2 && tc.rows >= 2 {
				newest := base.Add(time.Duration(tc.rows-1) * time.Minute)
				if !got[0].CheckedAt.Equal(newest) {
					t.Fatalf("first point = %s, want %s", got[0].CheckedAt, newest)
				}
				if last := got[len(got)-1].CheckedAt; !last.Equal(base) {
					t.Fatalf("last point = %s, want the oldest %s", last, base)
				}
			}
		})
	}
}

// eventsResponse is the decoded body of GET /monitors/events.
type eventsResponse struct {
	Items  []HeartbeatView `json:"items"`
	Total  int             `json:"total"`
	Limit  int             `json:"limit"`
	Offset int             `json:"offset"`
}

// TestEventsListTransitionsAcrossMonitors pins the instance-wide event log:
// important beats of every live monitor, newest first, ordinary beats left out,
// and a binned monitor's history hidden like everywhere else.
func TestEventsListTransitionsAcrossMonitors(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	a := newMonitorForHeartbeats(t, h, admin)
	b := newMonitorForHeartbeats(t, h, admin)
	binned := newMonitorForHeartbeats(t, h, admin)

	now := time.Now().UTC().Truncate(time.Second)
	h.store.AddHeartbeat(HeartbeatView{MonitorID: a, CheckedAt: now.Add(-30 * time.Minute), Status: "down", Important: true})
	h.store.AddHeartbeat(HeartbeatView{MonitorID: a, CheckedAt: now.Add(-29 * time.Minute), Status: "down"})
	h.store.AddHeartbeat(HeartbeatView{MonitorID: b, CheckedAt: now.Add(-20 * time.Minute), Status: "down", Important: true})
	h.store.AddHeartbeat(HeartbeatView{MonitorID: a, CheckedAt: now.Add(-10 * time.Minute), Status: "up", Important: true})
	h.store.AddHeartbeat(HeartbeatView{MonitorID: binned, CheckedAt: now.Add(-5 * time.Minute), Status: "down", Important: true})
	if rec := h.requestWith(http.MethodDelete, fmt.Sprintf("/api/v1/monitors/%d", binned), nil, admin); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}

	var got eventsResponse
	mustJSON(t, h.requestWith(http.MethodGet, "/api/v1/monitors/events", nil, admin), &got)
	if got.Total != 3 || len(got.Items) != 3 {
		t.Fatalf("got total=%d items=%d, want 3 and 3: %+v", got.Total, len(got.Items), got.Items)
	}
	want := []struct {
		id     int64
		status string
	}{{a, "up"}, {b, "down"}, {a, "down"}}
	for i, w := range want {
		if got.Items[i].MonitorID != w.id || got.Items[i].Status != w.status {
			t.Errorf("item %d = monitor %d %s, want monitor %d %s",
				i, got.Items[i].MonitorID, got.Items[i].Status, w.id, w.status)
		}
	}

	// The page window applies; the total does not shrink with it.
	var page eventsResponse
	mustJSON(t, h.requestWith(http.MethodGet, "/api/v1/monitors/events?limit=1&offset=1", nil, admin), &page)
	if page.Total != 3 || len(page.Items) != 1 || page.Items[0].MonitorID != b {
		t.Fatalf("page 2 of 1 = total %d, items %+v; want total 3 and monitor %d", page.Total, page.Items, b)
	}
}

// TestEventsListIsEmptyArrayNotNull: an instance with no transitions answers
// items: [] — the OpenAPI document declares an array, and null broke clients
// before.
func TestEventsListIsEmptyArrayNotNull(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	rec := h.requestWith(http.MethodGet, "/api/v1/monitors/events", nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("body %s does not carry an empty items array", rec.Body.String())
	}
}

// TestMonitorListCarriesRecentBeats pins the per-check bar's data: the newest
// recentBeatPoints statuses, oldest first, INCLUDING beats without a latency —
// the DOWN beats the sparkline drops are the ones the bar exists to show.
func TestMonitorListCarriesRecentBeats(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	id := newMonitorForHeartbeats(t, h, admin)

	now := time.Now().UTC().Truncate(time.Second)
	total := recentBeatPoints + 5
	for i := 0; i < total; i++ {
		hb := HeartbeatView{MonitorID: id, CheckedAt: now.Add(time.Duration(i-total) * time.Minute), Status: "up"}
		if i == total-1 {
			hb.Status = "down" // newest, and no latency
		} else {
			ms := 10
			hb.LatencyMs = &ms
		}
		h.store.AddHeartbeat(hb)
	}

	var list struct {
		Items []monitorView `json:"items"`
	}
	mustJSON(t, h.requestWith(http.MethodGet, "/api/v1/monitors", nil, admin), &list)
	if len(list.Items) != 1 {
		t.Fatalf("got %d monitors, want 1", len(list.Items))
	}
	beats := list.Items[0].RecentBeats
	if len(beats) != recentBeatPoints {
		t.Fatalf("got %d recent beats, want %d", len(beats), recentBeatPoints)
	}
	if beats[len(beats)-1] != "down" || beats[0] != "up" {
		t.Fatalf("recent beats not oldest-first ending in the latency-less down: first %q, last %q",
			beats[0], beats[len(beats)-1])
	}
}

// TestMonitorWithoutBeatsHasEmptyRecentBeats: never null on the wire.
func TestMonitorWithoutBeatsHasEmptyRecentBeats(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	newMonitorForHeartbeats(t, h, admin)
	rec := h.requestWith(http.MethodGet, "/api/v1/monitors", nil, admin)
	if !strings.Contains(rec.Body.String(), `"recent_beats":[]`) {
		t.Fatalf("body %s does not carry an empty recent_beats array", rec.Body.String())
	}
}
