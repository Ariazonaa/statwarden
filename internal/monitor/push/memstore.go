package push

import (
	"context"
	"sync"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
)

// MemStore is an in-memory Store for tests and DEV_MODE. It is safe for
// concurrent use.
type MemStore struct {
	mu         sync.Mutex
	monitors   map[int64]*domain.Monitor
	byToken    map[string]int64
	heartbeats []domain.Heartbeat
}

// NewMemStore seeds a MemStore with the given monitors. Their PushToken fields
// carry the raw token; the lookup index is keyed by its hash, mirroring the
// PgStore which stores only the hash.
func NewMemStore(monitors ...domain.Monitor) *MemStore {
	s := &MemStore{
		monitors: make(map[int64]*domain.Monitor),
		byToken:  make(map[string]int64),
	}
	for i := range monitors {
		m := monitors[i]
		s.monitors[m.ID] = &m
		if m.PushToken != "" {
			s.byToken[HashToken(m.PushToken)] = m.ID
		}
	}
	return s
}

// MonitorByPushToken implements Store: it resolves a token only for an active
// push monitor, so recording is restricted to type=push && active. The raw
// token is hashed before the lookup, matching how tokens are stored at rest.
func (s *MemStore) MonitorByPushToken(_ context.Context, token string) (domain.Monitor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byToken[HashToken(token)]
	if !ok {
		return domain.Monitor{}, ErrNotFound
	}
	m := s.monitors[id]
	if m.Type != "push" || !m.Active {
		return domain.Monitor{}, ErrNotFound
	}
	return *m, nil
}

// SaveHeartbeat implements Store.
func (s *MemStore) SaveHeartbeat(_ context.Context, hb domain.Heartbeat) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.heartbeats = append(s.heartbeats, hb)
	return nil
}

// UpdateMonitorStatus implements Store.
func (s *MemStore) UpdateMonitorStatus(_ context.Context, monitorID int64, status domain.MonitorStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.monitors[monitorID]; ok {
		m.Status = status
	}
	return nil
}

// ApplyBeat implements Store: append the heartbeat and reconcile the monitor's
// status under the same lock, so no watchdog sweep can interleave.
func (s *MemStore) ApplyBeat(_ context.Context, hb domain.Heartbeat) (domain.MonitorStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var prev domain.MonitorStatus
	if m, ok := s.monitors[hb.MonitorID]; ok {
		prev = m.Status
		hb.Important = prev != hb.Status
		s.heartbeats = append(s.heartbeats, hb)
		m.Status = hb.Status
		return prev, nil
	}
	s.heartbeats = append(s.heartbeats, hb)
	return prev, nil
}

// TripDown implements Store: mark the monitor DOWN only when no heartbeat newer
// than notAfter has arrived, so a fresh push observed after the watchdog's
// snapshot is not clobbered.
func (s *MemStore) TripDown(_ context.Context, hb domain.Heartbeat, notAfter time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if last, has := s.lastSeenLocked(hb.MonitorID); has && last.After(notAfter) {
		return false, nil
	}
	m, ok := s.monitors[hb.MonitorID]
	if !ok || m.Status == domain.StatusDown {
		return false, nil
	}
	s.heartbeats = append(s.heartbeats, hb)
	m.Status = domain.StatusDown
	return true, nil
}

// ListPushMonitors implements Store: it returns every push-type monitor with
// its most recent heartbeat time.
func (s *MemStore) ListPushMonitors(_ context.Context) ([]PushMonitor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []PushMonitor
	for _, m := range s.monitors {
		if m.Type != "push" {
			continue
		}
		last, has := s.lastSeenLocked(m.ID)
		out = append(out, PushMonitor{Monitor: *m, LastSeen: last, HasHeartbeat: has})
	}
	return out, nil
}

func (s *MemStore) lastSeenLocked(monitorID int64) (time.Time, bool) {
	var last time.Time
	found := false
	for _, hb := range s.heartbeats {
		if hb.MonitorID == monitorID && hb.CheckedAt.After(last) {
			last = hb.CheckedAt
			found = true
		}
	}
	return last, found
}

// Heartbeats returns a copy of the recorded heartbeats (test helper).
func (s *MemStore) Heartbeats() []domain.Heartbeat {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.Heartbeat(nil), s.heartbeats...)
}

// StatusOf returns a monitor's current status (test helper).
func (s *MemStore) StatusOf(id int64) (domain.MonitorStatus, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.monitors[id]
	if !ok {
		return "", false
	}
	return m.Status, true
}
