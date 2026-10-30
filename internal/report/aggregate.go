// Package report computes uptime, latency percentiles, downtime logs, monitor
// flakiness rankings and certificate-expiry overviews over recorded
// heartbeats. The heavy lifting is pure functions over heartbeat slices so the
// maths is fully unit-tested without a live database; a Service wires those to
// a small Store interface, and a rollup job pre-aggregates raw heartbeats into
// hourly/daily buckets so long-range queries stay fast.
package report

import (
	"math"
	"sort"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
)

// Range is a named time window used by the report endpoints.
type Range struct {
	From time.Time
	To   time.Time
}

// UptimeResult is availability over a window. Uptime is the up fraction of all
// conclusive (up or down) heartbeats; pending/maintenance beats are ignored.
type UptimeResult struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Up     int       `json:"up"`
	Down   int       `json:"down"`
	Total  int       `json:"total"`
	Uptime float64   `json:"uptime"` // 0..1; -1 when there is no data
}

// Uptime computes availability over [from, to] from the given heartbeats.
// Heartbeats outside the range are ignored.
func Uptime(beats []domain.Heartbeat, from, to time.Time) UptimeResult {
	res := UptimeResult{From: from, To: to, Uptime: -1}
	for _, hb := range beats {
		if hb.CheckedAt.Before(from) || hb.CheckedAt.After(to) {
			continue
		}
		switch hb.Status {
		case domain.StatusUp:
			res.Up++
		case domain.StatusDown:
			res.Down++
		}
	}
	res.Total = res.Up + res.Down
	if res.Total > 0 {
		res.Uptime = float64(res.Up) / float64(res.Total)
	}
	return res
}

// Percentiles holds latency percentiles in milliseconds.
type Percentiles struct {
	P50   int `json:"p50"`
	P95   int `json:"p95"`
	P99   int `json:"p99"`
	Min   int `json:"min"`
	Max   int `json:"max"`
	Avg   int `json:"avg"`
	Count int `json:"count"`
}

// LatencyPercentiles computes p50/p95/p99 (and min/max/avg) over the latency of
// the up heartbeats in the range. Heartbeats without a latency are ignored.
func LatencyPercentiles(beats []domain.Heartbeat, from, to time.Time) Percentiles {
	var lat []int
	sum := 0
	for _, hb := range beats {
		if hb.CheckedAt.Before(from) || hb.CheckedAt.After(to) {
			continue
		}
		if hb.Status != domain.StatusUp || hb.Latency == nil {
			continue
		}
		ms := int(hb.Latency.Milliseconds())
		lat = append(lat, ms)
		sum += ms
	}
	var p Percentiles
	if len(lat) == 0 {
		return p
	}
	sort.Ints(lat)
	p.Count = len(lat)
	p.Min = lat[0]
	p.Max = lat[len(lat)-1]
	// Rounded, not truncated. Integer division always floors, so a set averaging
	// 9.9ms reported 9ms — a consistent downward bias in the one number an
	// operator is most likely to quote, and the only statistic here that is not a
	// sample taken verbatim from the data.
	p.Avg = int(math.Round(float64(sum) / float64(len(lat))))
	p.P50 = percentile(lat, 50)
	p.P95 = percentile(lat, 95)
	p.P99 = percentile(lat, 99)
	return p
}

// percentile returns the p-th percentile (nearest-rank) of a sorted slice.
func percentile(sorted []int, p int) int {
	if len(sorted) == 0 {
		return 0
	}
	// nearest-rank: rank = ceil(p/100 * n), 1-based.
	rank := (p*len(sorted) + 99) / 100
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// Downtime is one contiguous outage.
type Downtime struct {
	Start    time.Time     `json:"start"`
	End      time.Time     `json:"end"`
	Duration time.Duration `json:"duration_ns"`
	Ongoing  bool          `json:"ongoing"`
	Message  string        `json:"message,omitempty"`
}

// DowntimeLog reconstructs outages from a heartbeat stream. Beats are sorted
// ascending by time; a DOWN beat opens an outage and the next UP beat closes
// it. An outage still open at the end of the data is reported Ongoing and runs
// to the end of the report window `to` — which the service caps at min(to, now)
// — rather than to the last heartbeat: a push monitor may emit a single DOWN
// beat and then go silent, so closing at the last beat would collapse the
// duration to ~0 even though the outage is still in progress.
// prior is the newest heartbeat from BEFORE the window, or nil when none is
// known. When it is a DOWN beat the monitor was already broken as the window
// opened, and the outage is seeded here clipped to `from`. Without it such an
// outage vanished from the report entirely: nothing inside the window opens it,
// because the DOWN transition happened earlier and a monitor that stays down
// emits no further transition. A push monitor is the worst case — the watchdog
// trips it once and it then reports nothing at all — so a week-long push outage
// showed up as a completely clean report.
func DowntimeLog(beats []domain.Heartbeat, from, to time.Time, prior *domain.Heartbeat) []Downtime {
	in := make([]domain.Heartbeat, 0, len(beats))
	for _, hb := range beats {
		if hb.CheckedAt.Before(from) || hb.CheckedAt.After(to) {
			continue
		}
		in = append(in, hb)
	}
	sort.Slice(in, func(i, j int) bool { return in[i].CheckedAt.Before(in[j].CheckedAt) })

	// Empty, never nil. A monitor with no outage in the window is the ordinary
	// case, and the handler serves this inside the FullReport STRUCT — where
	// emptySliceAsArray cannot reach it, because that only normalises a slice which
	// IS the whole response. So `"downtime": null` went out for every healthy
	// monitor, which is the third instance of this class after #51 and #98.
	out := []Downtime{}
	var cur *Downtime
	if prior != nil && prior.Status == domain.StatusDown {
		// Clipped to the window start: the outage began earlier, but a windowed
		// report must not claim time outside its own window.
		cur = &Downtime{Start: from, Message: prior.Message}
	}
	for _, hb := range in {
		switch hb.Status {
		case domain.StatusDown:
			if cur == nil {
				cur = &Downtime{Start: hb.CheckedAt, Message: hb.Message}
			}
		case domain.StatusUp:
			if cur != nil {
				cur.End = hb.CheckedAt
				cur.Duration = cur.End.Sub(cur.Start)
				out = append(out, *cur)
				cur = nil
			}
		}
	}
	if cur != nil {
		cur.End = to
		cur.Duration = cur.End.Sub(cur.Start)
		cur.Ongoing = true
		out = append(out, *cur)
	}
	return out
}

// CountTransitions counts state changes (important heartbeats) in the range —
// the raw signal behind the "flakiest monitors" ranking.
func CountTransitions(beats []domain.Heartbeat, from, to time.Time) int {
	n := 0
	for _, hb := range beats {
		if hb.CheckedAt.Before(from) || hb.CheckedAt.After(to) {
			continue
		}
		if hb.Important {
			n++
		}
	}
	return n
}

// Flakiness is one row of the flakiest-monitors ranking.
type Flakiness struct {
	MonitorID   int64   `json:"monitor_id"`
	MonitorName string  `json:"monitor_name"`
	Transitions int     `json:"transitions"`
	Uptime      float64 `json:"uptime"`
}

// RankFlakiest sorts by transition count descending (ties broken by lower
// uptime, then id) and returns the top n (n <= 0 returns all).
func RankFlakiest(items []Flakiness, n int) []Flakiness {
	// make, not append onto a nil slice: appending nothing to nil yields nil, so
	// an instance with no monitors ranked to `null`. The handler wraps this in
	// {"items": ...}, which puts it out of reach of emptySliceAsArray — that only
	// normalises a slice that IS the response — so the null reached the client.
	out := make([]Flakiness, 0, len(items))
	out = append(out, items...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Transitions != out[j].Transitions {
			return out[i].Transitions > out[j].Transitions
		}
		if out[i].Uptime != out[j].Uptime {
			return out[i].Uptime < out[j].Uptime
		}
		return out[i].MonitorID < out[j].MonitorID
	})
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}
