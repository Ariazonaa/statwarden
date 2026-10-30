package api

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// --- incidents ---

const incidentCols = `id, status_page_id, title, body, severity, status, pinned, started_at, resolved_at, created_by, postmortem, postmortem_published_at, created_at, updated_at`

func scanIncident(row pgx.Row) (Incident, error) {
	var in Incident
	var resolved *time.Time
	var createdBy *int64
	var postmortem *string
	err := row.Scan(&in.ID, &in.StatusPageID, &in.Title, &in.Body, &in.Severity, &in.Status,
		&in.Pinned, &in.StartedAt, &resolved, &createdBy,
		&postmortem, &in.PostmortemPublishedAt, &in.CreatedAt, &in.UpdatedAt)
	if err != nil {
		return Incident{}, err
	}
	in.ResolvedAt = resolved
	in.CreatedBy = createdBy
	in.Postmortem = strDeref(postmortem)
	return in, nil
}

func (s *PgStore) CreateIncident(ctx context.Context, in Incident) (Incident, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Incident{}, mapErr(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	out, err := scanIncident(tx.QueryRow(ctx, `
		INSERT INTO incidents (status_page_id, title, body, severity, status, pinned, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING `+incidentCols,
		in.StatusPageID, in.Title, in.Body, in.Severity, in.Status, in.Pinned, in.CreatedBy))
	if err != nil {
		return Incident{}, mapErr(err)
	}
	if err := replaceIncidentMonitors(ctx, tx, out.ID, in.MonitorIDs); err != nil {
		return Incident{}, mapErr(err)
	}
	// Seed the timeline with the opening update.
	if _, err := tx.Exec(ctx, `INSERT INTO incident_updates (incident_id, status, body, created_by) VALUES ($1,$2,$3,$4)`,
		out.ID, in.Status, in.Body, in.CreatedBy); err != nil {
		return Incident{}, mapErr(err)
	}
	if err := mapErr(tx.Commit(ctx)); err != nil {
		return Incident{}, err
	}
	return s.GetIncident(ctx, out.ID)
}

func (s *PgStore) ListIncidents(ctx context.Context, pageID int64) ([]Incident, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+incidentCols+` FROM incidents WHERE status_page_id = $1 ORDER BY pinned DESC, started_at DESC`, pageID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []Incident{}
	for rows.Next() {
		in, err := scanIncident(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err)
	}
	for i := range out {
		if err := s.loadIncidentDetail(ctx, &out[i]); err != nil {
			return nil, mapErr(err)
		}
	}
	return out, nil
}

func (s *PgStore) GetIncident(ctx context.Context, id int64) (Incident, error) {
	in, err := scanIncident(s.pool.QueryRow(ctx, `SELECT `+incidentCols+` FROM incidents WHERE id = $1`, id))
	if err != nil {
		return Incident{}, mapErr(err)
	}
	return in, mapErr(s.loadIncidentDetail(ctx, &in))
}

func (s *PgStore) loadIncidentDetail(ctx context.Context, in *Incident) error {
	// Affected monitors. Empty, never nil: an incident that names no specific
	// monitor is the common case, and this is served inside the public status
	// page payload where a nil renders as null.
	in.MonitorIDs = []int64{}
	rows, err := s.pool.Query(ctx, `SELECT monitor_id FROM incident_monitors WHERE incident_id = $1 ORDER BY monitor_id`, in.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var mid int64
		if err := rows.Scan(&mid); err != nil {
			rows.Close()
			return err
		}
		in.MonitorIDs = append(in.MonitorIDs, mid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// Timeline. Empty rather than nil for the same reason as MonitorIDs above,
	// and stated rather than relied upon: CreateIncident seeds an opening update
	// in the same transaction and nothing deletes one, so this is not reachable
	// today — it is one deletion path away from being the fourth instance of a
	// bug this codebase has already shipped three times.
	in.Updates = []IncidentUpdate{}
	urows, err := s.pool.Query(ctx, `SELECT id, incident_id, status, body, created_by, created_at FROM incident_updates WHERE incident_id = $1 ORDER BY created_at, id`, in.ID)
	if err != nil {
		return err
	}
	defer urows.Close()
	for urows.Next() {
		var u IncidentUpdate
		var createdBy *int64
		if err := urows.Scan(&u.ID, &u.IncidentID, &u.Status, &u.Body, &createdBy, &u.CreatedAt); err != nil {
			return err
		}
		u.CreatedBy = createdBy
		in.Updates = append(in.Updates, u)
	}
	return urows.Err()
}

func (s *PgStore) UpdateIncident(ctx context.Context, id int64, in Incident) (Incident, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Incident{}, mapErr(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if _, err := scanIncident(tx.QueryRow(ctx, `
		UPDATE incidents SET title=$2, body=$3, severity=$4, status=$5, pinned=$6, resolved_at=$7, updated_at=now()
		WHERE id=$1 RETURNING `+incidentCols,
		id, in.Title, in.Body, in.Severity, in.Status, in.Pinned, in.ResolvedAt)); err != nil {
		return Incident{}, mapErr(err)
	}
	if err := replaceIncidentMonitors(ctx, tx, id, in.MonitorIDs); err != nil {
		return Incident{}, mapErr(err)
	}
	if err := mapErr(tx.Commit(ctx)); err != nil {
		return Incident{}, err
	}
	return s.GetIncident(ctx, id)
}

// SetIncidentPostmortem is the only writer of the two postmortem columns; see
// the Store interface for why UpdateIncident must not touch them.
//
// An empty body stores NULL rather than an empty string, so "no retrospective"
// has one representation in the column instead of two that read differently in
// SQL.
func (s *PgStore) SetIncidentPostmortem(ctx context.Context, id int64, body string, publishedAt *time.Time) (Incident, error) {
	if _, err := scanIncident(s.pool.QueryRow(ctx, `
		UPDATE incidents SET postmortem = $2, postmortem_published_at = $3, updated_at = now()
		WHERE id = $1 RETURNING `+incidentCols,
		id, nilIfEmpty(body), publishedAt)); err != nil {
		return Incident{}, mapErr(err)
	}
	// Re-read through GetIncident so the caller gets the monitor ids and the
	// timeline too: the RETURNING row above carries neither, and this result is
	// serialised straight back to the client.
	return s.GetIncident(ctx, id)
}

func (s *PgStore) DeleteIncident(ctx context.Context, id int64) error {
	ct, err := s.pool.Exec(ctx, `DELETE FROM incidents WHERE id = $1`, id)
	return affected(ct, err)
}

func (s *PgStore) AddIncidentUpdate(ctx context.Context, u IncidentUpdate) (IncidentUpdate, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return IncidentUpdate{}, mapErr(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	var out IncidentUpdate
	var createdBy *int64
	err = tx.QueryRow(ctx, `INSERT INTO incident_updates (incident_id, status, body, created_by)
		VALUES ($1,$2,$3,$4) RETURNING id, incident_id, status, body, created_by, created_at`,
		u.IncidentID, u.Status, u.Body, u.CreatedBy).Scan(&out.ID, &out.IncidentID, &out.Status, &out.Body, &createdBy, &out.CreatedAt)
	if err != nil {
		return IncidentUpdate{}, mapErr(err)
	}
	out.CreatedBy = createdBy
	// Advance the incident status; stamp resolved_at on resolution and CLEAR it
	// on anything else.
	//
	// Clearing was missing, and the direct-edit path (handleUpdateIncident) has
	// always done it — so the field's value depended on which endpoint the
	// operator happened to use. Posting "investigating" on a resolved incident
	// left resolved_at pointing at the earlier close, and the incident then rode
	// in active_incidents carrying a resolution time. The post-mortem template
	// reads both and printed "Resolved: 09:00" plus a computed Duration for an
	// outage that was still running.
	resolvedClause := `, resolved_at = NULL`
	if u.Status == IncidentResolved {
		resolvedClause = `, resolved_at = COALESCE(resolved_at, now())`
	}
	if _, err := tx.Exec(ctx, `UPDATE incidents SET status=$2, updated_at=now()`+resolvedClause+` WHERE id=$1`, u.IncidentID, u.Status); err != nil {
		return IncidentUpdate{}, mapErr(err)
	}
	return out, mapErr(tx.Commit(ctx))
}

func replaceIncidentMonitors(ctx context.Context, tx pgx.Tx, id int64, monitorIDs []int64) error {
	if _, err := tx.Exec(ctx, `DELETE FROM incident_monitors WHERE incident_id = $1`, id); err != nil {
		return err
	}
	for _, mid := range monitorIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO incident_monitors (incident_id, monitor_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, id, mid); err != nil {
			return err
		}
	}
	return nil
}

// --- subscribers ---

const subscriberCols = `id, status_page_id, email, verified, verify_token, unsubscribe_token, created_at, verified_at`

func scanSubscriber(row pgx.Row) (Subscriber, error) {
	var sub Subscriber
	var verifyToken *string
	var verifiedAt *time.Time
	err := row.Scan(&sub.ID, &sub.StatusPageID, &sub.Email, &sub.Verified, &verifyToken, &sub.UnsubscribeToken, &sub.CreatedAt, &verifiedAt)
	if err != nil {
		return Subscriber{}, err
	}
	sub.VerifyToken = strDeref(verifyToken)
	sub.VerifiedAt = verifiedAt
	return sub, nil
}

func (s *PgStore) CreateSubscriber(ctx context.Context, sub Subscriber) (Subscriber, error) {
	out, err := scanSubscriber(s.pool.QueryRow(ctx, `
		INSERT INTO status_page_subscribers (status_page_id, email, verified, verify_token, unsubscribe_token, verify_sent_at)
		VALUES ($1,$2,$3,$4,$5, now()) RETURNING `+subscriberCols,
		sub.StatusPageID, sub.Email, sub.Verified, nilIfEmpty(sub.VerifyToken), sub.UnsubscribeToken))
	return out, mapErr(err)
}

func (s *PgStore) ListSubscribers(ctx context.Context, pageID int64, verifiedOnly bool) ([]Subscriber, error) {
	q := `SELECT ` + subscriberCols + ` FROM status_page_subscribers WHERE status_page_id = $1`
	if verifiedOnly {
		q += ` AND verified = true`
	}
	q += ` ORDER BY id`
	rows, err := s.pool.Query(ctx, q, pageID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []Subscriber{}
	for rows.Next() {
		sub, err := scanSubscriber(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, mapErr(rows.Err())
}

func (s *PgStore) GetSubscriberByVerifyToken(ctx context.Context, token string) (Subscriber, error) {
	out, err := scanSubscriber(s.pool.QueryRow(ctx, `SELECT `+subscriberCols+` FROM status_page_subscribers WHERE verify_token = $1`, token))
	return out, mapErr(err)
}

func (s *PgStore) GetSubscriberByUnsubscribeToken(ctx context.Context, token string) (Subscriber, error) {
	out, err := scanSubscriber(s.pool.QueryRow(ctx, `SELECT `+subscriberCols+` FROM status_page_subscribers WHERE unsubscribe_token = $1`, token))
	return out, mapErr(err)
}

func (s *PgStore) VerifySubscriber(ctx context.Context, id int64) (Subscriber, error) {
	out, err := scanSubscriber(s.pool.QueryRow(ctx, `
		UPDATE status_page_subscribers SET verified = true, verify_token = NULL, verified_at = now()
		WHERE id = $1 RETURNING `+subscriberCols, id))
	return out, mapErr(err)
}

func (s *PgStore) DeleteSubscriber(ctx context.Context, id int64) error {
	ct, err := s.pool.Exec(ctx, `DELETE FROM status_page_subscribers WHERE id = $1`, id)
	return affected(ct, err)
}
