package report

import (
	"context"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
)

// MonitorRef is the minimal monitor identity the report layer needs.
type MonitorRef struct {
	ID   int64
	Name string
	Type string
}

// CertRow is one certificate in the expiry overview.
type CertRow struct {
	MonitorID     int64     `json:"monitor_id"`
	MonitorName   string    `json:"monitor_name"`
	Subject       string    `json:"subject,omitempty"`
	Issuer        string    `json:"issuer,omitempty"`
	ValidTo       time.Time `json:"valid_to"`
	DaysRemaining int       `json:"days_remaining"`
	Expired       bool      `json:"expired"`
}

// Store is the report layer's read/write surface. PgStore implements it against
// PostgreSQL; MemStore backs tests.
type Store interface {
	// HeartbeatsInRange returns a monitor's heartbeats within [from, to].
	HeartbeatsInRange(ctx context.Context, monitorID int64, from, to time.Time) ([]domain.Heartbeat, error)
	// LastHeartbeatBefore returns the newest heartbeat strictly before at, so a
	// report can tell whether an outage was ALREADY in progress when its window
	// opened. Reports false when the monitor has no heartbeat before at.
	LastHeartbeatBefore(ctx context.Context, monitorID int64, at time.Time) (domain.Heartbeat, bool, error)
	// ListMonitors returns every monitor's identity.
	ListMonitors(ctx context.Context) ([]MonitorRef, error)
	// CertOverview returns every stored certificate joined to its monitor.
	CertOverview(ctx context.Context) ([]CertRow, error)
	// UpsertRollups writes aggregated buckets (idempotent per monitor+bucket).
	UpsertRollups(ctx context.Context, bucket Bucket, rollups []Rollup) error
	// RollupsInRange returns a monitor's rollups whose bucket start falls
	// within [from, to], ascending by bucket start.
	RollupsInRange(ctx context.Context, bucket Bucket, monitorID int64, from, to time.Time) ([]Rollup, error)
}

// rollupThreshold is the range length from which Uptime prefers pre-aggregated
// daily rollups over a full raw-heartbeat scan. Shorter ranges stay on raw
// heartbeats, where the scan is cheap and no bucket is ever partial.
const rollupThreshold = 48 * time.Hour

// Service orchestrates the report computations over a Store.
type Service struct {
	store Store
	now   func() time.Time
}

// NewService builds a Service. now defaults to time.Now.
func NewService(store Store, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{store: store, now: now}
}

// Uptime returns availability for one monitor over a range. Long ranges are
// answered from the daily rollups for the whole days they cover, with the
// leading and trailing partial days — and any day the rollup job has not
// written yet — still read from raw heartbeats. Both paths count the same
// heartbeats, so the result does not depend on which one ran.
func (s *Service) Uptime(ctx context.Context, monitorID int64, r Range) (UptimeResult, error) {
	from, to, ok := rollupSpan(r, s.now())
	if !ok {
		beats, err := s.store.HeartbeatsInRange(ctx, monitorID, r.From, r.To)
		if err != nil {
			return UptimeResult{}, err
		}
		return Uptime(beats, r.From, r.To), nil
	}

	// Buckets start at from and the last whole day starts one day before to.
	lastBucket := to.AddDate(0, 0, -1)
	rollups, err := s.store.RollupsInRange(ctx, Daily, monitorID, from, lastBucket)
	if err != nil {
		return UptimeResult{}, err
	}
	res := addUptime(UptimeResult{From: r.From, To: r.To, Uptime: -1}, UptimeFromRollups(rollups, from, lastBucket))

	// Partial day at the head of the range.
	if from.After(r.From) {
		if err := s.addRawSpan(ctx, monitorID, r.From, from.Add(-time.Nanosecond), &res); err != nil {
			return UptimeResult{}, err
		}
	}
	// Partial (current) day at the tail: never rolled up yet, always raw.
	if err := s.addRawSpan(ctx, monitorID, to, r.To, &res); err != nil {
		return UptimeResult{}, err
	}
	// Whole days the rollup job has not written yet would otherwise be counted
	// as zero heartbeats and silently understate uptime; read those raw.
	present := make(map[time.Time]bool, len(rollups))
	for _, rl := range rollups {
		present[rl.BucketAt.UTC()] = true
	}
	var gap time.Time
	for d := from; d.Before(to); d = d.AddDate(0, 0, 1) {
		if !present[d] {
			if gap.IsZero() {
				gap = d
			}
			continue
		}
		if !gap.IsZero() {
			if err := s.addRawSpan(ctx, monitorID, gap, d.Add(-time.Nanosecond), &res); err != nil {
				return UptimeResult{}, err
			}
			gap = time.Time{}
		}
	}
	if !gap.IsZero() {
		if err := s.addRawSpan(ctx, monitorID, gap, to.Add(-time.Nanosecond), &res); err != nil {
			return UptimeResult{}, err
		}
	}
	return res, nil
}

// rollupSpan returns the day-aligned window [from, to) of r that the daily
// rollups can answer. Only days that ended before now are complete, so to never
// runs past the start of the current day. ok is false when the range is too
// short to be worth the rollup path or contains no whole rolled-up day.
func rollupSpan(r Range, now time.Time) (time.Time, time.Time, bool) {
	if r.To.Sub(r.From) < rollupThreshold {
		return time.Time{}, time.Time{}, false
	}
	from := Daily.truncate(r.From)
	if from.Before(r.From) {
		from = from.AddDate(0, 0, 1)
	}
	to := Daily.truncate(r.To)
	if complete := Daily.truncate(now); to.After(complete) {
		to = complete
	}
	if !to.After(from) {
		return time.Time{}, time.Time{}, false
	}
	return from, to, true
}

// addRawSpan folds the raw-heartbeat uptime of [from, to] into res.
func (s *Service) addRawSpan(ctx context.Context, monitorID int64, from, to time.Time, res *UptimeResult) error {
	beats, err := s.store.HeartbeatsInRange(ctx, monitorID, from, to)
	if err != nil {
		return err
	}
	*res = addUptime(*res, Uptime(beats, from, to))
	return nil
}

// addUptime merges b's counts into a, keeping a's window and recomputing the
// up fraction.
func addUptime(a, b UptimeResult) UptimeResult {
	a.Up += b.Up
	a.Down += b.Down
	a.Total = a.Up + a.Down
	if a.Total > 0 {
		a.Uptime = float64(a.Up) / float64(a.Total)
	}
	return a
}

// UptimeSeries computes uptime for a whole ordered list of contiguous buckets
// with a BOUNDED number of store reads — one rollup query for the series plus
// one raw query per contiguous run of buckets the rollup job has not written —
// instead of one query per bucket.
//
// The per-bucket alternative is what the public status page used to do, and it
// does not scale with the thing it renders: 90 daily bars (the default range) or
// 168 hourly ones, times every monitor on the page, on a public unauthenticated
// endpoint. A page with 30 monitors issued thousands of sequential queries per
// view.
//
// Buckets are half-open [From, To), so a heartbeat exactly on a boundary counts
// once, in the later bucket — the per-bucket path counted it in both.
func (s *Service) UptimeSeries(ctx context.Context, monitorID int64, buckets []Range) ([]UptimeResult, error) {
	out := make([]UptimeResult, len(buckets))
	for i, b := range buckets {
		out[i] = UptimeResult{From: b.From, To: b.To, Uptime: -1}
	}
	if len(buckets) == 0 {
		return out, nil
	}

	// Bucket width picks the rollup granularity: hourly bars read hourly
	// rollups, daily and monthly bars read daily ones.
	gran := Daily
	if buckets[0].To.Sub(buckets[0].From) <= time.Hour {
		gran = Hourly
	}
	rollups, err := s.store.RollupsInRange(ctx, gran, monitorID, buckets[0].From, buckets[len(buckets)-1].To)
	if err != nil {
		return nil, err
	}

	// Fold each rollup into its bucket. Both are ascending, so one pass with an
	// advancing cursor covers the whole series.
	covered := make([]bool, len(buckets))
	cur := 0
	for _, r := range rollups {
		at := r.BucketAt.UTC()
		for cur < len(buckets) && !at.Before(buckets[cur].To) {
			cur++
		}
		if cur >= len(buckets) || at.Before(buckets[cur].From) {
			continue
		}
		out[cur].Up += r.Up
		out[cur].Down += r.Down
		covered[cur] = true
	}

	// The bucket in progress is never rolled up yet, and a stretch the rollup job
	// missed has no rows either. Both are answered from raw heartbeats, one query
	// per contiguous run rather than one per bucket.
	openFrom := gran.truncate(s.now())
	needsRaw := func(i int) bool { return !covered[i] || !buckets[i].To.Before(openFrom) }
	for i := 0; i < len(buckets); i++ {
		if !needsRaw(i) {
			continue
		}
		j := i
		for j+1 < len(buckets) && needsRaw(j+1) {
			j++
		}
		beats, err := s.store.HeartbeatsInRange(ctx, monitorID, buckets[i].From, buckets[j].To)
		if err != nil {
			return nil, err
		}
		for k := i; k <= j; k++ {
			// Raw supersedes anything a partial rollup contributed, so the open
			// bucket is never counted twice.
			out[k].Up, out[k].Down = 0, 0
			for _, hb := range beats {
				if hb.CheckedAt.Before(buckets[k].From) || !hb.CheckedAt.Before(buckets[k].To) {
					continue
				}
				switch hb.Status {
				case domain.StatusUp:
					out[k].Up++
				case domain.StatusDown:
					out[k].Down++
				}
			}
		}
		i = j
	}

	for i := range out {
		out[i].Total = out[i].Up + out[i].Down
		if out[i].Total > 0 {
			out[i].Uptime = float64(out[i].Up) / float64(out[i].Total)
		}
	}
	return out, nil
}

// Backfill rebuilds hourly and daily rollups for [since, now] from raw
// heartbeats, one read per monitor for the whole window.
//
// RunRollup only ever writes the bucket that just closed, so history that
// predates the job — an upgrade, or a spell where it was down — has no rollups
// at all, and every long-range read falls back to scanning raw heartbeats
// forever. UpsertRollups is idempotent, so re-running this is free and safe.
func (s *Service) Backfill(ctx context.Context, since time.Time) error {
	monitors, err := s.store.ListMonitors(ctx)
	if err != nil {
		return err
	}
	now := s.now()
	// The window is snapped FORWARD to a daily boundary.
	//
	// `since` is an arbitrary instant — in production it is whatever time the
	// process happened to start — so the day bucket containing it holds only the
	// beats after it. Aggregate re-buckets by timestamp and UpsertRollups is a
	// full replace, so that partial count overwrote a rollup that had been
	// written correctly months earlier. A restart at 09:37 with a 180-day window
	// replaced day -180's 1440 beats (120 of them DOWN) with the 578 from 09:37
	// onward, all up, and the day then read as 100.000% instead of 91.667%. Every
	// restart corrupted a different day.
	//
	// Starting at the next boundary loses nothing: the bucket before it is
	// either already complete or outside the retention window entirely.
	// Guarded on whether TRUNCATION actually moved it, which is the question.
	//
	// Comparing against now() instead was true for essentially any `since` a
	// backfill is called with, so the snap was "truncate then always add a day".
	// For a `since` that is ALREADY midnight that skips a full day which genuinely
	// needed rebuilding, and for a `since` inside the current day it can push the
	// start past `now`, making the whole backfill a silent no-op. This is the
	// same idiom rollupSpan uses.
	if snapped := Daily.truncate(since); snapped.Before(since) {
		since = snapped.AddDate(0, 0, 1)
	} else {
		since = snapped
	}
	for _, m := range monitors {
		beats, err := s.store.HeartbeatsInRange(ctx, m.ID, since, now)
		if err != nil {
			return err
		}
		if len(beats) == 0 {
			continue
		}
		if err := s.store.UpsertRollups(ctx, Hourly, Aggregate(m.ID, beats, Hourly)); err != nil {
			return err
		}
		if err := s.store.UpsertRollups(ctx, Daily, Aggregate(m.ID, beats, Daily)); err != nil {
			return err
		}
	}
	return nil
}

// FullReport bundles the per-monitor report: uptime, latency percentiles and
// the downtime log for a range.
type FullReport struct {
	MonitorID   int64        `json:"monitor_id"`
	Range       Range        `json:"-"`
	Uptime      UptimeResult `json:"uptime"`
	Percentiles Percentiles  `json:"latency"`
	Downtime    []Downtime   `json:"downtime"`
}

// Report computes uptime, latency percentiles and the downtime log for one
// monitor over a range.
func (s *Service) Report(ctx context.Context, monitorID int64, r Range) (FullReport, error) {
	beats, err := s.store.HeartbeatsInRange(ctx, monitorID, r.From, r.To)
	if err != nil {
		return FullReport{}, err
	}
	// The state entering the window decides whether an outage that started
	// before it is reported at all — see DowntimeLog.
	var prior *domain.Heartbeat
	if hb, ok, perr := s.store.LastHeartbeatBefore(ctx, monitorID, r.From); perr != nil {
		return FullReport{}, perr
	} else if ok {
		prior = &hb
	}
	return FullReport{
		MonitorID:   monitorID,
		Range:       r,
		Uptime:      Uptime(beats, r.From, r.To),
		Percentiles: LatencyPercentiles(beats, r.From, r.To),
		Downtime:    DowntimeLog(beats, r.From, r.To, prior),
	}, nil
}

// Flakiest ranks monitors by state-transition count over a range. limit <= 0
// returns all monitors.
func (s *Service) Flakiest(ctx context.Context, r Range, limit int) ([]Flakiness, error) {
	monitors, err := s.store.ListMonitors(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]Flakiness, 0, len(monitors))
	for _, m := range monitors {
		beats, err := s.store.HeartbeatsInRange(ctx, m.ID, r.From, r.To)
		if err != nil {
			return nil, err
		}
		up := Uptime(beats, r.From, r.To)
		items = append(items, Flakiness{
			MonitorID:   m.ID,
			MonitorName: m.Name,
			Transitions: CountTransitions(beats, r.From, r.To),
			Uptime:      up.Uptime,
		})
	}
	return RankFlakiest(items, limit), nil
}

// CertOverview returns the certificate-expiry overview, sorted by soonest
// expiry first.
func (s *Service) CertOverview(ctx context.Context) ([]CertRow, error) {
	rows, err := s.store.CertOverview(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	for i := range rows {
		rows[i].Expired = !rows[i].ValidTo.IsZero() && rows[i].ValidTo.Before(now)
	}
	return rows, nil
}

// RunRollup aggregates the raw heartbeats of the just-completed hour and day
// into the hourly and daily rollup tables for every monitor. It is safe to run
// repeatedly (UpsertRollups is idempotent).
func (s *Service) RunRollup(ctx context.Context) error {
	now := s.now()
	hourFrom := Hourly.truncate(now.Add(-time.Hour))
	hourTo := hourFrom.Add(time.Hour)
	dayFrom := Daily.truncate(now.AddDate(0, 0, -1))
	dayTo := dayFrom.AddDate(0, 0, 1)

	monitors, err := s.store.ListMonitors(ctx)
	if err != nil {
		return err
	}
	var hourly, daily []Rollup
	for _, m := range monitors {
		hb, err := s.store.HeartbeatsInRange(ctx, m.ID, hourFrom, hourTo)
		if err != nil {
			return err
		}
		hourly = append(hourly, Aggregate(m.ID, hb, Hourly)...)

		db, err := s.store.HeartbeatsInRange(ctx, m.ID, dayFrom, dayTo)
		if err != nil {
			return err
		}
		daily = append(daily, Aggregate(m.ID, db, Daily)...)
	}
	if err := s.store.UpsertRollups(ctx, Hourly, hourly); err != nil {
		return err
	}
	return s.store.UpsertRollups(ctx, Daily, daily)
}
