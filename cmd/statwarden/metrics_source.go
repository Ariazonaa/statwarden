package main

import (
	"context"
	"fmt"

	"github.com/Ariazonaa/statwarden/internal/domain"

	"github.com/Ariazonaa/statwarden/internal/api"
	"github.com/Ariazonaa/statwarden/internal/metrics"
	"github.com/Ariazonaa/statwarden/internal/monitor/engine"
)

// metricsSource adapts the API store and monitor engine to metrics.Source: it
// lists monitors, overlays the engine's live status and reads each monitor's
// last latency from its most recent heartbeat.
type metricsSource struct {
	store   api.Store
	engine  *engine.Engine
	workers int
}

func newMetricsSource(store api.Store, eng *engine.Engine, workers int) *metricsSource {
	return &metricsSource{store: store, engine: eng, workers: workers}
}

// metricsPageSize is how many monitors Snapshot lists per round trip.
const metricsPageSize = 1000

// Snapshot implements metrics.Source.
func (m *metricsSource) Snapshot(ctx context.Context) (metrics.Snapshot, error) {
	monitors, err := m.allMonitors(ctx)
	if err != nil {
		return metrics.Snapshot{}, err
	}
	snap := metrics.Snapshot{
		StatusCounts: map[string]int{},
		Workers:      m.workers,
	}
	if m.engine != nil {
		_, snap.Unscheduled = m.engine.Scheduling()
	}
	for _, mon := range monitors {
		status := string(mon.Status)
		if m.engine != nil {
			if st, ok := m.engine.StatusOf(mon.ID); ok {
				status = string(st)
			}
		}
		sample := metrics.MonitorSample{
			ID:        mon.ID,
			Name:      mon.Name,
			Type:      mon.Type,
			Status:    status,
			LatencyMs: -1,
		}
		// A failed read fails the scrape: dropped, it looked exactly like a
		// monitor without a latency, and a database fault went unnoticed.
		hb, has, err := m.store.LatestHeartbeat(ctx, mon.ID)
		if err != nil {
			return metrics.Snapshot{}, fmt.Errorf("latest heartbeat of monitor %d: %w", mon.ID, err)
		}
		if has && hb.LatencyMs != nil {
			sample.LatencyMs = float64(*hb.LatencyMs)
		}
		snap.Monitors = append(snap.Monitors, sample)
		snap.StatusCounts[status]++
	}
	return snap, nil
}

// allMonitors lists every monitor, page by page. A single page of 10 000 used
// to be all there was: anything past it silently vanished from the scrape and
// from the status counts.
func (m *metricsSource) allMonitors(ctx context.Context) ([]domain.Monitor, error) {
	var out []domain.Monitor
	for {
		page, total, err := m.store.ListMonitors(ctx, api.MonitorFilter{}, api.Page{Limit: metricsPageSize, Offset: len(out)})
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if len(page) == 0 || len(out) >= total {
			return out, nil
		}
	}
}
