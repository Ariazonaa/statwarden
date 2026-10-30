package engine

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Ariazonaa/statwarden/internal/db/sqlcgen"
	"github.com/Ariazonaa/statwarden/internal/domain"
)

// ConfigOpener unseals a monitor config that was encrypted at rest.
// *secrets.Box satisfies it. A nil opener leaves configs untouched, which is
// the DEV_MODE case where nothing was sealed in the first place.
type ConfigOpener interface {
	DecryptConfig(cfg json.RawMessage) (json.RawMessage, error)
}

// PgStore implements Store against PostgreSQL through the sqlc-generated
// queries.
type PgStore struct {
	q *sqlcgen.Queries
	// pool serves the hand-written queries for columns newer than the stale
	// sqlcgen package (CLAUDE.md: do not regenerate it as a side effect).
	pool *pgxpool.Pool
	open ConfigOpener
	log  *slog.Logger
}

// NewPgStore wraps a pgx pool in a Store. opener unseals monitors.config; pass
// nil only when nothing seals it (DEV_MODE without an ENCRYPTION_KEY).
func NewPgStore(pool *pgxpool.Pool, opener ConfigOpener, logger *slog.Logger) *PgStore {
	if logger == nil {
		logger = slog.Default()
	}
	return &PgStore{q: sqlcgen.New(pool), pool: pool, open: opener, log: logger}
}

// openConfig unseals a config read from the database.
//
// This is load-bearing, not defensive. The REST API seals monitors.config with
// the SecretsBox on every write, and ENCRYPTION_KEY is MANDATORY outside
// DEV_MODE, so in production the column normally holds an {"$encrypted":"..."}
// envelope. Handing that to checker.Validate fails with `unknown field
// "$encrypted"`, and engine.Start turns that into "skipping monitor" — so every
// monitor created or edited since at-rest encryption landed silently stopped
// being checked at the next process restart, while the API kept reporting it as
// active. The failure grows over time, because migration 0009 leaves legacy
// rows in plaintext and only re-seals them on their next write.
//
// A config that is not an envelope passes through unchanged, so those legacy
// plaintext rows keep working.
// A failure to open is LOGGED, not swallowed. Returning the sealed envelope
// quietly is what let a wrong or rotated ENCRYPTION_KEY look like a config
// problem in one monitor, when it is really every monitor failing validation
// and dropping out of the schedule at once.
func (s *PgStore) openConfig(id int64, cfg json.RawMessage) json.RawMessage {
	if s.open == nil || len(cfg) == 0 {
		return cfg
	}
	plain, err := s.open.DecryptConfig(cfg)
	if err != nil {
		s.log.Error("engine: cannot decrypt monitor config; the monitor will fail validation "+
			"and not be scheduled — check ENCRYPTION_KEY",
			"monitor_id", id, "error", err)
		return cfg
	}
	return plain
}

// ListActiveMonitors implements Store.
func (s *PgStore) ListActiveMonitors(ctx context.Context) ([]domain.Monitor, error) {
	rows, err := s.q.ListActiveMonitors(ctx)
	if err != nil {
		return nil, err
	}
	recovery, err := s.recoveryChecks(ctx)
	if err != nil {
		return nil, err
	}
	monitors := make([]domain.Monitor, 0, len(rows))
	for _, r := range rows {
		m := monitorFromRow(r)
		m.Config = s.openConfig(m.ID, m.Config)
		m.RecoveryChecks = recovery[m.ID]
		monitors = append(monitors, m)
	}
	return monitors, nil
}

// recoveryChecks loads monitors.recovery_checks (0020), which the generated
// ListActiveMonitors predates. A monitor missing from the map — one activated
// between the two queries — gets 0, which Step treats as single-success
// recovery, and its next Upsert from the API carries the real value anyway.
func (s *PgStore) recoveryChecks(ctx context.Context) (map[int64]int, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, recovery_checks FROM monitors WHERE active = true`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]int)
	for rows.Next() {
		var id int64
		var n int32
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = int(n)
	}
	return out, rows.Err()
}

// SaveHeartbeat implements Store.
func (s *PgStore) SaveHeartbeat(ctx context.Context, hb domain.Heartbeat) error {
	var latency pgtype.Int4
	if hb.Latency != nil {
		latency = pgtype.Int4{Int32: int32(hb.Latency.Milliseconds()), Valid: true} //nolint:gosec // latencies are bounded by check timeouts, far below int32 ms
	}
	_, err := s.q.InsertHeartbeat(ctx, sqlcgen.InsertHeartbeatParams{
		MonitorID: hb.MonitorID,
		CheckedAt: pgtype.Timestamptz{Time: hb.CheckedAt, Valid: true},
		Status:    string(hb.Status),
		LatencyMs: latency,
		Message:   hb.Message,
		Important: hb.Important,
		Retries:   int32(hb.Retries), //nolint:gosec // retry counters are tiny
		Extra:     hb.Extra,
	})
	return err
}

// UpdateMonitorStatus implements Store.
func (s *PgStore) UpdateMonitorStatus(ctx context.Context, monitorID int64, status domain.MonitorStatus) error {
	return s.q.UpdateMonitorStatus(ctx, sqlcgen.UpdateMonitorStatusParams{
		ID:     monitorID,
		Status: string(status),
	})
}

// SaveCertInfo implements Store.
func (s *PgStore) SaveCertInfo(ctx context.Context, monitorID int64, info domain.CertInfo) error {
	return s.q.UpsertCertInfo(ctx, sqlcgen.UpsertCertInfoParams{
		MonitorID:         monitorID,
		Subject:           pgtype.Text{String: info.Subject, Valid: info.Subject != ""},
		Issuer:            pgtype.Text{String: info.Issuer, Valid: info.Issuer != ""},
		Serial:            pgtype.Text{String: info.Serial, Valid: info.Serial != ""},
		FingerprintSha256: pgtype.Text{String: info.FingerprintSHA256, Valid: info.FingerprintSHA256 != ""},
		ValidFrom:         pgtype.Timestamptz{Time: info.ValidFrom, Valid: !info.ValidFrom.IsZero()},
		ValidTo:           pgtype.Timestamptz{Time: info.ValidTo, Valid: !info.ValidTo.IsZero()},
		DaysRemaining:     pgtype.Int4{Int32: int32(info.DaysRemaining), Valid: true}, //nolint:gosec // day counts are tiny
	})
}

// monitorFromRow converts a monitors row into the domain type, widening the
// integer second/minute columns to durations.
func monitorFromRow(r sqlcgen.Monitor) domain.Monitor {
	m := domain.Monitor{
		ID:             r.ID,
		Name:           r.Name,
		Type:           r.Type,
		Config:         json.RawMessage(r.Config),
		Interval:       time.Duration(r.IntervalSeconds) * time.Second,
		MaxRetries:     int(r.MaxRetries),
		RetryInterval:  time.Duration(r.RetryIntervalSeconds) * time.Second,
		ResendInterval: time.Duration(r.ResendIntervalMinutes) * time.Minute,
		UpsideDown:     r.UpsideDown,
		Active:         r.Active,
		Status:         domain.MonitorStatus(r.Status),
		Description:    r.Description,
		LinkURL:        r.LinkUrl,
	}
	if r.TimeoutSeconds.Valid {
		m.Timeout = time.Duration(r.TimeoutSeconds.Int32) * time.Second
	}
	if r.ParentID.Valid {
		parentID := r.ParentID.Int64
		m.ParentID = &parentID
	}
	if r.PushToken.Valid {
		m.PushToken = r.PushToken.String
	}
	return m
}
