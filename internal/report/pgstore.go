package report

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Ariazonaa/statwarden/internal/domain"
)

// PgStore implements Store against PostgreSQL with hand-written pgx queries.
type PgStore struct {
	pool *pgxpool.Pool
}

// NewPgStore wraps a pgx pool as a report Store.
func NewPgStore(pool *pgxpool.Pool) *PgStore { return &PgStore{pool: pool} }

// HeartbeatsInRange implements Store.
func (s *PgStore) HeartbeatsInRange(ctx context.Context, monitorID int64, from, to time.Time) ([]domain.Heartbeat, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT monitor_id, checked_at, status, latency_ms, message, important, retries, extra
		FROM heartbeats
		WHERE monitor_id = $1 AND checked_at >= $2 AND checked_at <= $3
		ORDER BY checked_at ASC`, monitorID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Heartbeat
	for rows.Next() {
		hb, err := scanReportHeartbeat(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, hb)
	}
	return out, rows.Err()
}

// scanReportHeartbeat scans the heartbeat projection shared by the two read
// queries above, so their column lists cannot drift apart.
func scanReportHeartbeat(row pgx.Row) (domain.Heartbeat, error) {
	var (
		hb      domain.Heartbeat
		latency pgtype.Int4
		extra   []byte
		checked time.Time
		status  string
	)
	if err := row.Scan(&hb.MonitorID, &checked, &status, &latency, &hb.Message, &hb.Important, &hb.Retries, &extra); err != nil {
		return domain.Heartbeat{}, err
	}
	hb.CheckedAt = checked
	hb.Status = domain.MonitorStatus(status)
	if latency.Valid {
		d := time.Duration(latency.Int32) * time.Millisecond
		hb.Latency = &d
	}
	hb.Extra = json.RawMessage(extra)
	return hb, nil
}

// LastHeartbeatBefore implements Store. It rides
// heartbeats_monitor_id_checked_at_idx (monitor_id, checked_at DESC), so it is
// an index seek rather than a scan even on a monitor with a long history.
func (s *PgStore) LastHeartbeatBefore(ctx context.Context, monitorID int64, at time.Time) (domain.Heartbeat, bool, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT monitor_id, checked_at, status, latency_ms, message, important, retries, extra
		FROM heartbeats
		WHERE monitor_id = $1 AND checked_at < $2
		ORDER BY checked_at DESC LIMIT 1`, monitorID, at)
	if err != nil {
		return domain.Heartbeat{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return domain.Heartbeat{}, false, rows.Err()
	}
	hb, err := scanReportHeartbeat(rows)
	if err != nil {
		return domain.Heartbeat{}, false, err
	}
	return hb, true, rows.Err()
}

// ListMonitors implements Store.
//
// Monitors in the recycle bin are excluded (migration 0018). This backs the
// flakiest ranking and the certificate overview, and a monitor somebody deleted
// last week has no business appearing in either — least of all in the SLA report,
// which is meant to describe what is being monitored now. Restoring it brings it
// back here too, because the filter is on the flag rather than on a copy.
func (s *PgStore) ListMonitors(ctx context.Context) ([]MonitorRef, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name, type FROM monitors WHERE deleted_at IS NULL ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MonitorRef
	for rows.Next() {
		var m MonitorRef
		if err := rows.Scan(&m.ID, &m.Name, &m.Type); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CertOverview implements Store.
//
// cert_info rows outlive a soft delete (nothing cascades off a marked row), so
// without the deleted_at filter a monitor somebody binned last week would keep
// appearing in the certificate overview — and now in the dashboard's certificate
// widget, warning about a domain nobody is watching any more.
func (s *PgStore) CertOverview(ctx context.Context) ([]CertRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.monitor_id, m.name, c.subject, c.issuer, c.valid_to, c.days_remaining
		FROM cert_info c JOIN monitors m ON m.id = c.monitor_id
		WHERE m.deleted_at IS NULL
		ORDER BY c.valid_to ASC NULLS LAST`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Empty, never nil: the handler wraps this in {"items": ...}, where
	// emptySliceAsArray cannot reach it.
	out := []CertRow{}
	for rows.Next() {
		var (
			r       CertRow
			subject pgtype.Text
			issuer  pgtype.Text
			validTo pgtype.Timestamptz
			days    pgtype.Int4
		)
		if err := rows.Scan(&r.MonitorID, &r.MonitorName, &subject, &issuer, &validTo, &days); err != nil {
			return nil, err
		}
		r.Subject = subject.String
		r.Issuer = issuer.String
		r.ValidTo = validTo.Time
		r.DaysRemaining = int(days.Int32)
		out = append(out, r)
	}
	return out, rows.Err()
}

// rollupTable maps a bucket to its rollup table.
func rollupTable(bucket Bucket) string {
	if bucket == Daily {
		return "heartbeat_rollups_daily"
	}
	return "heartbeat_rollups_hourly"
}

// UpsertRollups implements Store, writing to heartbeat_rollups_hourly or
// heartbeat_rollups_daily depending on the bucket.
func (s *PgStore) UpsertRollups(ctx context.Context, bucket Bucket, rollups []Rollup) error {
	if len(rollups) == 0 {
		return nil
	}
	table := rollupTable(bucket)
	batch := &pgx.Batch{}
	for _, r := range rollups {
		batch.Queue(`
			INSERT INTO `+table+` (monitor_id, bucket_at, up_count, down_count, avg_latency_ms, min_latency_ms, max_latency_ms)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (monitor_id, bucket_at) DO UPDATE SET
				up_count = EXCLUDED.up_count,
				down_count = EXCLUDED.down_count,
				avg_latency_ms = EXCLUDED.avg_latency_ms,
				min_latency_ms = EXCLUDED.min_latency_ms,
				max_latency_ms = EXCLUDED.max_latency_ms`,
			r.MonitorID, r.BucketAt, r.Up, r.Down, r.AvgLatency, r.MinLatency, r.MaxLatency)
	}
	br := s.pool.SendBatch(ctx, batch)
	defer br.Close() //nolint:errcheck // best-effort close
	for range rollups {
		if _, err := br.Exec(); err != nil {
			return err
		}
	}
	return nil
}

// RollupsInRange implements Store, reading back the buckets written by
// UpsertRollups.
func (s *PgStore) RollupsInRange(ctx context.Context, bucket Bucket, monitorID int64, from, to time.Time) ([]Rollup, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT monitor_id, bucket_at, up_count, down_count, avg_latency_ms, min_latency_ms, max_latency_ms
		FROM `+rollupTable(bucket)+`
		WHERE monitor_id = $1 AND bucket_at >= $2 AND bucket_at <= $3
		ORDER BY bucket_at ASC`, monitorID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rollup
	for rows.Next() {
		var (
			r        Rollup
			bucketAt time.Time
			avg      pgtype.Int4
			min      pgtype.Int4
			max      pgtype.Int4
		)
		if err := rows.Scan(&r.MonitorID, &bucketAt, &r.Up, &r.Down, &avg, &min, &max); err != nil {
			return nil, err
		}
		r.BucketAt = bucketAt.UTC()
		r.AvgLatency = int(avg.Int32)
		r.MinLatency = int(min.Int32)
		r.MaxLatency = int(max.Int32)
		out = append(out, r)
	}
	return out, rows.Err()
}
