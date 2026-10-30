package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/report"
)

// historyNow is the instant the archive tests run at.
//
// Pinned, because the archive's last bucket is the CURRENT month and the tests
// assert on it. Reading time.Now() on both sides looks equivalent and is not: a
// run that crosses midnight on the 1st compares a payload built in one month
// against an expectation formed in the next, and the failure appears once a
// year, on a machine nobody is watching, in a test about arithmetic that did
// not change. Mid-month so nothing here is one hour from a boundary either.
var historyNow = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

// historyFixture is a published status page carrying one monitor.
func newHistoryFixture(t *testing.T, showUptime bool) (*harness, *session, StatusPage, int64) {
	t.Helper()
	h := newHarnessTweaked(t, func(o *Options) {
		o.Reporter = &fakeReporter{}
		o.Now = func() time.Time { return historyNow }
	})
	admin := h.setupAdmin()

	rec := h.requestWith(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "api", "type": "fake", "interval_seconds": 60, "config": map[string]any{},
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create monitor = %d, body %s", rec.Code, rec.Body.String())
	}
	var mon struct {
		ID int64 `json:"id"`
	}
	mustJSON(t, rec, &mon)

	rec = h.requestWith(http.MethodPost, "/api/v1/status-pages", map[string]any{
		"slug": "public", "title": "Public", "theme": "auto", "published": true,
		"monitors": []map[string]any{{"monitor_id": mon.ID, "show_uptime": showUptime}},
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status page = %d, body %s", rec.Code, rec.Body.String())
	}
	var page StatusPage
	mustJSON(t, rec, &page)
	return h, admin, page, mon.ID
}

func TestArchiveReturnsOneBucketPerMonth(t *testing.T) {
	h, _, _, _ := newHistoryFixture(t, true)

	rec := h.request(http.MethodGet, "/api/v1/public/status/public/history?months=6", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("history = %d, body %s", rec.Code, rec.Body.String())
	}
	var view PublicHistoryView
	mustJSON(t, rec, &view)

	if len(view.Months) != 6 {
		t.Fatalf("got %d months, want 6", len(view.Months))
	}
	// Oldest first, one calendar month apart, aligned to the first of the month
	// in UTC — the client renders these as column headers without re-deriving.
	for i, m := range view.Months {
		if m.Day() != 1 || m.Location() != time.UTC {
			t.Errorf("month %d is %s; buckets must start on the 1st in UTC", i, m)
		}
		if i > 0 && !view.Months[i-1].Before(m) {
			t.Errorf("months are not oldest-first: %s then %s", view.Months[i-1], m)
		}
	}
	// The final bucket is the current, still-running month.
	last := view.Months[len(view.Months)-1]
	if last.Year() != historyNow.Year() || last.Month() != historyNow.Month() {
		t.Errorf("last bucket is %s, want %s", last, historyNow.Format("2006-01"))
	}
	// And the first is five months before it, which is the assertion the
	// wall-clock version could not make at all.
	wantFirst := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if !view.Months[0].Equal(wantFirst) {
		t.Errorf("first bucket is %s, want %s", view.Months[0], wantFirst)
	}
	// Every parallel array really is parallel; the client indexes across them.
	if len(view.Overall) != len(view.Months) {
		t.Fatalf("overall has %d entries for %d months", len(view.Overall), len(view.Months))
	}
	for _, mon := range view.Monitors {
		if len(mon.Months) != len(view.Months) {
			t.Fatalf("monitor %q has %d buckets for %d months", mon.Name, len(mon.Months), len(view.Months))
		}
	}
}

// show_uptime=false is the operator saying "do not publish this monitor's
// figures". An archive that is nothing but figures has to honour it, or the
// setting is a display quirk on one page rather than a decision about what is
// public.
func TestArchiveOmitsMonitorsWithUptimeHidden(t *testing.T) {
	h, _, _, _ := newHistoryFixture(t, false)

	rec := h.request(http.MethodGet, "/api/v1/public/status/public/history", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("history = %d", rec.Code)
	}
	var view PublicHistoryView
	mustJSON(t, rec, &view)
	if len(view.Monitors) != 0 {
		t.Fatalf("a monitor with show_uptime=false was archived: %+v", view.Monitors)
	}
	// And the page-wide figure says "no data" rather than inventing one.
	for i, o := range view.Overall {
		if o != -1 {
			t.Fatalf("overall[%d] = %v with no monitors; want -1", i, o)
		}
	}
}

// The archive is a zoom level on a public page, not a statement about which
// evidence someone wants. A stray link must not become an error screen for a
// visitor who cannot fix it — so ?months= is clamped, never rejected.
func TestArchiveClampsTheWindowInsteadOfFailing(t *testing.T) {
	h, _, _, _ := newHistoryFixture(t, true)

	for _, tc := range []struct {
		query string
		want  int
	}{
		{"", defaultArchiveMonths},
		{"?months=0", defaultArchiveMonths},
		{"?months=-5", defaultArchiveMonths},
		{"?months=banana", defaultArchiveMonths},
		{"?months=3", 3},
		{"?months=99999", maxArchiveMonths},
	} {
		rec := h.request(http.MethodGet, "/api/v1/public/status/public/history"+tc.query, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("history%s = %d, body %s", tc.query, rec.Code, rec.Body.String())
		}
		var view PublicHistoryView
		mustJSON(t, rec, &view)
		if len(view.Months) != tc.want {
			t.Errorf("history%s returned %d months, want %d", tc.query, len(view.Months), tc.want)
		}
	}
}

// Without this the archive is a way around the password: the same data at a
// coarser granularity, on a route the gate forgot about.
func TestArchiveHonoursThePagePassword(t *testing.T) {
	h, admin, page, _ := newHistoryFixture(t, true)
	rec := h.requestWith(http.MethodPut, "/api/v1/status-pages/"+id64(page.ID), map[string]any{
		"slug": "public", "title": "Public", "theme": "auto", "published": true,
		"password": "hunter2hunter2",
	}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("set password = %d, body %s", rec.Code, rec.Body.String())
	}

	if rec := h.request(http.MethodGet, "/api/v1/public/status/public/history", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("archive without the password = %d, want 401", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/public/status/public/history", nil)
	req.Header.Set("X-Page-Password", "hunter2hunter2")
	viaHeader := httptest.NewRecorder()
	h.handler.ServeHTTP(viaHeader, req)
	if viaHeader.Code != http.StatusOK {
		t.Fatalf("archive with the password = %d, body %s", viaHeader.Code, viaHeader.Body.String())
	}
}

func TestArchiveIsNotServedForAnUnpublishedPage(t *testing.T) {
	h, admin, page, _ := newHistoryFixture(t, true)
	rec := h.requestWith(http.MethodPut, "/api/v1/status-pages/"+id64(page.ID), map[string]any{
		"slug": "public", "title": "Public", "theme": "auto", "published": false,
	}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("unpublish = %d, body %s", rec.Code, rec.Body.String())
	}
	if rec := h.request(http.MethodGet, "/api/v1/public/status/public/history", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("archive of an unpublished page = %d, want 404", rec.Code)
	}
}

// The archive must not reach Service.Uptime.
//
// That call walks its range one day at a time looking for rollup gaps, which is
// exactly why parseReportRange caps a report window at ~400 days. The archive
// spans up to ten years and is UNAUTHENTICATED, so one call per monitor per
// request put the capped full-scan back on the public surface. The per-row total
// is derived from the monthly buckets that were already fetched instead.
func TestArchiveDoesNotWidenTheReportFullScan(t *testing.T) {
	rep := &fakeReporter{}
	rep.uptime = report.UptimeResult{Up: 99, Down: 1, Total: 100, Uptime: 0.99}
	h := newHarnessTweaked(t, func(o *Options) { o.Reporter = rep })
	admin := h.setupAdmin()

	rec := h.requestWith(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "api", "type": "fake", "interval_seconds": 60, "config": map[string]any{},
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create monitor = %d", rec.Code)
	}
	var mon struct {
		ID int64 `json:"id"`
	}
	mustJSON(t, rec, &mon)
	rec = h.requestWith(http.MethodPost, "/api/v1/status-pages", map[string]any{
		"slug": "public", "title": "Public", "theme": "auto", "published": true,
		"monitors": []map[string]any{{"monitor_id": mon.ID, "show_uptime": true}},
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status page = %d, body %s", rec.Code, rec.Body.String())
	}

	rep.uptimeCalls = 0
	rep.uptimeSpans = nil
	rec = h.request(http.MethodGet, "/api/v1/public/status/public/history?months=120", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("archive = %d, body %s", rec.Code, rec.Body.String())
	}
	if rep.uptimeCalls != 0 {
		t.Fatalf("the archive made %d Service.Uptime call(s) over %v; the widest was the "+
			"whole archived window, which is the day-by-day scan parseReportRange caps at ~400 days",
			rep.uptimeCalls, rep.uptimeSpans)
	}

	// And the total it reports instead is real: derived from the buckets it did
	// fetch, not silently dropped.
	var view PublicHistoryView
	mustJSON(t, rec, &view)
	if len(view.Monitors) != 1 {
		t.Fatalf("got %d monitors in the archive", len(view.Monitors))
	}
	if view.Monitors[0].Uptime <= 0 {
		t.Errorf("row total = %v; it should be the mean over the months with data",
			view.Monitors[0].Uptime)
	}
}

// Deleting a monitor that sits on a published status page must not break the
// page for anonymous visitors.
//
// Deleting became a SOFT delete, and the status_page_monitors row deliberately
// survives it — so the placement outlives the monitor. The view loop skips it,
// but the assembly loop used a plain map index and appended the ZERO
// PublicMonitorView instead: id 0, empty name, and a nil heatmap that marshals
// as `null`. The public page calls .map() on that unguarded and dies, and two
// binned monitors produce two rows keyed 0 and trip Svelte's duplicate-key
// throw. The badge reads only the overall status, so a badge canary stayed green
// while the page was blank.
func TestABinnedMonitorDoesNotPoisonThePublicPage(t *testing.T) {
	h := newHarnessTweaked(t, func(o *Options) { o.Reporter = &fakeReporter{} })
	admin := h.setupAdmin()

	ids := make([]int64, 0, 3)
	for _, name := range []string{"keep", "bin-a", "bin-b"} {
		rec := h.requestWith(http.MethodPost, "/api/v1/monitors", map[string]any{
			"name": name, "type": "fake", "interval_seconds": 60, "config": map[string]any{},
		}, admin)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s = %d, body %s", name, rec.Code, rec.Body.String())
		}
		var mon struct {
			ID int64 `json:"id"`
		}
		mustJSON(t, rec, &mon)
		ids = append(ids, mon.ID)
	}

	placements := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		placements = append(placements, map[string]any{"monitor_id": id, "show_uptime": true})
	}
	rec := h.requestWith(http.MethodPost, "/api/v1/status-pages", map[string]any{
		"slug": "public", "title": "Public", "theme": "auto", "published": true,
		"monitors": placements,
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status page = %d, body %s", rec.Code, rec.Body.String())
	}

	// TWO deleted monitors: one is enough for the nil heatmap, two are what
	// collide on id 0.
	for _, id := range ids[1:] {
		if rec := h.requestWith(http.MethodDelete, "/api/v1/monitors/"+id64(id), nil, admin); rec.Code != http.StatusNoContent {
			t.Fatalf("delete %d = %d", id, rec.Code)
		}
	}

	rec = h.request(http.MethodGet, "/api/v1/public/status/public", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("public page = %d, body %s", rec.Code, rec.Body.String())
	}
	var view PublicPageView
	mustJSON(t, rec, &view)

	all := append([]PublicMonitorView{}, view.Ungrouped...)
	for _, g := range view.Groups {
		all = append(all, g.Monitors...)
	}
	if len(all) != 1 {
		t.Fatalf("page renders %d components, want only the live one: %+v", len(all), all)
	}
	if all[0].ID != ids[0] || all[0].Name != "keep" {
		t.Fatalf("the surviving component is %+v, want the live monitor", all[0])
	}
	// Every field the client dereferences must be usable, not a zero value.
	for _, m := range all {
		if m.ID == 0 {
			t.Error("a component carries id 0; two of those collide as an {#each} key")
		}
		if m.Name == "" || m.Status == "" {
			t.Errorf("a component has no name or status: %+v", m)
		}
		if m.Heatmap == nil {
			t.Error("heatmap is nil; it marshals as null and the page calls .map() on it")
		}
	}
	if strings.Contains(rec.Body.String(), `"heatmap":null`) {
		t.Errorf("the payload carries a null heatmap: %s", rec.Body.String())
	}
}

// The same guarantee without a reporter, which is DEV_MODE against the real
// server: the bars cannot be computed, but the field must still be an array.
//
// The fixture is built WITHOUT one on purpose. This test used to call
// newHistoryFixture, which wires a fakeReporter — so it always took the
// `reporter != nil` branch that overwrites Heatmap with a non-nil slice, and
// would have passed with the struct literal's `Heatmap: []PublicDayBar{}`
// deleted outright. It proved nothing about the case in its own name.
func TestHeatmapIsAnArrayEvenWithoutAReporter(t *testing.T) {
	h := newHarnessTweaked(t, func(o *Options) {
		o.Now = func() time.Time { return historyNow }
	})
	admin := h.setupAdmin()
	rec := h.requestWith(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "api", "type": "fake", "interval_seconds": 60, "config": map[string]any{},
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create monitor = %d", rec.Code)
	}
	var mon struct {
		ID int64 `json:"id"`
	}
	mustJSON(t, rec, &mon)
	rec = h.requestWith(http.MethodPost, "/api/v1/status-pages", map[string]any{
		"slug": "public", "title": "Public", "theme": "auto", "published": true,
		"monitors": []map[string]any{{"monitor_id": mon.ID, "show_uptime": true}},
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create page = %d, body %s", rec.Code, rec.Body.String())
	}
	rec = h.request(http.MethodGet, "/api/v1/public/status/public", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("public page = %d, body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"heatmap":[]`) {
		t.Fatalf("no empty heatmap in the payload, so the reporter-less branch was not taken: %s",
			rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"heatmap":null`) {
		t.Errorf("null heatmap in the payload: %s", rec.Body.String())
	}
}

// meanUptime, over the mix it actually sees.
//
// It is the archive's whole uptime column, and it was only ever exercised
// end-to-end through a fixture where every month had data — so the branch that
// matters, "skip the months with no data rather than averaging them in as
// zero", was never taken. A month with no heartbeats is not a month at 0%: an
// instance that was switched off for a year would otherwise report a decade of
// perfect service as roughly 10%.
func TestMeanUptimeIgnoresMonthsWithNoData(t *testing.T) {
	bar := func(u float64) PublicDayBar { return PublicDayBar{Uptime: u} }

	cases := []struct {
		name string
		bars []PublicDayBar
		want float64
	}{
		{"no bars at all", nil, -1},
		{"every bar empty", []PublicDayBar{bar(-1), bar(-1)}, -1},
		{"all full", []PublicDayBar{bar(1), bar(1)}, 1},
		{"empties are skipped, not counted as zero",
			[]PublicDayBar{bar(-1), bar(1), bar(-1), bar(0.5), bar(-1)}, 0.75},
		{"a genuine zero IS counted", []PublicDayBar{bar(0), bar(1)}, 0.5},
		{"a single month of data stands alone", []PublicDayBar{bar(-1), bar(0.9)}, 0.9},
	}
	for _, c := range cases {
		got := meanUptime(c.bars)
		if diff := got - c.want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("%s: meanUptime = %v, want %v", c.name, got, c.want)
		}
	}
}
