package report

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
)

var base = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func hb(offsetSec int, status domain.MonitorStatus, latencyMs int, important bool) domain.Heartbeat {
	var lat *time.Duration
	if latencyMs >= 0 {
		d := time.Duration(latencyMs) * time.Millisecond
		lat = &d
	}
	return domain.Heartbeat{
		MonitorID: 1,
		CheckedAt: base.Add(time.Duration(offsetSec) * time.Second),
		Status:    status,
		Latency:   lat,
		Important: important,
	}
}

func TestUptime(t *testing.T) {
	beats := []domain.Heartbeat{
		hb(0, domain.StatusUp, 10, false),
		hb(60, domain.StatusUp, 10, false),
		hb(120, domain.StatusDown, -1, true),
		hb(180, domain.StatusUp, 10, true),
		hb(240, domain.StatusPending, -1, false), // ignored
	}
	got := Uptime(beats, base, base.Add(time.Hour))
	if got.Up != 3 || got.Down != 1 || got.Total != 4 {
		t.Fatalf("counts = %+v", got)
	}
	if got.Uptime < 0.749 || got.Uptime > 0.751 {
		t.Fatalf("uptime = %v, want 0.75", got.Uptime)
	}
}

func TestUptimeNoData(t *testing.T) {
	got := Uptime(nil, base, base.Add(time.Hour))
	if got.Uptime != -1 || got.Total != 0 {
		t.Fatalf("empty uptime = %+v, want -1/0", got)
	}
}

func TestUptimeRangeFilter(t *testing.T) {
	beats := []domain.Heartbeat{
		hb(-100, domain.StatusDown, -1, false), // before range
		hb(10, domain.StatusUp, 5, false),
		hb(100000, domain.StatusDown, -1, false), // after range
	}
	got := Uptime(beats, base, base.Add(time.Minute))
	if got.Total != 1 || got.Up != 1 {
		t.Fatalf("range-filtered uptime = %+v", got)
	}
}

func TestLatencyPercentiles(t *testing.T) {
	var beats []domain.Heartbeat
	for i := 1; i <= 100; i++ {
		beats = append(beats, hb(i, domain.StatusUp, i, false))
	}
	p := LatencyPercentiles(beats, base, base.Add(time.Hour))
	if p.Count != 100 || p.Min != 1 || p.Max != 100 {
		t.Fatalf("percentiles = %+v", p)
	}
	if p.P50 != 50 || p.P95 != 95 || p.P99 != 99 {
		t.Fatalf("p50/p95/p99 = %d/%d/%d, want 50/95/99", p.P50, p.P95, p.P99)
	}
}

func TestLatencyPercentilesEmpty(t *testing.T) {
	p := LatencyPercentiles(nil, base, base.Add(time.Hour))
	if p.Count != 0 || p.P95 != 0 {
		t.Fatalf("empty percentiles = %+v", p)
	}
}

func TestDowntimeLog(t *testing.T) {
	beats := []domain.Heartbeat{
		hb(0, domain.StatusUp, 10, false),
		hb(60, domain.StatusDown, -1, true),
		hb(120, domain.StatusDown, -1, false),
		hb(180, domain.StatusUp, 10, true), // recovery
		hb(240, domain.StatusDown, -1, true),
	}
	log := DowntimeLog(beats, base, base.Add(time.Hour), nil)
	if len(log) != 2 {
		t.Fatalf("downtime count = %d, want 2: %+v", len(log), log)
	}
	if log[0].Duration != 120*time.Second || log[0].Ongoing {
		t.Fatalf("first outage = %+v, want 120s closed", log[0])
	}
	if !log[1].Ongoing {
		t.Fatalf("second outage should be ongoing: %+v", log[1])
	}
}

func TestAggregateAndUptimeFromRollups(t *testing.T) {
	beats := []domain.Heartbeat{
		hb(0, domain.StatusUp, 10, false),
		hb(1800, domain.StatusUp, 30, false),  // same hour
		hb(3600, domain.StatusDown, -1, true), // next hour
	}
	roll := Aggregate(1, beats, Hourly)
	if len(roll) != 2 {
		t.Fatalf("rollup buckets = %d, want 2", len(roll))
	}
	if roll[0].Up != 2 || roll[0].AvgLatency != 20 || roll[0].MinLatency != 10 || roll[0].MaxLatency != 30 {
		t.Fatalf("first bucket = %+v", roll[0])
	}
	up := UptimeFromRollups(roll, base, base.Add(2*time.Hour))
	if up.Up != 2 || up.Down != 1 {
		t.Fatalf("rollup uptime = %+v", up)
	}
}

func TestServiceReport(t *testing.T) {
	store := NewMemStore()
	store.AddMonitor(MonitorRef{ID: 1, Name: "web", Type: "http"})
	store.AddHeartbeat(hb(0, domain.StatusUp, 10, false))
	store.AddHeartbeat(hb(60, domain.StatusDown, -1, true))
	store.AddHeartbeat(hb(120, domain.StatusUp, 20, true))
	svc := NewService(store, func() time.Time { return base })

	rep, err := svc.Report(context.Background(), 1, Range{From: base, To: base.Add(time.Hour)})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if rep.Uptime.Total != 3 || len(rep.Downtime) != 1 {
		t.Fatalf("report = %+v", rep)
	}
}

func TestServiceFlakiest(t *testing.T) {
	store := NewMemStore()
	store.AddMonitor(MonitorRef{ID: 1, Name: "steady", Type: "http"})
	store.AddMonitor(MonitorRef{ID: 2, Name: "flaky", Type: "http"})
	store.AddHeartbeat(domain.Heartbeat{MonitorID: 1, CheckedAt: base, Status: domain.StatusUp})
	for i := 0; i < 5; i++ {
		st := domain.StatusUp
		if i%2 == 0 {
			st = domain.StatusDown
		}
		store.AddHeartbeat(domain.Heartbeat{MonitorID: 2, CheckedAt: base.Add(time.Duration(i) * time.Minute), Status: st, Important: true})
	}
	svc := NewService(store, func() time.Time { return base })
	rank, err := svc.Flakiest(context.Background(), Range{From: base, To: base.Add(time.Hour)}, 10)
	if err != nil {
		t.Fatalf("Flakiest: %v", err)
	}
	if len(rank) != 2 || rank[0].MonitorID != 2 {
		t.Fatalf("flakiest ranking wrong: %+v", rank)
	}
}

func TestServiceRunRollup(t *testing.T) {
	store := NewMemStore()
	store.AddMonitor(MonitorRef{ID: 1, Name: "web", Type: "http"})
	now := time.Date(2026, 1, 2, 5, 30, 0, 0, time.UTC)
	// Previous hour is 04:00-05:00.
	prevHour := time.Date(2026, 1, 2, 4, 15, 0, 0, time.UTC)
	store.AddHeartbeat(domain.Heartbeat{MonitorID: 1, CheckedAt: prevHour, Status: domain.StatusUp})
	// Previous day is Jan 1.
	prevDay := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	store.AddHeartbeat(domain.Heartbeat{MonitorID: 1, CheckedAt: prevDay, Status: domain.StatusUp})

	svc := NewService(store, func() time.Time { return now })
	if err := svc.RunRollup(context.Background()); err != nil {
		t.Fatalf("RunRollup: %v", err)
	}
	if len(store.Rollups(Hourly)) != 1 {
		t.Fatalf("hourly rollups = %d, want 1", len(store.Rollups(Hourly)))
	}
	if len(store.Rollups(Daily)) != 1 {
		t.Fatalf("daily rollups = %d, want 1", len(store.Rollups(Daily)))
	}
}

// seedUptimeWeek fills a store with one heartbeat every 10 minutes over the
// week before now, alternating a DOWN beat in every 7th slot, and returns it.
func seedUptimeWeek(now time.Time) *MemStore {
	store := NewMemStore()
	store.AddMonitor(MonitorRef{ID: 1, Name: "web", Type: "http"})
	start := now.AddDate(0, 0, -7)
	for i := 0; start.Add(time.Duration(i) * 10 * time.Minute).Before(now); i++ {
		st := domain.StatusUp
		if i%7 == 0 {
			st = domain.StatusDown
		}
		store.AddHeartbeat(domain.Heartbeat{
			MonitorID: 1,
			CheckedAt: start.Add(time.Duration(i) * 10 * time.Minute),
			Status:    st,
		})
	}
	return store
}

// rollupAllDays writes daily rollups for every whole day of the seeded week so
// Service.Uptime can take the rollup path.
func rollupAllDays(t *testing.T, store *MemStore, now time.Time) {
	t.Helper()
	for d := Daily.truncate(now.AddDate(0, 0, -7)); d.Before(Daily.truncate(now)); d = d.AddDate(0, 0, 1) {
		beats, err := store.HeartbeatsInRange(context.Background(), 1, d, d.AddDate(0, 0, 1).Add(-time.Nanosecond))
		if err != nil {
			t.Fatalf("HeartbeatsInRange: %v", err)
		}
		if err := store.UpsertRollups(context.Background(), Daily, Aggregate(1, beats, Daily)); err != nil {
			t.Fatalf("UpsertRollups: %v", err)
		}
	}
}

// TestServiceUptimeRollupMatchesRaw is the point of the rollup read path: over
// the same heartbeats it must return exactly what a raw scan returns.
func TestServiceUptimeRollupMatchesRaw(t *testing.T) {
	// A non-midnight "now" so the range has a partial head and tail day.
	now := time.Date(2026, 3, 12, 9, 37, 13, 0, time.UTC)
	store := seedUptimeWeek(now)
	rollupAllDays(t, store, now)
	svc := NewService(store, func() time.Time { return now })

	for _, tc := range []struct {
		name string
		r    Range
	}{
		{"exact week", Range{From: now.AddDate(0, 0, -7), To: now}},
		{"midnight aligned", Range{From: Daily.truncate(now.AddDate(0, 0, -6)), To: Daily.truncate(now)}},
		{"ends mid-day", Range{From: now.AddDate(0, 0, -5).Add(17 * time.Minute), To: now.Add(-3 * time.Hour)}},
		{"three days", Range{From: now.AddDate(0, 0, -3), To: now}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, ok := rollupSpan(tc.r, now); !ok {
				t.Fatalf("range %v..%v did not take the rollup path", tc.r.From, tc.r.To)
			}
			got, err := svc.Uptime(context.Background(), 1, tc.r)
			if err != nil {
				t.Fatalf("Uptime: %v", err)
			}
			beats, err := store.HeartbeatsInRange(context.Background(), 1, tc.r.From, tc.r.To)
			if err != nil {
				t.Fatalf("HeartbeatsInRange: %v", err)
			}
			want := Uptime(beats, tc.r.From, tc.r.To)
			if got != want {
				t.Fatalf("rollup path = %+v, raw path = %+v", got, want)
			}
		})
	}
}

// TestServiceUptimeShortRangeUsesRaw guards the threshold: a short range must
// not consult rollups at all.
func TestServiceUptimeShortRangeUsesRaw(t *testing.T) {
	now := time.Date(2026, 3, 12, 9, 37, 13, 0, time.UTC)
	store := seedUptimeWeek(now)
	svc := NewService(store, func() time.Time { return now })

	r := Range{From: now.Add(-6 * time.Hour), To: now}
	if _, _, ok := rollupSpan(r, now); ok {
		t.Fatalf("6h range should not take the rollup path")
	}
	got, err := svc.Uptime(context.Background(), 1, r)
	if err != nil {
		t.Fatalf("Uptime: %v", err)
	}
	beats, _ := store.HeartbeatsInRange(context.Background(), 1, r.From, r.To)
	if want := Uptime(beats, r.From, r.To); got != want {
		t.Fatalf("short-range uptime = %+v, want %+v", got, want)
	}
}

// TestServiceUptimeCurrentDayNotStale proves the trailing partial day still
// comes from raw heartbeats: beats recorded after the last rollup must show up.
func TestServiceUptimeCurrentDayNotStale(t *testing.T) {
	now := time.Date(2026, 3, 12, 9, 37, 13, 0, time.UTC)
	store := seedUptimeWeek(now)
	rollupAllDays(t, store, now)
	svc := NewService(store, func() time.Time { return now })
	r := Range{From: now.AddDate(0, 0, -7), To: now}

	before, err := svc.Uptime(context.Background(), 1, r)
	if err != nil {
		t.Fatalf("Uptime: %v", err)
	}
	// Five fresh DOWN beats in the current, not-yet-rolled-up day.
	for i := 1; i <= 5; i++ {
		store.AddHeartbeat(domain.Heartbeat{
			MonitorID: 1,
			CheckedAt: now.Add(-time.Duration(i) * time.Minute),
			Status:    domain.StatusDown,
		})
	}
	after, err := svc.Uptime(context.Background(), 1, r)
	if err != nil {
		t.Fatalf("Uptime: %v", err)
	}
	if after.Down != before.Down+5 || after.Total != before.Total+5 {
		t.Fatalf("current-day beats not picked up: before %+v, after %+v", before, after)
	}
	beats, _ := store.HeartbeatsInRange(context.Background(), 1, r.From, r.To)
	if want := Uptime(beats, r.From, r.To); after != want {
		t.Fatalf("after fresh beats = %+v, raw path = %+v", after, want)
	}
}

// TestServiceUptimeMissingRollupFallsBack covers a day the rollup job never
// wrote: it must be read raw rather than counted as no data.
func TestServiceUptimeMissingRollupFallsBack(t *testing.T) {
	now := time.Date(2026, 3, 12, 9, 37, 13, 0, time.UTC)
	store := seedUptimeWeek(now)
	// A beat exactly on each midnight, so the day that closes a gap starts with
	// a heartbeat the gap's raw span must not also claim.
	for d := Daily.truncate(now.AddDate(0, 0, -7)); !d.After(Daily.truncate(now)); d = d.AddDate(0, 0, 1) {
		store.AddHeartbeat(domain.Heartbeat{MonitorID: 1, CheckedAt: d, Status: domain.StatusUp})
	}
	// Roll up every whole day except the two in the middle of the week.
	for d := Daily.truncate(now.AddDate(0, 0, -7)); d.Before(Daily.truncate(now)); d = d.AddDate(0, 0, 1) {
		if d.Equal(Daily.truncate(now.AddDate(0, 0, -4))) || d.Equal(Daily.truncate(now.AddDate(0, 0, -3))) {
			continue
		}
		beats, _ := store.HeartbeatsInRange(context.Background(), 1, d, d.AddDate(0, 0, 1).Add(-time.Nanosecond))
		if err := store.UpsertRollups(context.Background(), Daily, Aggregate(1, beats, Daily)); err != nil {
			t.Fatalf("UpsertRollups: %v", err)
		}
	}
	svc := NewService(store, func() time.Time { return now })

	r := Range{From: now.AddDate(0, 0, -7), To: now}
	got, err := svc.Uptime(context.Background(), 1, r)
	if err != nil {
		t.Fatalf("Uptime: %v", err)
	}
	beats, _ := store.HeartbeatsInRange(context.Background(), 1, r.From, r.To)
	if want := Uptime(beats, r.From, r.To); got != want {
		t.Fatalf("with missing rollups = %+v, raw path = %+v", got, want)
	}
}

// TestServiceUptimeNoDoubleCount pins the boundary arithmetic: no heartbeat may
// be counted by both the rollup span and a raw span.
func TestServiceUptimeNoDoubleCount(t *testing.T) {
	now := time.Date(2026, 3, 12, 0, 0, 0, 0, time.UTC) // exactly a day boundary
	store := NewMemStore()
	store.AddMonitor(MonitorRef{ID: 1, Name: "web", Type: "http"})
	// One UP beat on each day boundary across the range.
	for i := 0; i <= 5; i++ {
		store.AddHeartbeat(domain.Heartbeat{
			MonitorID: 1,
			CheckedAt: now.AddDate(0, 0, -5+i),
			Status:    domain.StatusUp,
		})
	}
	svc := NewService(store, func() time.Time { return now })
	rollupAllDays(t, store, now.AddDate(0, 0, 2))

	r := Range{From: now.AddDate(0, 0, -5), To: now}
	got, err := svc.Uptime(context.Background(), 1, r)
	if err != nil {
		t.Fatalf("Uptime: %v", err)
	}
	if got.Total != 6 || got.Up != 6 {
		t.Fatalf("boundary beats counted %+v, want 6 up / 6 total", got)
	}
}

func TestServiceCertOverview(t *testing.T) {
	store := NewMemStore()
	store.AddCert(CertRow{MonitorID: 1, MonitorName: "web", ValidTo: base.Add(-24 * time.Hour)})
	store.AddCert(CertRow{MonitorID: 2, MonitorName: "api", ValidTo: base.Add(240 * time.Hour)})
	svc := NewService(store, func() time.Time { return base })
	rows, err := svc.CertOverview(context.Background())
	if err != nil {
		t.Fatalf("CertOverview: %v", err)
	}
	if !rows[0].Expired {
		t.Fatalf("first cert should be expired: %+v", rows[0])
	}
	if rows[1].Expired {
		t.Fatalf("second cert should be valid: %+v", rows[1])
	}
}

func TestWriteSLACSV(t *testing.T) {
	var buf bytes.Buffer
	rows := []SLARow{{
		MonitorID:   1,
		MonitorName: "web",
		Uptime:      UptimeResult{Up: 99, Down: 1, Total: 100, Uptime: 0.99},
		Percentiles: Percentiles{P50: 10, P95: 40, P99: 80, Avg: 15},
	}}
	if err := WriteSLACSV(&buf, Range{From: base, To: base.Add(time.Hour)}, rows); err != nil {
		t.Fatalf("WriteSLACSV: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "monitor_id,monitor_name") || !strings.Contains(out, "99.000") {
		t.Fatalf("unexpected CSV:\n%s", out)
	}
}

// WriteSLAPDF's own output is asserted in sla_pdf_test.go (magic bytes,
// trailer, well-formedness); the stub-era ErrPDFUnsupported check retired when
// the real renderer landed.

// ---------------------------------------------------------------------------
// UptimeSeries / Backfill
// ---------------------------------------------------------------------------

// countingStore counts the reads a call makes. The whole point of UptimeSeries
// is the query COUNT, so the count is the assertion.
type countingStore struct {
	Store
	rollupReads int
	rawReads    int
}

func (c *countingStore) RollupsInRange(ctx context.Context, b Bucket, id int64, from, to time.Time) ([]Rollup, error) {
	c.rollupReads++
	return c.Store.RollupsInRange(ctx, b, id, from, to)
}

func (c *countingStore) HeartbeatsInRange(ctx context.Context, id int64, from, to time.Time) ([]domain.Heartbeat, error) {
	c.rawReads++
	return c.Store.HeartbeatsInRange(ctx, id, from, to)
}

// dailyBuckets builds n contiguous day buckets ending with the day holding now.
func dailyBuckets(now time.Time, n int) []Range {
	day := Daily.truncate(now)
	out := make([]Range, 0, n)
	for i := n - 1; i >= 0; i-- {
		start := day.AddDate(0, 0, -i)
		out = append(out, Range{From: start, To: start.AddDate(0, 0, 1)})
	}
	return out
}

// The reason UptimeSeries exists: a run of buckets must cost a bounded number of
// reads, not one per bucket. The public status page renders 90 of these per
// monitor on its default range, for every monitor on the page.
func TestUptimeSeriesIsBoundedInQueries(t *testing.T) {
	now := time.Date(2026, 3, 12, 9, 37, 13, 0, time.UTC)
	inner := seedUptimeWeek(now)
	rollupAllDays(t, inner, now)
	store := &countingStore{Store: inner}
	svc := NewService(store, func() time.Time { return now })

	buckets := dailyBuckets(now, 7)
	got, err := svc.UptimeSeries(context.Background(), 1, buckets)
	if err != nil {
		t.Fatalf("UptimeSeries: %v", err)
	}
	if len(got) != len(buckets) {
		t.Fatalf("results = %d, want %d", len(got), len(buckets))
	}
	if store.rollupReads != 1 {
		t.Errorf("rollup reads = %d, want exactly 1 for the whole series", store.rollupReads)
	}
	// Only the still-open current day has no rollup yet.
	if store.rawReads > 1 {
		t.Errorf("raw reads = %d, want at most 1 (the open bucket)", store.rawReads)
	}
}

// The rollup path is an optimisation, never a different answer: every bucket
// must match counting its own heartbeats.
func TestUptimeSeriesMatchesPerBucketCounts(t *testing.T) {
	now := time.Date(2026, 3, 12, 9, 37, 13, 0, time.UTC)
	store := seedUptimeWeek(now)
	rollupAllDays(t, store, now)
	svc := NewService(store, func() time.Time { return now })

	buckets := dailyBuckets(now, 7)
	got, err := svc.UptimeSeries(context.Background(), 1, buckets)
	if err != nil {
		t.Fatalf("UptimeSeries: %v", err)
	}
	for i, b := range buckets {
		beats, _ := store.HeartbeatsInRange(context.Background(), 1, b.From, b.To)
		var up, down int
		for _, h := range beats {
			// Half-open [From, To): a boundary beat belongs to the later bucket.
			if h.CheckedAt.Before(b.From) || !h.CheckedAt.Before(b.To) {
				continue
			}
			switch h.Status {
			case domain.StatusUp:
				up++
			case domain.StatusDown:
				down++
			}
		}
		if got[i].Up != up || got[i].Down != down {
			t.Errorf("bucket %d (%s) = up %d/down %d, want up %d/down %d",
				i, b.From.Format("2006-01-02"), got[i].Up, got[i].Down, up, down)
		}
	}
}

// A heartbeat exactly on a bucket boundary must land in exactly one bucket. The
// per-bucket path this replaced used an inclusive To, so it counted twice.
func TestUptimeSeriesCountsBoundaryBeatOnce(t *testing.T) {
	now := time.Date(2026, 3, 12, 9, 37, 13, 0, time.UTC)
	store := NewMemStore()
	store.AddMonitor(MonitorRef{ID: 1, Name: "web", Type: "http"})
	store.AddHeartbeat(domain.Heartbeat{
		MonitorID: 1,
		CheckedAt: Daily.truncate(now).AddDate(0, 0, -1), // exactly midnight
		Status:    domain.StatusUp,
	})
	svc := NewService(store, func() time.Time { return now })

	got, err := svc.UptimeSeries(context.Background(), 1, dailyBuckets(now, 3))
	if err != nil {
		t.Fatalf("UptimeSeries: %v", err)
	}
	total := 0
	for _, r := range got {
		total += r.Total
	}
	if total != 1 {
		t.Fatalf("boundary beat counted %d times across buckets, want 1", total)
	}
}

// A gap the rollup job never wrote must still be answered from raw heartbeats
// rather than reported as no data — and in one read for the whole gap.
func TestUptimeSeriesFillsRollupGapFromRaw(t *testing.T) {
	now := time.Date(2026, 3, 12, 9, 37, 13, 0, time.UTC)
	inner := seedUptimeWeek(now) // heartbeats for the whole week, no rollups at all
	store := &countingStore{Store: inner}
	svc := NewService(store, func() time.Time { return now })

	got, err := svc.UptimeSeries(context.Background(), 1, dailyBuckets(now, 7))
	if err != nil {
		t.Fatalf("UptimeSeries: %v", err)
	}
	for i, r := range got {
		if r.Total == 0 {
			t.Errorf("bucket %d reported no data, but heartbeats exist for it", i)
		}
	}
	// One contiguous uncovered run means one raw read, not one per bucket.
	if store.rawReads != 1 {
		t.Errorf("raw reads = %d, want 1 for the single contiguous gap", store.rawReads)
	}
}

// Backfill must leave the rollups the periodic job would have written, so an
// instance that never ran it converges instead of scanning raw beats forever.
func TestBackfillMakesSeriesReadFromRollups(t *testing.T) {
	now := time.Date(2026, 3, 12, 9, 37, 13, 0, time.UTC)
	inner := seedUptimeWeek(now)
	if err := NewService(inner, func() time.Time { return now }).
		Backfill(context.Background(), now.AddDate(0, 0, -8)); err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if len(inner.Rollups(Daily)) == 0 {
		t.Fatal("Backfill wrote no daily rollups")
	}
	store := &countingStore{Store: inner}
	svc := NewService(store, func() time.Time { return now })
	if _, err := svc.UptimeSeries(context.Background(), 1, dailyBuckets(now, 7)); err != nil {
		t.Fatalf("UptimeSeries: %v", err)
	}
	if store.rawReads > 1 {
		t.Errorf("raw reads after backfill = %d, want at most 1 (the open bucket)", store.rawReads)
	}
}

// --- outage already in progress at the window start ---------------------------

// TestDowntimeLogReportsOutageOpenAtWindowStart covers the outage that begins
// BEFORE the reporting window.
//
// Nothing inside the window opens it — the DOWN transition happened earlier and
// a monitor that simply stays down emits no further transition — so it used to
// vanish from the report completely. A push monitor is the worst case: the
// watchdog trips it once and it then reports nothing at all, so a week-long
// outage rendered as a perfectly clean report.
func TestDowntimeLogReportsOutageOpenAtWindowStart(t *testing.T) {
	from := base.Add(time.Hour)
	to := base.Add(3 * time.Hour)
	prior := domain.Heartbeat{
		MonitorID: 1,
		CheckedAt: base, // an hour before the window opens
		Status:    domain.StatusDown,
		Message:   "no heartbeat received",
		Important: true,
	}

	// No beats at all inside the window, which is exactly the push-monitor case.
	log := DowntimeLog(nil, from, to, &prior)
	if len(log) != 1 {
		t.Fatalf("downtime entries = %d, want 1: %+v", len(log), log)
	}
	if !log[0].Ongoing {
		t.Fatal("the outage is still open at the end of the window; want Ongoing")
	}
	// Clipped to the window: a windowed report must not claim time outside it.
	if !log[0].Start.Equal(from) {
		t.Fatalf("start = %s, want the window start %s", log[0].Start, from)
	}
	if log[0].Duration != to.Sub(from) {
		t.Fatalf("duration = %s, want the whole window %s", log[0].Duration, to.Sub(from))
	}
	if log[0].Message != "no heartbeat received" {
		t.Fatalf("message = %q, want the prior beat's", log[0].Message)
	}
}

// A recovery inside the window closes an outage that started before it.
func TestDowntimeLogClosesPriorOutageOnRecovery(t *testing.T) {
	from := base.Add(time.Hour)
	to := base.Add(3 * time.Hour)
	prior := domain.Heartbeat{MonitorID: 1, CheckedAt: base, Status: domain.StatusDown}
	beats := []domain.Heartbeat{
		{MonitorID: 1, CheckedAt: from.Add(30 * time.Minute), Status: domain.StatusUp},
	}
	log := DowntimeLog(beats, from, to, &prior)
	if len(log) != 1 {
		t.Fatalf("downtime entries = %d, want 1: %+v", len(log), log)
	}
	if log[0].Ongoing {
		t.Fatal("the outage was closed by the UP beat; want Ongoing false")
	}
	if want := 30 * time.Minute; log[0].Duration != want {
		t.Fatalf("duration = %s, want %s", log[0].Duration, want)
	}
}

// A monitor that was UP entering the window must not gain a phantom outage.
func TestDowntimeLogIgnoresPriorUpBeat(t *testing.T) {
	from := base.Add(time.Hour)
	prior := domain.Heartbeat{MonitorID: 1, CheckedAt: base, Status: domain.StatusUp}
	if log := DowntimeLog(nil, from, from.Add(time.Hour), &prior); len(log) != 0 {
		t.Fatalf("downtime entries = %d, want 0: %+v", len(log), log)
	}
}
