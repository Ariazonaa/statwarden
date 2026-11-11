package main

import (
	"context"
	"errors"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/api"
	"github.com/Ariazonaa/statwarden/internal/domain"
)

// pagedStore serves a fixed monitor list page by page, the way PgStore does,
// and fails LatestHeartbeat for one monitor when asked to.
type pagedStore struct {
	*api.MemStore
	monitors []domain.Monitor
	failID   int64
}

func (s *pagedStore) ListMonitors(_ context.Context, _ api.MonitorFilter, p api.Page) ([]domain.Monitor, int, error) {
	end := min(p.Offset+p.Limit, len(s.monitors))
	if p.Offset >= end {
		return []domain.Monitor{}, len(s.monitors), nil
	}
	return s.monitors[p.Offset:end], len(s.monitors), nil
}

func (s *pagedStore) LatestHeartbeat(_ context.Context, id int64) (api.HeartbeatView, bool, error) {
	if id == s.failID {
		return api.HeartbeatView{}, false, errors.New("connection reset")
	}
	return api.HeartbeatView{}, false, nil
}

func syntheticMonitors(n int) []domain.Monitor {
	out := make([]domain.Monitor, n)
	for i := range out {
		out[i] = domain.Monitor{ID: int64(i + 1), Name: "m", Type: "http", Status: domain.StatusUp}
	}
	return out
}

// /metrics read a single page of 10 000 monitors, so every monitor past that
// silently vanished from the scrape and from the status counts.
func TestMetricsSnapshotCoversEveryMonitor(t *testing.T) {
	const n = 10_050
	src := newMetricsSource(&pagedStore{MemStore: api.NewMemStore(), monitors: syntheticMonitors(n)}, nil, 4)
	snap, err := src.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Monitors) != n || snap.StatusCounts["up"] != n {
		t.Fatalf("snapshot has %d monitors (%d up), want %d", len(snap.Monitors), snap.StatusCounts["up"], n)
	}
}

// A failed latency read was dropped, so a database fault showed as monitors
// that merely had no latency. It now fails the snapshot, which the collector
// reports as a scrape error.
func TestMetricsSnapshotReportsAFailedLatencyRead(t *testing.T) {
	src := newMetricsSource(&pagedStore{MemStore: api.NewMemStore(), monitors: syntheticMonitors(3), failID: 2}, nil, 4)
	if _, err := src.Snapshot(context.Background()); err == nil {
		t.Fatal("Snapshot hid a failed latency read")
	}
}
