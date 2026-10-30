package report

import (
	"sort"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
)

// Bucket is the rollup granularity.
type Bucket string

const (
	// Hourly rolls raw heartbeats into one row per hour.
	Hourly Bucket = "hour"
	// Daily rolls raw heartbeats into one row per day.
	Daily Bucket = "day"
)

// Rollup is one aggregated bucket of heartbeats for a monitor. It carries
// enough to reconstruct uptime and latency summaries without the raw rows.
type Rollup struct {
	MonitorID  int64     `json:"monitor_id"`
	BucketAt   time.Time `json:"bucket_at"` // truncated to the bucket boundary (UTC)
	Up         int       `json:"up"`
	Down       int       `json:"down"`
	AvgLatency int       `json:"avg_latency_ms"`
	MinLatency int       `json:"min_latency_ms"`
	MaxLatency int       `json:"max_latency_ms"`
}

// truncate returns t floored to the bucket boundary in UTC.
func (b Bucket) truncate(t time.Time) time.Time {
	u := t.UTC()
	switch b {
	case Daily:
		return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	default: // Hourly
		return time.Date(u.Year(), u.Month(), u.Day(), u.Hour(), 0, 0, 0, time.UTC)
	}
}

// Aggregate groups heartbeats for a single monitor into buckets. The result is
// sorted ascending by bucket time. Only up/down beats contribute to the
// counts; latency stats use up beats that carry a latency.
func Aggregate(monitorID int64, beats []domain.Heartbeat, b Bucket) []Rollup {
	type acc struct {
		up, down      int
		sum, min, max int
		latCount      int
	}
	buckets := map[time.Time]*acc{}
	for _, hb := range beats {
		key := b.truncate(hb.CheckedAt)
		a := buckets[key]
		if a == nil {
			a = &acc{min: -1}
			buckets[key] = a
		}
		switch hb.Status {
		case domain.StatusUp:
			a.up++
		case domain.StatusDown:
			a.down++
		default:
			continue
		}
		if hb.Status == domain.StatusUp && hb.Latency != nil {
			ms := int(hb.Latency.Milliseconds())
			a.sum += ms
			a.latCount++
			if a.min < 0 || ms < a.min {
				a.min = ms
			}
			if ms > a.max {
				a.max = ms
			}
		}
	}
	out := make([]Rollup, 0, len(buckets))
	for at, a := range buckets {
		r := Rollup{MonitorID: monitorID, BucketAt: at, Up: a.up, Down: a.down}
		if a.latCount > 0 {
			r.AvgLatency = a.sum / a.latCount
			r.MinLatency = a.min
			r.MaxLatency = a.max
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BucketAt.Before(out[j].BucketAt) })
	return out
}

// UptimeFromRollups computes availability across a slice of rollups in
// [from, to] (bucket boundaries compared inclusively against from and to).
func UptimeFromRollups(rollups []Rollup, from, to time.Time) UptimeResult {
	res := UptimeResult{From: from, To: to, Uptime: -1}
	for _, r := range rollups {
		if r.BucketAt.Before(from) || r.BucketAt.After(to) {
			continue
		}
		res.Up += r.Up
		res.Down += r.Down
	}
	res.Total = res.Up + res.Down
	if res.Total > 0 {
		res.Uptime = float64(res.Up) / float64(res.Total)
	}
	return res
}
