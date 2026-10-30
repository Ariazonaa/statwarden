// Package metrics exposes Statwarden's monitor and engine state in the Prometheus
// exposition format. A pull-based collector reads a Source on every scrape so
// the exported numbers are always current, and the HTTP handler is optionally
// gated behind a bearer token configured in settings.
package metrics

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Ariazonaa/statwarden/internal/deprecation"
)

// MonitorSample is one monitor's current state for export.
type MonitorSample struct {
	ID     int64
	Name   string
	Type   string
	Status string // up/down/pending/paused/maintenance
	// LatencyMs is the last observed latency; negative means "unknown" and is
	// not exported.
	LatencyMs float64
}

// Snapshot is the full metric state pulled on each scrape.
type Snapshot struct {
	Monitors []MonitorSample
	// StatusCounts is monitor count keyed by status ("up", "down", …).
	StatusCounts map[string]int
	// Workers is the engine worker-pool size (0 when unknown).
	Workers int
	// Unscheduled is how many monitors the engine could not schedule. Anything
	// above zero means those monitors are NOT being checked while the API still
	// lists them as active, so it is the one number worth alerting on directly.
	Unscheduled int
}

// Source produces a Snapshot on demand. *engine adapters implement it in
// cmd/statwarden; tests use a fake.
type Source interface {
	Snapshot(ctx context.Context) (Snapshot, error)
}

const namespace = "statwarden"

var (
	monitorUpDesc = prometheus.NewDesc(
		namespace+"_monitor_up",
		"Whether a monitor's last check was up (1) or not (0).",
		[]string{"monitor_id", "monitor_name", "type"}, nil)
	monitorLatencyDesc = prometheus.NewDesc(
		namespace+"_monitor_latency_ms",
		"Last observed monitor latency in milliseconds.",
		[]string{"monitor_id", "monitor_name", "type"}, nil)
	monitorsTotalDesc = prometheus.NewDesc(
		namespace+"_monitors_total",
		"Number of monitors grouped by status.",
		[]string{"status"}, nil)
	workersDesc = prometheus.NewDesc(
		namespace+"_engine_workers",
		"Configured engine worker-pool size.",
		nil, nil)
	scrapeErrorDesc = prometheus.NewDesc(
		namespace+"_scrape_error",
		"1 when the last metrics scrape failed to read Statwarden state.",
		nil, nil)
	unscheduledDesc = prometheus.NewDesc(
		namespace+"_monitors_unscheduled",
		"Monitors the engine could not schedule; these are NOT being checked even though the API lists them as active.",
		nil, nil)
)

// ---- Background-job self-observation ---------------------------------------
//
// The rollup and heartbeat-retention jobs run on their own tickers, out of band
// from any scrape, so their health cannot be derived from a Snapshot the way the
// monitor gauges above are. Instead each job reports one run through
// ObserveJobRun; the vectors below are updated in place and collected by the
// same registry NewRegistry builds. A single "job" label keeps the two jobs —
// and any future one — on a shared, low-cardinality surface.
const (
	// JobRetention is the label the heartbeat-pruning job records under. Its
	// rows_total counts heartbeats PRUNED.
	JobRetention = "retention"
	// JobRollup is the label the hourly/daily rollup job records under. Its
	// rows_total counts rollup rows WRITTEN.
	JobRollup = "rollup"
)

var (
	jobRunsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "job", Name: "runs_total",
		Help: "Completed background-job runs, by job.",
	}, []string{"job"})
	jobErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "job", Name: "errors_total",
		Help: "Background-job runs that ended in an error, by job.",
	}, []string{"job"})
	jobRowsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "job", Name: "rows_total",
		Help: "Rows affected by background-job runs — heartbeats pruned for the " +
			"retention job, rollup rows written for the rollup job — by job.",
	}, []string{"job"})
	jobLastRunSeconds = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "job", Name: "last_run_timestamp_seconds",
		Help: "Unix time of the last completed run of a background job, by job.",
	}, []string{"job"})
	jobDurationSeconds = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "job", Name: "duration_seconds",
		Help: "Wall-clock duration of the last background-job run, in seconds, by job.",
	}, []string{"job"})
)

// jobCollectors is the set of job vectors registered into every registry
// NewRegistry builds. They are package-level so the jobs can update them without
// threading a handle through, and registering one Collector in several registries
// (each NewRegistry call makes a fresh one) is allowed.
func jobCollectors() []prometheus.Collector {
	return []prometheus.Collector{
		jobRunsTotal, jobErrorsTotal, jobRowsTotal, jobLastRunSeconds, jobDurationSeconds,
	}
}

// ObserveJobRun records one completed run of a background job: it always counts
// the run and stamps its finish time and duration, then either adds the rows it
// affected (on success) or counts one error (on failure). rows is ignored when
// err is non-nil, where the count is not meaningful. Safe for concurrent use —
// the underlying vectors are goroutine-safe.
func ObserveJobRun(job string, rows int, dur time.Duration, err error) {
	jobRunsTotal.WithLabelValues(job).Inc()
	jobLastRunSeconds.WithLabelValues(job).SetToCurrentTime()
	jobDurationSeconds.WithLabelValues(job).Set(dur.Seconds())
	if err != nil {
		jobErrorsTotal.WithLabelValues(job).Inc()
		return
	}
	if rows > 0 {
		jobRowsTotal.WithLabelValues(job).Add(float64(rows))
	}
}

// collector is a pull-based prometheus.Collector over a Source.
type collector struct {
	src Source
}

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- monitorUpDesc
	ch <- monitorLatencyDesc
	ch <- monitorsTotalDesc
	ch <- workersDesc
	ch <- scrapeErrorDesc
	ch <- unscheduledDesc
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	snap, err := c.src.Snapshot(context.Background())
	if err != nil {
		ch <- prometheus.MustNewConstMetric(scrapeErrorDesc, prometheus.GaugeValue, 1)
		return
	}
	ch <- prometheus.MustNewConstMetric(scrapeErrorDesc, prometheus.GaugeValue, 0)

	for _, m := range snap.Monitors {
		id := strconv.FormatInt(m.ID, 10)
		up := 0.0
		if m.Status == "up" {
			up = 1
		}
		ch <- prometheus.MustNewConstMetric(monitorUpDesc, prometheus.GaugeValue, up, id, m.Name, m.Type)
		if m.LatencyMs >= 0 {
			ch <- prometheus.MustNewConstMetric(monitorLatencyDesc, prometheus.GaugeValue, m.LatencyMs, id, m.Name, m.Type)
		}
	}
	for status, n := range snap.StatusCounts {
		ch <- prometheus.MustNewConstMetric(monitorsTotalDesc, prometheus.GaugeValue, float64(n), status)
	}
	if snap.Workers > 0 {
		ch <- prometheus.MustNewConstMetric(workersDesc, prometheus.GaugeValue, float64(snap.Workers))
	}
	// Always exported, including the healthy zero: an alert on "> 0" needs the
	// series to exist before it goes wrong.
	ch <- prometheus.MustNewConstMetric(unscheduledDesc, prometheus.GaugeValue, float64(snap.Unscheduled))
}

// NewRegistry builds a Prometheus registry with the Statwarden collector (and the
// standard Go/process collectors) registered.
func NewRegistry(src Source) *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(&collector{src: src})
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	reg.MustRegister(jobCollectors()...)
	return reg
}

// Handler returns an http.Handler serving the Prometheus exposition format. If
// token is non-empty, requests must present it via the Authorization: Bearer
// header or an X-Metrics-Token header; otherwise the endpoint is open.
//
// A ?token= query parameter is still accepted so existing scrape configurations
// keep working, but it is DEPRECATED: the URL is written to reverse-proxy access
// logs and to Prometheus's own target metadata, which turns the scrape secret
// into something several systems keep a copy of. Its use is warned about once
// per process — a scrape runs every few seconds, so logging every occurrence
// would drown the log rather than inform anyone.
func Handler(src Source, token string) http.Handler {
	reg := NewRegistry(src)
	promHandler := promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	var warnQueryToken sync.Once
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token != "" {
			ok, viaQuery := tokenOK(r, token)
			if !ok {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "metrics token required", http.StatusUnauthorized)
				return
			}
			if viaQuery {
				// Headers on every response, not once per process like the log
				// line: they are what a scrape client can actually act on, and
				// Prometheus does not read this instance's logs.
				deprecation.Mark(w.Header())
				warnQueryToken.Do(func() {
					slog.Warn("deprecated: metrics scraped with a ?token= query parameter; "+
						"use the Authorization: Bearer or X-Metrics-Token header instead",
						"sunset", deprecation.Sunset.Format(time.RFC3339))
				})
			}
		}
		promHandler.ServeHTTP(w, r)
	})
}

// tokenOK reports whether the request carries the configured metrics token, and
// whether it arrived through the deprecated query parameter.
//
// The comparisons are constant-time, matching how every other token in this
// codebase is checked (CSRF, the status-page unlock): a scrape endpoint is
// reachable by anything that can route to it, so the timing of a rejection
// should not narrow down the token.
func tokenOK(r *http.Request, token string) (ok, viaQuery bool) {
	want := []byte("Bearer " + token)
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) == 1 {
		return true, false
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Metrics-Token")), []byte(token)) == 1 {
		return true, false
	}
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("token")), []byte(token)) == 1 {
		return true, true
	}
	return false, false
}
