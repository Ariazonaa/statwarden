package engine

import (
	"context"
	"sync"

	"github.com/Ariazonaa/statwarden/internal/domain"
)

// Store is the engine's small persistence surface: load monitors at startup,
// record heartbeats, keep the denormalised monitor status and observed TLS
// certificates up to date. PgStore implements it against PostgreSQL; MemStore
// backs tests and database-less DEV_MODE runs.
type Store interface {
	// ListActiveMonitors returns all monitors with active = true.
	ListActiveMonitors(ctx context.Context) ([]domain.Monitor, error)
	// SaveHeartbeat appends one check result.
	SaveHeartbeat(ctx context.Context, hb domain.Heartbeat) error
	// UpdateMonitorStatus writes the current state to the monitor row.
	UpdateMonitorStatus(ctx context.Context, monitorID int64, status domain.MonitorStatus) error
	// SaveCertInfo upserts the certificate observed by a TLS-capable check.
	SaveCertInfo(ctx context.Context, monitorID int64, info domain.CertInfo) error
}

// MemStore is an in-memory Store for tests and DEV_MODE without a database.
// All methods are safe for concurrent use.
type MemStore struct {
	mu         sync.Mutex
	monitors   []domain.Monitor
	heartbeats []domain.Heartbeat
	statuses   map[int64]domain.MonitorStatus
	certs      map[int64]domain.CertInfo
}

// NewMemStore returns an empty MemStore seeded with the given monitors.
func NewMemStore(monitors ...domain.Monitor) *MemStore {
	return &MemStore{
		monitors: append([]domain.Monitor(nil), monitors...),
		statuses: make(map[int64]domain.MonitorStatus),
		certs:    make(map[int64]domain.CertInfo),
	}
}

// ListActiveMonitors implements Store.
func (s *MemStore) ListActiveMonitors(context.Context) ([]domain.Monitor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.Monitor
	for _, m := range s.monitors {
		if m.Active {
			out = append(out, m)
		}
	}
	return out, nil
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
	s.statuses[monitorID] = status
	return nil
}

// SaveCertInfo implements Store.
func (s *MemStore) SaveCertInfo(_ context.Context, monitorID int64, info domain.CertInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.certs[monitorID] = info
	return nil
}

// Heartbeats returns a copy of all recorded heartbeats.
func (s *MemStore) Heartbeats() []domain.Heartbeat {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.Heartbeat(nil), s.heartbeats...)
}

// Status returns the last status written for a monitor.
func (s *MemStore) Status(monitorID int64) (domain.MonitorStatus, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.statuses[monitorID]
	return st, ok
}

// Cert returns the last certificate written for a monitor.
func (s *MemStore) Cert(monitorID int64) (domain.CertInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.certs[monitorID]
	return c, ok
}
