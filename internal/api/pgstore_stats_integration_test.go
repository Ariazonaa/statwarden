//go:build integration

package api

import (
	"context"
	"testing"
	"time"
)

// MonitorStats is the endpoint that made the dashboard take seven seconds to
// paint, and the fix is entirely in its SQL: a narrower aggregate window for the
// list, and a LATERAL instead of a window function for the sparkline. None of
// that is observable from the Go suite, which runs against MemStore — so the
// behaviour is pinned here, against a real schema, where the queries actually
// execute.

// insertBeat records one heartbeat with an optional latency.
func insertBeat(t *testing.T, monitorID int64, at time.Time, status string, latencyMs *int32) {
	t.Helper()
	_, err := testPool.Exec(context.Background(),
		`INSERT INTO heartbeats (monitor_id, checked_at, status, latency_ms, important)
		 VALUES ($1, $2, $3, $4, false)`, monitorID, at, status, latencyMs)
	if err != nil {
		t.Fatalf("insert heartbeat: %v", err)
	}
}

func ptr32(v int32) *int32 { return &v }

// TestMonitorStatsScopeBoundsTheAggregate is the regression for the cost.
//
// StatsList must stop at thirty days: it may not report a yearly uptime, and its
// 30 d figure may not quietly include beats older than that window. Against the
// previous implementation the yearly value was always populated, so the nil
// assertion below fails there — which is the point.
func TestMonitorStatsScopeBoundsTheAggregate(t *testing.T) {
	store := newPgStore(t)
	id := insertMonitor(t, "web")
	now := time.Now().UTC()

	// Inside 24 h: two up, one down.
	insertBeat(t, id, now.Add(-1*time.Hour), "up", ptr32(100))
	insertBeat(t, id, now.Add(-2*time.Hour), "up", ptr32(110))
	insertBeat(t, id, now.Add(-3*time.Hour), "down", nil)
	// Inside 30 d but outside 24 h: one up.
	insertBeat(t, id, now.AddDate(0, 0, -10), "up", ptr32(120))
	// Outside 30 d, inside a year: two down. Only the yearly window sees these.
	insertBeat(t, id, now.AddDate(0, 0, -100), "down", nil)
	insertBeat(t, id, now.AddDate(0, 0, -200), "down", nil)

	list, err := store.MonitorStats(context.Background(), []int64{id}, now, StatsList)
	if err != nil {
		t.Fatalf("MonitorStats(StatsList): %v", err)
	}
	got := list[id]

	if got.Uptime1y != nil {
		t.Fatalf("StatsList reported uptime_1y = %v; the list view does not show it "+
			"and computing it costs a year of heartbeats per monitor", *got.Uptime1y)
	}
	// 2 up of 3 conclusive beats in the last 24 h.
	if got.Uptime24h == nil || *got.Uptime24h < 0.66 || *got.Uptime24h > 0.67 {
		t.Fatalf("StatsList uptime_24h = %v, want ~0.667", got.Uptime24h)
	}
	// 3 up of 4 conclusive beats in the last 30 d — the two beats older than
	// thirty days must NOT be counted, which is what the narrowed outer bound
	// has to get right now that the 30 d counters carry no FILTER of their own.
	if got.Uptime30d == nil || *got.Uptime30d < 0.74 || *got.Uptime30d > 0.76 {
		t.Fatalf("StatsList uptime_30d = %v, want ~0.75 (the >30 d beats leaked in)", got.Uptime30d)
	}

	detail, err := store.MonitorStats(context.Background(), []int64{id}, now, StatsDetail)
	if err != nil {
		t.Fatalf("MonitorStats(StatsDetail): %v", err)
	}
	d := detail[id]
	// The monitor page does show the year: 3 up of 6 conclusive beats.
	if d.Uptime1y == nil || *d.Uptime1y < 0.49 || *d.Uptime1y > 0.51 {
		t.Fatalf("StatsDetail uptime_1y = %v, want ~0.5", d.Uptime1y)
	}
	// The narrower windows must agree across scopes.
	if d.Uptime30d == nil || *d.Uptime30d < 0.74 || *d.Uptime30d > 0.76 {
		t.Fatalf("StatsDetail uptime_30d = %v, want the same ~0.75 the list reports", d.Uptime30d)
	}
}

// TestMonitorStatsSparklineIsCappedAndOldestFirst pins the LATERAL rewrite.
//
// The previous form ranked every beat a monitor ever recorded with a window
// function in order to keep the newest forty. This asserts the two properties
// that rewrite had to preserve — exactly sparklinePoints values, oldest first —
// so a future edit cannot trade them away for speed.
func TestMonitorStatsSparklineIsCappedAndOldestFirst(t *testing.T) {
	store := newPgStore(t)
	id := insertMonitor(t, "web")
	now := time.Now().UTC()

	// More beats than the cap, latency rising with time so the expected slice is
	// unambiguous. A few carry no latency and must be skipped entirely rather
	// than consuming a slot.
	const extra = 15
	for i := range sparklinePoints + extra {
		at := now.Add(-time.Duration(sparklinePoints+extra-i) * time.Minute)
		if i%10 == 3 {
			insertBeat(t, id, at, "down", nil)
			continue
		}
		insertBeat(t, id, at, "up", ptr32(int32(1000+i)))
	}

	stats, err := store.MonitorStats(context.Background(), []int64{id}, now, StatsList)
	if err != nil {
		t.Fatalf("MonitorStats: %v", err)
	}
	spark := stats[id].Sparkline

	if len(spark) != sparklinePoints {
		t.Fatalf("sparkline has %d points, want exactly %d", len(spark), sparklinePoints)
	}
	for i := 1; i < len(spark); i++ {
		if spark[i] <= spark[i-1] {
			t.Fatalf("sparkline is not oldest-first: %v", spark)
		}
	}
	// The newest beat's latency has to be the last element, or the LATERAL kept
	// the wrong end of the series.
	if want := 1000 + sparklinePoints + extra - 1; spark[len(spark)-1] != want {
		t.Fatalf("newest sparkline point = %d, want %d", spark[len(spark)-1], want)
	}
}

// TestMonitorStatsNewestBeatWinsPerMonitor pins the LATERAL that replaced the
// DISTINCT ON for the newest heartbeat.
//
// The property is easy to lose in a rewrite and impossible to see from
// MemStore: the row returned must be the NEWEST per monitor, and monitors must
// not borrow each other's. A LIMIT 1 in the wrong place, or a join that drops
// the correlation, produces a plausible-looking answer that is simply the wrong
// beat — and the dashboard would show a stale latency without anything failing.
func TestMonitorStatsNewestBeatWinsPerMonitor(t *testing.T) {
	store := newPgStore(t)
	now := time.Now().UTC()

	a := insertMonitor(t, "a")
	b := insertMonitor(t, "b")

	// Deliberately interleaved in time and inserted out of order, so a query
	// that leans on insertion order rather than checked_at fails here.
	insertBeat(t, a, now.Add(-2*time.Hour), "up", ptr32(200))
	insertBeat(t, b, now.Add(-1*time.Hour), "up", ptr32(700))
	insertBeat(t, a, now.Add(-10*time.Minute), "up", ptr32(111)) // a's newest
	insertBeat(t, b, now.Add(-3*time.Hour), "up", ptr32(900))
	insertBeat(t, b, now.Add(-5*time.Minute), "up", ptr32(222)) // b's newest

	stats, err := store.MonitorStats(context.Background(), []int64{a, b}, now, StatsList)
	if err != nil {
		t.Fatalf("MonitorStats: %v", err)
	}
	for id, want := range map[int64]int{a: 111, b: 222} {
		got := stats[id]
		if got.LatencyMs == nil {
			t.Fatalf("monitor %d: no latency for the newest beat", id)
		}
		if *got.LatencyMs != want {
			t.Fatalf("monitor %d: latency_ms = %d, want %d (the newest beat)", id, *got.LatencyMs, want)
		}
		if got.LastCheckedAt == nil {
			t.Fatalf("monitor %d: last_checked_at is nil", id)
		}
	}

	// A monitor with no heartbeats at all must simply be absent rather than
	// inheriting a neighbour's row — CROSS JOIN LATERAL drops it, which is the
	// behaviour the caller already handles as "no data".
	c := insertMonitor(t, "c")
	stats, err = store.MonitorStats(context.Background(), []int64{a, c}, now, StatsList)
	if err != nil {
		t.Fatalf("MonitorStats: %v", err)
	}
	if got := stats[c]; got.LastCheckedAt != nil {
		t.Fatalf("monitor without heartbeats reported last_checked_at = %v", got.LastCheckedAt)
	}
}

// TestMonitorStatsBatchesAcrossMonitors guards the property the endpoint's own
// comment claims: the decoration is batched, so the query count does not grow
// with the page. A LATERAL is per-row by nature, and it would have been easy to
// turn it into a per-monitor round trip instead.
func TestMonitorStatsBatchesAcrossMonitors(t *testing.T) {
	store := newPgStore(t)
	now := time.Now().UTC()

	ids := make([]int64, 0, 3)
	for _, name := range []string{"a", "b", "c"} {
		id := insertMonitor(t, name)
		ids = append(ids, id)
		insertBeat(t, id, now.Add(-time.Minute), "up", ptr32(50))
	}

	stats, err := store.MonitorStats(context.Background(), ids, now, StatsList)
	if err != nil {
		t.Fatalf("MonitorStats: %v", err)
	}
	if len(stats) != len(ids) {
		t.Fatalf("got stats for %d monitors, want %d", len(stats), len(ids))
	}
	for _, id := range ids {
		if got := stats[id]; got.LastCheckedAt == nil || len(got.Sparkline) != 1 {
			t.Fatalf("monitor %d: last_checked_at=%v sparkline=%v", id, got.LastCheckedAt, got.Sparkline)
		}
	}
}

// TestMonitorStatsRecentBeatsKeepLatencylessBeats pins the per-check bar's
// query: the newest recentBeatPoints statuses, oldest first, and — unlike the
// sparkline beside it — including the beats that carry no latency.
func TestMonitorStatsRecentBeatsKeepLatencylessBeats(t *testing.T) {
	store := newPgStore(t)
	id := insertMonitor(t, "web")
	now := time.Now().UTC()

	total := recentBeatPoints + 5
	for i := 0; i < total; i++ {
		at := now.Add(time.Duration(i-total) * time.Minute)
		if i == total-1 {
			insertBeat(t, id, at, "down", nil)
		} else {
			insertBeat(t, id, at, "up", ptr32(10))
		}
	}

	stats, err := store.MonitorStats(context.Background(), []int64{id}, now, StatsList)
	if err != nil {
		t.Fatalf("MonitorStats: %v", err)
	}
	beats := stats[id].RecentBeats
	if len(beats) != recentBeatPoints {
		t.Fatalf("got %d recent beats, want %d", len(beats), recentBeatPoints)
	}
	if beats[0] != "up" || beats[len(beats)-1] != "down" {
		t.Fatalf("not oldest-first ending in the latency-less down: first %q, last %q", beats[0], beats[len(beats)-1])
	}
}

// TestListImportantHeartbeatsSkipsBinnedMonitors pins the event log's two SQL
// edges the Go suite cannot see: only important rows, and none from a
// soft-deleted monitor, newest first with the total over all pages.
func TestListImportantHeartbeatsSkipsBinnedMonitors(t *testing.T) {
	store := newPgStore(t)
	ctx := context.Background()
	a := insertMonitor(t, "a")
	binned := insertMonitor(t, "binned")
	now := time.Now().UTC()

	mark := func(id int64, at time.Time, status string, important bool) {
		t.Helper()
		if _, err := testPool.Exec(ctx,
			`INSERT INTO heartbeats (monitor_id, checked_at, status, important) VALUES ($1, $2, $3, $4)`,
			id, at, status, important); err != nil {
			t.Fatalf("insert heartbeat: %v", err)
		}
	}
	mark(a, now.Add(-3*time.Minute), "down", true)
	mark(a, now.Add(-2*time.Minute), "down", false)
	mark(a, now.Add(-1*time.Minute), "up", true)
	mark(binned, now, "down", true)
	if err := store.DeleteMonitor(ctx, binned); err != nil {
		t.Fatalf("DeleteMonitor: %v", err)
	}

	got, total, err := store.ListImportantHeartbeats(ctx, Page{Limit: 1, Offset: 0})
	if err != nil {
		t.Fatalf("ListImportantHeartbeats: %v", err)
	}
	if total != 2 || len(got) != 1 || got[0].MonitorID != a || got[0].Status != "up" {
		t.Fatalf("got total %d, items %+v; want total 2 and monitor %d up first", total, got, a)
	}
}
