package report

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
)

// MemStore is an in-memory Store for tests and DEV_MODE.
type MemStore struct {
	mu         sync.Mutex
	monitors   []MonitorRef
	heartbeats map[int64][]domain.Heartbeat
	certs      []CertRow
	rollups    map[Bucket]map[rollupKey]Rollup
}

type rollupKey struct {
	monitorID int64
	at        time.Time
}

// NewMemStore returns an empty MemStore.
func NewMemStore() *MemStore {
	return &MemStore{
		heartbeats: map[int64][]domain.Heartbeat{},
		rollups:    map[Bucket]map[rollupKey]Rollup{},
	}
}

// AddMonitor registers a monitor identity (test helper).
func (s *MemStore) AddMonitor(m MonitorRef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.monitors = append(s.monitors, m)
}

// AddHeartbeat records a heartbeat (test helper).
func (s *MemStore) AddHeartbeat(hb domain.Heartbeat) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.heartbeats[hb.MonitorID] = append(s.heartbeats[hb.MonitorID], hb)
}

// AddCert registers a certificate row (test helper).
func (s *MemStore) AddCert(c CertRow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.certs = append(s.certs, c)
}

// HeartbeatsInRange implements Store.
func (s *MemStore) HeartbeatsInRange(_ context.Context, monitorID int64, from, to time.Time) ([]domain.Heartbeat, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.Heartbeat
	for _, hb := range s.heartbeats[monitorID] {
		if hb.CheckedAt.Before(from) || hb.CheckedAt.After(to) {
			continue
		}
		out = append(out, hb)
	}
	return out, nil
}

// LastHeartbeatBefore implements Store, mirroring PgStore: the newest beat
// strictly before at.
func (s *MemStore) LastHeartbeatBefore(_ context.Context, monitorID int64, at time.Time) (domain.Heartbeat, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best domain.Heartbeat
	found := false
	for _, hb := range s.heartbeats[monitorID] {
		if !hb.CheckedAt.Before(at) {
			continue
		}
		if !found || hb.CheckedAt.After(best.CheckedAt) {
			best, found = hb, true
		}
	}
	return best, found, nil
}

// ListMonitors implements Store.
func (s *MemStore) ListMonitors(context.Context) ([]MonitorRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]MonitorRef(nil), s.monitors...), nil
}

// CertOverview implements Store.
func (s *MemStore) CertOverview(context.Context) ([]CertRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Empty, never nil, matching PgStore: appending nothing to a nil slice
	// yields nil, and the handler nests this where emptySliceAsArray cannot
	// normalise it.
	out := make([]CertRow, 0, len(s.certs))
	return append(out, s.certs...), nil
}

// UpsertRollups implements Store.
func (s *MemStore) UpsertRollups(_ context.Context, bucket Bucket, rollups []Rollup) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.rollups[bucket]
	if m == nil {
		m = map[rollupKey]Rollup{}
		s.rollups[bucket] = m
	}
	for _, r := range rollups {
		m[rollupKey{r.MonitorID, r.BucketAt}] = r
	}
	return nil
}

// RollupsInRange implements Store.
func (s *MemStore) RollupsInRange(_ context.Context, bucket Bucket, monitorID int64, from, to time.Time) ([]Rollup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Rollup
	for _, r := range s.rollups[bucket] {
		if r.MonitorID != monitorID || r.BucketAt.Before(from) || r.BucketAt.After(to) {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BucketAt.Before(out[j].BucketAt) })
	return out, nil
}

// Rollups returns stored rollups for a bucket (test helper).
func (s *MemStore) Rollups(bucket Bucket) []Rollup {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Rollup
	for _, r := range s.rollups[bucket] {
		out = append(out, r)
	}
	return out
}
