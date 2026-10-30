package push

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Ariazonaa/statwarden/internal/domain"
)

// ConfigOpener unseals a monitor config that was encrypted at rest.
// *secrets.Box satisfies it; nil means nothing was sealed (DEV_MODE).
type ConfigOpener interface {
	DecryptConfig(cfg json.RawMessage) (json.RawMessage, error)
}

// PgStore implements Store against PostgreSQL with hand-written pgx queries,
// mirroring the style of internal/api and internal/monitor/engine.
type PgStore struct {
	pool *pgxpool.Pool
	open ConfigOpener
	log  *slog.Logger
}

// NewPgStore wraps a pgx pool as a push Store. opener unseals monitors.config;
// pass nil only when nothing seals it (DEV_MODE without an ENCRYPTION_KEY).
func NewPgStore(pool *pgxpool.Pool, opener ConfigOpener, logger *slog.Logger) *PgStore {
	if logger == nil {
		logger = slog.Default()
	}
	return &PgStore{pool: pool, open: opener, log: logger}
}

// openConfig unseals a config read from the database.
//
// The watchdog derives its deadline from the push config via
// pushcheck.Deadline, which parses the JSON. Against a sealed
// {"$encrypted":"..."} envelope that parse fails, Deadline reports ok=false and
// the monitor is skipped — so a push monitor that had genuinely stopped
// reporting was never tripped DOWN and never alerted on. Non-enveloped legacy
// rows pass through unchanged.
// A failure to open is logged rather than swallowed: it means the watchdog
// cannot derive a deadline for this monitor and will never trip it DOWN, which
// is silence in exactly the place silence is dangerous.
func (s *PgStore) openConfig(id int64, cfg json.RawMessage) json.RawMessage {
	if s.open == nil || len(cfg) == 0 {
		return cfg
	}
	plain, err := s.open.DecryptConfig(cfg)
	if err != nil {
		s.log.Error("push: cannot decrypt monitor config; the watchdog cannot compute a "+
			"deadline and will never trip this monitor DOWN — check ENCRYPTION_KEY",
			"monitor_id", id, "error", err)
		return cfg
	}
	return plain
}

const monitorColumns = `id, name, type, config, interval_seconds, timeout_seconds,
	max_retries, retry_interval_seconds, resend_interval_minutes, upside_down,
	active, status, parent_id, push_token_hash, description, link_url`

// MonitorByPushToken implements Store: it resolves a token only for an active
// push monitor, so a paused monitor, a non-push monitor, or an unknown token
// all read as ErrNotFound and recording stays restricted to type=push && active.
// Tokens are stored hashed, so the incoming raw token is hashed the same way
// before the lookup — the column never holds the plaintext to match against.
func (s *PgStore) MonitorByPushToken(ctx context.Context, token string) (domain.Monitor, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+monitorColumns+`
		FROM monitors WHERE push_token_hash = $1 AND type = 'push' AND active = true`, HashToken(token))
	m, err := scanMonitor(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Monitor{}, ErrNotFound
	}
	m.Config = s.openConfig(m.ID, m.Config)
	return m, err
}

// SaveHeartbeat implements Store.
func (s *PgStore) SaveHeartbeat(ctx context.Context, hb domain.Heartbeat) error {
	var latency pgtype.Int4
	if hb.Latency != nil {
		latency = pgtype.Int4{Int32: int32(hb.Latency.Milliseconds()), Valid: true} //nolint:gosec // push latencies are bounded
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO heartbeats (monitor_id, checked_at, status, latency_ms, message, important, retries, extra)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		hb.MonitorID, hb.CheckedAt, string(hb.Status), latency, hb.Message, hb.Important, hb.Retries, jsonOrNil(hb.Extra))
	return err
}

// UpdateMonitorStatus implements Store.
func (s *PgStore) UpdateMonitorStatus(ctx context.Context, monitorID int64, status domain.MonitorStatus) error {
	_, err := s.pool.Exec(ctx, `UPDATE monitors SET status = $2, updated_at = now() WHERE id = $1`, monitorID, string(status))
	return err
}

// insertHeartbeatTx inserts one heartbeat inside a transaction, mirroring
// SaveHeartbeat's column mapping.
func insertHeartbeatTx(ctx context.Context, tx pgx.Tx, hb domain.Heartbeat) error {
	var latency pgtype.Int4
	if hb.Latency != nil {
		latency = pgtype.Int4{Int32: int32(hb.Latency.Milliseconds()), Valid: true} //nolint:gosec // push latencies are bounded
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO heartbeats (monitor_id, checked_at, status, latency_ms, message, important, retries, extra)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		hb.MonitorID, hb.CheckedAt, string(hb.Status), latency, hb.Message, hb.Important, hb.Retries, jsonOrNil(hb.Extra))
	return err
}

// ApplyBeat implements Store: it records the heartbeat and reconciles the
// status column in a single transaction, holding a row lock on the monitor so
// a concurrent watchdog sweep cannot slip a DOWN in between the two writes.
// lockLiveMonitor takes the row lock for a write that must not touch a monitor
// which is paused or in the recycle bin. Shared by both writers so they cannot
// drift — hardening one and not the other is exactly what happened before.
const lockLiveMonitor = `SELECT status FROM monitors
	WHERE id = $1 AND active = true AND deleted_at IS NULL
	FOR UPDATE`

func (s *PgStore) ApplyBeat(ctx context.Context, hb domain.Heartbeat) (domain.MonitorStatus, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	var prev string
	// The guard is repeated HERE, not only in the token lookup.
	//
	// Record does the lookup and this write in two separate transactions, so a
	// heartbeat already in flight when an operator hit Delete found the row by id
	// alone and wrote status='up' onto it. The recycle bin then listed a monitor
	// claiming to be up, with a heartbeat stamped after its own deletion, and a
	// paused→up alert went out for something the operator had just removed.
	// Migration 0018 relies on the recorder filtering `active`; that only ever
	// covered the lookup.
	if err := tx.QueryRow(ctx, lockLiveMonitor, hb.MonitorID).Scan(&prev); errors.Is(err, pgx.ErrNoRows) {
		// Deleted or paused between the token lookup and this write. That is a
		// policy rejection, identical in meaning to the 404 the token lookup
		// returns one transaction earlier — so it is reported as ErrNotFound and
		// not as the raw pgx error, which the handler could only turn into a 500
		// "record heartbeat". A push agent is a cron job reading `curl --fail`:
		// 5xx makes it retry and alert its owner about a monitor the operator
		// deliberately removed.
		return "", ErrNotFound
	} else if err != nil {
		return "", err
	}
	hb.Important = domain.MonitorStatus(prev) != hb.Status
	if err := insertHeartbeatTx(ctx, tx, hb); err != nil {
		return "", err
	}
	if domain.MonitorStatus(prev) != hb.Status {
		if _, err := tx.Exec(ctx, `UPDATE monitors SET status = $2, updated_at = now() WHERE id = $1`, hb.MonitorID, string(hb.Status)); err != nil {
			return "", err
		}
	}
	return domain.MonitorStatus(prev), tx.Commit(ctx)
}

// TripDown implements Store: under a row lock it verifies the monitor is not
// already DOWN and that no heartbeat newer than notAfter exists (a fresh push
// that overtook the watchdog), then records the DOWN heartbeat and status. It
// returns false without writing when the trip is stale.
func (s *PgStore) TripDown(ctx context.Context, hb domain.Heartbeat, notAfter time.Time) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	var status string
	// The SAME guard the beat path carries, and for a worse reason: this writer
	// emits a DOWN alert.
	//
	// Sweep takes its list from ListPushMonitors and then calls TripDown per
	// monitor in a separate transaction, so anything binned or paused in that gap
	// used to get a heartbeat stamped after its own deletion, a status of "down"
	// written over the "paused" the pause had just set, and a page sent for a
	// monitor the operator had removed. Hardening only ApplyBeat left the noisy
	// half fixed and the loud half open.
	if err := tx.QueryRow(ctx, lockLiveMonitor, hb.MonitorID).Scan(&status); errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	} else if err != nil {
		return false, err
	}
	if domain.MonitorStatus(status) == domain.StatusDown {
		return false, nil
	}
	var last pgtype.Timestamptz
	if err := tx.QueryRow(ctx, `SELECT max(checked_at) FROM heartbeats WHERE monitor_id = $1`, hb.MonitorID).Scan(&last); err != nil {
		return false, err
	}
	if last.Valid && last.Time.After(notAfter) {
		return false, nil
	}
	if err := insertHeartbeatTx(ctx, tx, hb); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE monitors SET status = $2, updated_at = now() WHERE id = $1`, hb.MonitorID, string(domain.StatusDown)); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// ListPushMonitors implements Store: active push monitors with their most
// recent heartbeat time.
func (s *PgStore) ListPushMonitors(ctx context.Context) ([]PushMonitor, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+monitorColumns+`,
		       (SELECT max(checked_at) FROM heartbeats h WHERE h.monitor_id = monitors.id) AS last_seen
		FROM monitors
		WHERE type = 'push' AND active = true`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PushMonitor
	for rows.Next() {
		var (
			m       domain.Monitor
			cfg     []byte
			timeout pgtype.Int4
			parent  pgtype.Int8
			token   pgtype.Text
			last    pgtype.Timestamptz
			ivl, ri int32
			rim     int32
		)
		if err := rows.Scan(&m.ID, &m.Name, &m.Type, &cfg, &ivl, &timeout, &m.MaxRetries,
			&ri, &rim, &m.UpsideDown, &m.Active, &m.Status, &parent, &token, &m.Description, &m.LinkURL, &last); err != nil {
			return nil, err
		}
		// Unsealed here, not in scanMonitor: this is the slice the watchdog
		// derives its deadline from, and a sealed envelope makes that derivation
		// fail silently.
		m.Config = s.openConfig(m.ID, json.RawMessage(cfg))
		m.Interval = time.Duration(ivl) * time.Second
		m.RetryInterval = time.Duration(ri) * time.Second
		m.ResendInterval = time.Duration(rim) * time.Minute
		if timeout.Valid {
			m.Timeout = time.Duration(timeout.Int32) * time.Second
		}
		if parent.Valid {
			p := parent.Int64
			m.ParentID = &p
		}
		if token.Valid {
			m.PushToken = token.String
		}
		out = append(out, PushMonitor{Monitor: m, LastSeen: last.Time, HasHeartbeat: last.Valid})
	}
	return out, rows.Err()
}

// scanMonitor scans the monitorColumns projection into a domain.Monitor.
func scanMonitor(row pgx.Row) (domain.Monitor, error) {
	var (
		m       domain.Monitor
		cfg     []byte
		timeout pgtype.Int4
		parent  pgtype.Int8
		token   pgtype.Text
		ivl, ri int32
		mr, rim int32
	)
	if err := row.Scan(&m.ID, &m.Name, &m.Type, &cfg, &ivl, &timeout, &mr,
		&ri, &rim, &m.UpsideDown, &m.Active, &m.Status, &parent, &token, &m.Description, &m.LinkURL); err != nil {
		return domain.Monitor{}, err
	}
	m.Config = json.RawMessage(cfg)
	m.Interval = time.Duration(ivl) * time.Second
	m.MaxRetries = int(mr)
	m.RetryInterval = time.Duration(ri) * time.Second
	m.ResendInterval = time.Duration(rim) * time.Minute
	if timeout.Valid {
		m.Timeout = time.Duration(timeout.Int32) * time.Second
	}
	if parent.Valid {
		p := parent.Int64
		m.ParentID = &p
	}
	if token.Valid {
		m.PushToken = token.String
	}
	return m, nil
}

func jsonOrNil(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}
