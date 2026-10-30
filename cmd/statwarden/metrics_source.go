package main

import (
	"context"

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

// Snapshot implements metrics.Source.
func (m *metricsSource) Snapshot(ctx context.Context) (metrics.Snapshot, error) {
	monitors, _, err := m.store.ListMonitors(ctx, api.MonitorFilter{}, api.Page{Limit: 10000})
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
		if hb, has, err := m.store.LatestHeartbeat(ctx, mon.ID); err == nil && has && hb.LatencyMs != nil {
			sample.LatencyMs = float64(*hb.LatencyMs)
		}
		snap.Monitors = append(snap.Monitors, sample)
		snap.StatusCounts[status]++
	}
	return snap, nil
}
