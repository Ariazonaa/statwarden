package notify

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Ariazonaa/statwarden/internal/db/sqlcgen"
	"github.com/Ariazonaa/statwarden/internal/notify/secrets"
)

// PgStore implements Store against PostgreSQL through the sqlc-generated
// queries. It is also the persistence boundary for channel secrets: configs
// are decrypted with the secrets.Box on the way out, so the dispatcher and
// providers only ever see plaintext JSON.
type PgStore struct {
	q   *sqlcgen.Queries
	box *secrets.Box
	log *slog.Logger
	// pool backs the hand-written queries. internal/db/sqlcgen is generated and
	// already stale relative to later migrations, and regenerating it drags in
	// unrelated model changes, so a new query is written by hand here rather
	// than added to internal/db/queries.
	pool *pgxpool.Pool
}

// NewPgStore wraps a pgx pool in a Store. box decrypts channel configs; log
// records non-fatal decode problems.
func NewPgStore(pool *pgxpool.Pool, box *secrets.Box, logger *slog.Logger) *PgStore {
	if logger == nil {
		logger = slog.Default()
	}
	return &PgStore{q: sqlcgen.New(pool), box: box, log: logger, pool: pool}
}

// OpenOutages implements Store. The outage start is the monitor's most recent
// transition INTO down, which is exactly what heartbeats.important marks, so the
// subquery rides heartbeats_important_idx (monitor_id, checked_at DESC) WHERE
// important. A monitor whose transition beat has already been pruned by
// retention falls back to now(): the outage is still real, only its reported
// age is truncated, which is the safe direction to be wrong in.
func (s *PgStore) OpenOutages(ctx context.Context) ([]Outage, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT m.id,
		       COALESCE((
		           SELECT h.checked_at FROM heartbeats h
		           WHERE h.monitor_id = m.id AND h.important AND h.status = 'down'
		           ORDER BY h.checked_at DESC LIMIT 1
		       ), now())
		FROM monitors m
		WHERE m.status = 'down' AND m.active
		ORDER BY m.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Outage
	for rows.Next() {
		var o Outage
		if err := rows.Scan(&o.MonitorID, &o.Since); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ChannelsAwaitingRecovery implements Store.
//
// "Most recent successful delivery per channel, kept if it was a DOWN". Anchored
// on the delivery log rather than on the outage start, deliberately: the outage
// start is itself a fallback when the transition heartbeat has been pruned, and
// a time window derived from a guess would silently return nobody. This asks
// the question that actually matters — has this channel been told the monitor
// is down and not yet told otherwise — and needs no clock at all.
func (s *PgStore) ChannelsAwaitingRecovery(ctx context.Context, monitorID int64) ([]int64, error) {
	// One placeholder, one arg.
	rows, err := s.pool.Query(ctx, `
		SELECT notification_id FROM (
			SELECT DISTINCT ON (notification_id) notification_id, event_kind
			FROM notification_deliveries
			WHERE monitor_id = $1
			  AND notification_id IS NOT NULL
			  AND status = 'success'
			  AND event_kind IN ('down', 'up')
			ORDER BY notification_id, finished_at DESC, id DESC
		) latest
		WHERE event_kind = 'down'`, monitorID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Assignments implements Store.
func (s *PgStore) Assignments(ctx context.Context, monitorID int64) ([]Assignment, error) {
	rows, err := s.q.ListMonitorAssignments(ctx, monitorID)
	if err != nil {
		return nil, err
	}
	out := make([]Assignment, 0, len(rows))
	for _, r := range rows {
		cfg := json.RawMessage(r.Config)
		if s.box != nil {
			plain, derr := s.box.DecryptConfig(cfg)
			if derr != nil {
				// A single unreadable channel must not sink the others.
				s.log.Error("notify: decrypt channel config",
					"notification_id", r.ID, "name", r.Name, "error", derr)
				continue
			}
			cfg = plain
		}
		out = append(out, Assignment{
			Notification: Notification{
				ID:        r.ID,
				Name:      r.Name,
				Type:      r.Type,
				Config:    cfg,
				IsDefault: r.IsDefault,
				Active:    r.Active,
				Templates: ParseTemplateOverrides(json.RawMessage(r.Templates), s.log),
			},
			EscalationAfter: time.Duration(r.EscalationAfterMinutes) * time.Minute,
		})
	}
	return out, nil
}

// MonitorSettings implements Store.
func (s *PgStore) MonitorSettings(ctx context.Context, monitorID int64) (MonitorSettings, error) {
	row, err := s.q.GetMonitorNotifySettings(ctx, monitorID)
	if err != nil {
		return MonitorSettings{}, err
	}
	return MonitorSettings{
		Name:           row.Name,
		URL:            monitorURL(row.LinkUrl, s.openConfig(monitorID, row.Config)),
		ResendInterval: time.Duration(row.ResendIntervalMinutes) * time.Minute,
	}, nil
}

// openConfig unseals monitors.config for the URL lookup below.
//
// The column holds an encryption envelope, so reading a "url" field straight
// out of it never matched anything: every alert for a monitor without an
// explicit link_url went out with no URL in it, in production only — DEV_MODE
// has no box, stores plaintext, and looked perfectly fine. A config that cannot
// be opened is logged and treated as absent, because a missing link in a
// message is a blemish and a dropped alert is the bug this package exists to
// prevent.
func (s *PgStore) openConfig(monitorID int64, cfg []byte) json.RawMessage {
	raw := json.RawMessage(cfg)
	if s.box == nil || len(raw) == 0 {
		return raw
	}
	plain, err := s.box.DecryptConfig(raw)
	if err != nil {
		s.log.Error("notify: decrypt monitor config for the notification URL",
			"monitor_id", monitorID, "error", err)
		return nil
	}
	return plain
}

// RecordDelivery implements Store.
func (s *PgStore) RecordDelivery(ctx context.Context, d Delivery) error {
	return s.q.InsertNotificationDelivery(ctx, sqlcgen.InsertNotificationDeliveryParams{
		NotificationID: nullInt8(d.NotificationID),
		MonitorID:      nullInt8(d.MonitorID),
		ProviderType:   d.ProviderType,
		EventKind:      string(d.EventKind),
		Status:         string(d.Status),
		Error:          d.Error,
		Attempts:       int32(d.Attempts), //nolint:gosec // attempt counts are tiny
		StartedAt:      pgtype.Timestamptz{Time: d.StartedAt, Valid: !d.StartedAt.IsZero()},
		FinishedAt:     pgtype.Timestamptz{Time: d.FinishedAt, Valid: !d.FinishedAt.IsZero()},
	})
}

// nullInt8 maps 0 to SQL NULL (used for optional monitor/notification refs).
func nullInt8(v int64) pgtype.Int8 {
	if v == 0 {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: v, Valid: true}
}

// monitorURL derives a display URL for templates: the monitor's link_url if
// set, otherwise a "url" string field in its config, otherwise empty.
func monitorURL(linkURL string, config json.RawMessage) string {
	if linkURL != "" {
		return linkURL
	}
	var probe struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(config, &probe); err == nil && probe.URL != "" {
		return probe.URL
	}
	return ""
}

// --- durable hand-off (migration 0014) ---

// EnqueueOutbox implements Store. The row is inserted already claimed by this
// caller, so the sweeper cannot take an event that the process recording it is
// still holding in its own queue and deliver it a second time.
func (s *PgStore) EnqueueOutbox(ctx context.Context, rec OutboxRecord, lease time.Duration) (int64, error) {
	var monitorID *int64
	if rec.MonitorID != 0 {
		id := rec.MonitorID
		monitorID = &id
	}
	var id int64
	// $1 monitor_id, $2 kind, $3 payload, $4 lease seconds — four placeholders,
	// four args.
	err := s.pool.QueryRow(ctx,
		`INSERT INTO notification_outbox (monitor_id, kind, payload, claimed_until)
		 VALUES ($1, $2, $3, now() + make_interval(secs => $4)) RETURNING id`,
		monitorID, string(rec.Kind), []byte(rec.Payload), lease.Seconds()).Scan(&id)
	return id, err
}

// CompleteOutbox implements Store.
func (s *PgStore) CompleteOutbox(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM notification_outbox WHERE id = $1`, id)
	return err
}

// ClaimOutbox implements Store.
//
// FOR UPDATE SKIP LOCKED keeps two workers — or a worker racing the tick that
// follows it — from taking the same row, and the lease is written in the same
// statement so a claim is never observable without its expiry.
func (s *PgStore) ClaimOutbox(ctx context.Context, lease time.Duration, limit int) ([]OutboxItem, error) {
	// $1 lease seconds, $2 limit — two placeholders, two args.
	rows, err := s.pool.Query(ctx, `
		UPDATE notification_outbox SET
			attempts      = attempts + 1,
			claimed_until = now() + make_interval(secs => $1)
		WHERE id IN (
			SELECT id FROM notification_outbox
			WHERE claimed_until <= now()
			ORDER BY id
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, kind, monitor_id, payload, attempts`, lease.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOutbox(rows)
}

// DropExhaustedOutbox implements Store.
func (s *PgStore) DropExhaustedOutbox(ctx context.Context, maxAttempts int) ([]OutboxItem, error) {
	rows, err := s.pool.Query(ctx,
		`DELETE FROM notification_outbox WHERE attempts >= $1
		 RETURNING id, kind, monitor_id, payload, attempts`,
		maxAttempts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOutbox(rows)
}

// scanOutbox reads the projection the two queries above share.
func scanOutbox(rows pgx.Rows) ([]OutboxItem, error) {
	var out []OutboxItem
	for rows.Next() {
		var (
			item      OutboxItem
			kind      string
			monitorID pgtype.Int8
			payload   []byte
		)
		if err := rows.Scan(&item.ID, &kind, &monitorID, &payload, &item.Attempts); err != nil {
			return nil, err
		}
		item.Kind = OutboxKind(kind)
		item.MonitorID = monitorID.Int64
		item.Payload = payload
		out = append(out, item)
	}
	return out, rows.Err()
}
