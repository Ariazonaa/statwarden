package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/push"
)

// reconcilePushToken decides the value of monitors.push_token_hash for a monitor
// of the given type. The token is a heartbeat credential, so it is stored like a
// password: only its hash is persisted, and the raw token is revealed once.
//
// currentHash is the hash already on the row (nil/"" when none). It returns:
//   - storeHash: the value to write — a fresh hash when a token is minted, the
//     existing hash when a push monitor keeps its token, and nil (revoking any
//     stored secret) for every non-push type.
//   - plaintext: the freshly minted raw token, non-empty ONLY when a new token
//     was generated, so the caller can reveal it once. Empty when the existing
//     token is kept or the monitor carries none — a kept token cannot be
//     revealed again because its plaintext was never stored.
func reconcilePushToken(monitorType string, currentHash *string) (storeHash *string, plaintext string, err error) {
	if monitorType != "push" {
		return nil, "", nil
	}
	if currentHash != nil && *currentHash != "" {
		return currentHash, "", nil
	}
	tok, err := push.GenerateToken()
	if err != nil {
		return nil, "", err
	}
	h := push.HashToken(tok)
	return &h, tok, nil
}

// --- monitors ---

const monitorCols = `id, name, type, config, interval_seconds, timeout_seconds, max_retries,
	retry_interval_seconds, resend_interval_minutes, upside_down, active, status,
	parent_id, group_id, push_token_hash, description, link_url, created_at, recovery_checks`

// recoveryChecksOrDefault maps an unset (0) RecoveryChecks to the column's
// floor of 1. Callers that predate the field — the Kuma import, an old config
// export, a client that omits it — send nothing, and "one success recovers" is
// what they meant; the CHECK constraint would reject a literal 0.
func recoveryChecksOrDefault(n int) int {
	return max(n, 1)
}

func scanMonitor(row pgx.Row) (domain.Monitor, error) {
	var m domain.Monitor
	var config []byte
	var timeout *int32
	var interval, retryInterval int32
	var resendMinutes, maxRetries, recoveryChecks int32
	var parentID, groupID *int64
	var pushToken *string
	err := row.Scan(&m.ID, &m.Name, &m.Type, &config, &interval, &timeout, &maxRetries,
		&retryInterval, &resendMinutes, &m.UpsideDown, &m.Active, &m.Status,
		&parentID, &groupID, &pushToken, &m.Description, &m.LinkURL, &m.CreatedAt, &recoveryChecks)
	if err != nil {
		return domain.Monitor{}, err
	}
	m.Config = json.RawMessage(config)
	m.Interval = time.Duration(interval) * time.Second
	m.MaxRetries = int(maxRetries)
	m.RecoveryChecks = int(recoveryChecks)
	m.RetryInterval = time.Duration(retryInterval) * time.Second
	m.ResendInterval = time.Duration(resendMinutes) * time.Minute
	if timeout != nil {
		m.Timeout = time.Duration(*timeout) * time.Second
	}
	m.ParentID = parentID
	m.GroupID = groupID
	if pushToken != nil {
		m.PushToken = *pushToken
	}
	return m, nil
}

func (s *PgStore) CreateMonitor(ctx context.Context, in MonitorInput) (domain.Monitor, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Monitor{}, mapErr(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	status := string(domain.StatusPending)
	if !in.Active {
		status = string(domain.StatusPaused)
	}
	// A push monitor is useless without a token, so mint one atomically as part
	// of the create transaction rather than leaving it to a later regenerate.
	// Only the hash is stored; the raw token is revealed once through the create
	// response below.
	pushHash, pushPlain, err := reconcilePushToken(in.Type, nil)
	if err != nil {
		return domain.Monitor{}, mapErr(err)
	}
	row := tx.QueryRow(ctx, `
		INSERT INTO monitors (name, type, config, interval_seconds, timeout_seconds, max_retries,
			retry_interval_seconds, resend_interval_minutes, upside_down, active, status,
			parent_id, group_id, description, link_url, push_token_hash, created_by, recovery_checks)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
		RETURNING `+monitorCols,
		in.Name, in.Type, jsonOrEmpty(in.Config), in.Interval, in.Timeout, in.MaxRetries,
		in.RetryInterval, in.ResendInterval, in.UpsideDown, in.Active, status,
		in.ParentID, in.GroupID, in.Description, in.LinkURL, pushHash, in.CreatedBy,
		recoveryChecksOrDefault(in.RecoveryChecks))
	m, err := scanMonitor(row)
	if err != nil {
		return domain.Monitor{}, mapErr(err)
	}
	if err := replaceMonitorLinks(ctx, tx, m.ID, in); err != nil {
		return domain.Monitor{}, mapErr(err)
	}
	// scanMonitor filled PushToken from the hash column; overwrite it with the
	// freshly minted plaintext so the create response can reveal it this once
	// (empty for non-push monitors, which carry no token).
	m.PushToken = pushPlain
	return m, mapErr(tx.Commit(ctx))
}

// liveMonitor is the predicate every ordinary monitor read carries: a monitor
// in the bin is not found, not "found but flagged". Callers all the way up to
// the public status page treat ErrNotFound as "skip this", and a deleted monitor
// leaking through as a nameless pending row would be worse than either.
//
// Written out as a constant so a new query cannot forget it silently — a missing
// filter here is invisible in the Go suite, which runs against MemStore.
//
// "Ordinary" is doing work in that first sentence. Three kinds of read do NOT
// carry it, each on purpose: the bin endpoints, which exist to see exactly these
// rows; PurgeMonitor and RestoreMonitor, which require them; and
// parentCycleInTx, which walks the stored parent graph. That last one is the
// subtle one — it must see binned ancestors, because a chain that loops through
// one is still a loop the moment that monitor is restored, and a cycle
// introduced now would only be discovered then.
const liveMonitor = ` AND deleted_at IS NULL`

func (s *PgStore) GetMonitor(ctx context.Context, id int64) (domain.Monitor, error) {
	m, err := scanMonitor(s.pool.QueryRow(ctx, `SELECT `+monitorCols+` FROM monitors WHERE id = $1`+liveMonitor, id))
	return m, mapErr(err)
}

// GetDeletedMonitor reads a monitor FROM the bin. It is the mirror of GetMonitor
// and the only read that admits one, so restore and purge can confirm what they
// are about to act on without every other caller gaining the ability to see it.
func (s *PgStore) GetDeletedMonitor(ctx context.Context, id int64) (domain.Monitor, error) {
	m, err := scanMonitor(s.pool.QueryRow(ctx,
		`SELECT `+monitorCols+` FROM monitors WHERE id = $1 AND deleted_at IS NOT NULL`, id))
	return m, mapErr(err)
}

func (s *PgStore) ListMonitors(ctx context.Context, f MonitorFilter, p Page) ([]domain.Monitor, int, error) {
	// Seeded with the bin filter rather than the old "1=1" placeholder: every
	// list, count and filter combination inherits it, and there is no branch
	// where it can be left off.
	where := []string{"deleted_at IS NULL"}
	args := []any{}
	add := func(cond string, val any) {
		args = append(args, val)
		where = append(where, cond+" $"+itoa(len(args)))
	}
	if f.Status != "" {
		add("status =", f.Status)
	}
	if f.Type != "" {
		add("type =", f.Type)
	}
	if f.Search != "" {
		add("name ILIKE", "%"+escapeLikePattern(f.Search)+"%")
	}
	if f.TagID != nil {
		add("id IN (SELECT monitor_id FROM monitor_tags WHERE tag_id =", *f.TagID)
		where[len(where)-1] += ")"
	}
	if f.GroupID != nil {
		// GroupUnassigned asks for the unfiled monitors, which is an IS NULL
		// test rather than an equality one — group_id = NULL matches nothing.
		if *f.GroupID == GroupUnassigned {
			where = append(where, "group_id IS NULL")
		} else {
			add("group_id =", *f.GroupID)
		}
	}
	cond := strings.Join(where, " AND ")

	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM monitors WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, mapErr(err)
	}

	args = append(args, p.Limit, p.Offset)
	q := `SELECT ` + monitorCols + ` FROM monitors WHERE ` + cond +
		` ORDER BY id LIMIT $` + itoa(len(args)-1) + ` OFFSET $` + itoa(len(args))
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, mapErr(err)
	}
	defer rows.Close()
	out := []domain.Monitor{}
	for rows.Next() {
		m, err := scanMonitor(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, m)
	}
	return out, total, rows.Err()
}

func (s *PgStore) UpdateMonitor(ctx context.Context, id int64, in MonitorInput) (domain.Monitor, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Monitor{}, mapErr(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// Serialise parent changes across the whole table.
	//
	// The API validates the parent chain before calling, but that check and
	// this write are separate operations, and the row lock below only covers
	// the monitor being edited. Two requests — A.parent=B and B.parent=A —
	// therefore both validated against a cycle-free graph and both committed,
	// leaving a cycle in the database. A cycle is not a cosmetic problem here:
	// the suppression walk that decides "is this monitor down only because its
	// parent is" follows these edges.
	//
	// A transaction-scoped advisory lock is the cheap fix. Parent changes are
	// rare, the lock is held for the length of one UPDATE, and it makes the
	// re-check below authoritative: whoever gets in second sees the other's
	// committed state.
	if in.ParentID != nil {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('statwarden.monitor_parent_graph'))`); err != nil {
			return domain.Monitor{}, mapErr(err)
		}
		if err := parentCycleInTx(ctx, tx, id, *in.ParentID); err != nil {
			return domain.Monitor{}, err
		}
	}

	// The row lock below already refuses a binned monitor and returns, so the
	// UPDATE further down carries the filter redundantly — deliberately. A
	// full-replace write guarded only by an earlier statement in the same
	// function is one reordering away from resurrecting a deleted monitor.
	//
	// Reconcile the push token with the (possibly changed) type under a row
	// lock: a monitor that becomes a push monitor is minted a token, one that
	// leaves the push type has its token revoked, and an existing token is kept.
	// Only the hash is read and written; a token minted here is not revealed on
	// update (the read path redacts it) and must be fetched via regenerate.
	var curHash *string
	if err := tx.QueryRow(ctx, `SELECT push_token_hash FROM monitors WHERE id = $1`+liveMonitor+` FOR UPDATE`, id).Scan(&curHash); err != nil {
		return domain.Monitor{}, mapErr(err)
	}
	pushHash, _, err := reconcilePushToken(in.Type, curHash)
	if err != nil {
		return domain.Monitor{}, mapErr(err)
	}

	// status follows active, exactly as SetMonitorActive does.
	//
	// It was missing from this statement, the only writer of the active column
	// that left it out. handleUpdateMonitor then calls engine.Remove, so nothing
	// ever rewrote the column again: a monitor paused through a full-replace PUT
	// went on reading "up" on the dashboard, the badge and the public status page
	// indefinitely. MemStore mirrored the omission, so no test could see it.
	//
	// Only the deactivating direction is forced. Activating leaves the stored
	// status alone, so a resumed monitor keeps its last known state until its
	// first check lands rather than flashing "pending".
	row := tx.QueryRow(ctx, `
		UPDATE monitors SET name=$2, type=$3, config=$4, interval_seconds=$5, timeout_seconds=$6,
			max_retries=$7, retry_interval_seconds=$8, resend_interval_minutes=$9, upside_down=$10,
			active=$11, parent_id=$12, group_id=$13, description=$14, link_url=$15, push_token_hash=$16,
			recovery_checks=$17,
			status = CASE WHEN $11 THEN status ELSE 'paused' END,
			updated_at=now()
		WHERE id=$1`+liveMonitor+`
		RETURNING `+monitorCols,
		id, in.Name, in.Type, jsonOrEmpty(in.Config), in.Interval, in.Timeout,
		in.MaxRetries, in.RetryInterval, in.ResendInterval, in.UpsideDown,
		in.Active, in.ParentID, in.GroupID, in.Description, in.LinkURL, pushHash,
		recoveryChecksOrDefault(in.RecoveryChecks))
	m, err := scanMonitor(row)
	if err != nil {
		return domain.Monitor{}, mapErr(err)
	}
	if err := replaceMonitorLinks(ctx, tx, id, in); err != nil {
		return domain.Monitor{}, mapErr(err)
	}
	return m, mapErr(tx.Commit(ctx))
}

// DeleteMonitor moves a monitor to the bin. See migration 0018 for why this is
// reversible at all, and why it also clears `active`.
//
// `active = false` is not bookkeeping: the scheduler, the push recorder and the
// alert dispatcher all select on it, so clearing it is what takes a binned
// monitor out of every one of them without teaching any of them about
// deleted_at. `status` follows to 'paused' so the row does not sit in the bin
// claiming to be UP.
//
// Deleting an already-binned monitor is ErrNotFound, not a silent success: the
// caller asked to remove something that, as far as every read is concerned, is
// not there.
func (s *PgStore) DeleteMonitor(ctx context.Context, id int64) error {
	ct, err := s.pool.Exec(ctx, `
		UPDATE monitors SET deleted_at = now(), active = false, status = $2, updated_at = now()
		WHERE id = $1`+liveMonitor, id, string(domain.StatusPaused))
	return affected(ct, err)
}

// RestoreMonitor brings a monitor back, PAUSED.
//
// Paused because the column cannot say what `active` was before the delete, and
// guessing "on" would restart checks and alerts for something that has been gone
// for a week — at the moment somebody is only trying to look at its history.
// Resuming it is one more deliberate click.
func (s *PgStore) RestoreMonitor(ctx context.Context, id int64) (domain.Monitor, error) {
	m, err := scanMonitor(s.pool.QueryRow(ctx, `
		UPDATE monitors SET deleted_at = NULL, active = false, status = $2, updated_at = now()
		WHERE id = $1 AND deleted_at IS NOT NULL
		RETURNING `+monitorCols, id, string(domain.StatusPaused)))
	return m, mapErr(err)
}

// PurgeMonitor removes a binned monitor for good, taking its heartbeats with it
// through the schema's ON DELETE CASCADE. It refuses a LIVE monitor: the only
// route to a hard delete is through the bin, so there is always a window in
// which a mistake can be undone.
func (s *PgStore) PurgeMonitor(ctx context.Context, id int64) error {
	ct, err := s.pool.Exec(ctx, `DELETE FROM monitors WHERE id = $1 AND deleted_at IS NOT NULL`, id)
	return affected(ct, err)
}

// ListDeletedMonitors returns the bin, most recently deleted first.
func (s *PgStore) ListDeletedMonitors(ctx context.Context) ([]domain.Monitor, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+monitorCols+` FROM monitors WHERE deleted_at IS NOT NULL ORDER BY deleted_at DESC, id DESC`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []domain.Monitor{}
	for rows.Next() {
		m, err := scanMonitor(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, mapErr(rows.Err())
}

func (s *PgStore) SetMonitorActive(ctx context.Context, id int64, active bool) error {
	status := string(domain.StatusPending)
	if !active {
		status = string(domain.StatusPaused)
	}
	ct, err := s.pool.Exec(ctx, `UPDATE monitors SET active = $2, status = $3, updated_at = now() WHERE id = $1`+liveMonitor, id, active, status)
	return affected(ct, err)
}

func (s *PgStore) SetMonitorGroup(ctx context.Context, id int64, groupID *int64) error {
	ct, err := s.pool.Exec(ctx, `UPDATE monitors SET group_id = $2, updated_at = now() WHERE id = $1`+liveMonitor, id, groupID)
	return affected(ct, err)
}

// AddMonitorTags attaches tags without disturbing the ones already present.
// The insert is a single statement — atomic on its own — and ON CONFLICT DO
// NOTHING makes re-tagging an already-tagged monitor a no-op rather than an
// error, so a bulk tag over a mixed selection succeeds for every monitor.
//
// The monitor existence check is separate and deliberate: without it, an
// unknown monitor id would insert zero rows and look like success, and the bulk
// endpoint would report a deleted monitor as tagged.
func (s *PgStore) AddMonitorTags(ctx context.Context, id int64, tagIDs []int64) error {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT true FROM monitors WHERE id = $1`+liveMonitor, id).Scan(&exists); err != nil {
		return mapErr(err)
	}
	if len(tagIDs) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO monitor_tags (monitor_id, tag_id)
		SELECT $1, t FROM unnest($2::bigint[]) AS t
		ON CONFLICT DO NOTHING`, id, tagIDs)
	return mapErr(err)
}

// AddMonitorNotifications attaches notification channels without disturbing the
// ones already present, mirroring AddMonitorTags exactly: one ON CONFLICT DO
// NOTHING insert (atomic on its own, and idempotent so re-attaching a channel a
// monitor already has is a no-op rather than an error).
//
// The existence check is separate and deliberate for the same reason as in
// AddMonitorTags: without it an unknown monitor id would insert zero rows and
// look like success, and the bulk endpoint would report a deleted monitor as
// notified.
func (s *PgStore) AddMonitorNotifications(ctx context.Context, id int64, notificationIDs []int64) error {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT true FROM monitors WHERE id = $1`+liveMonitor, id).Scan(&exists); err != nil {
		return mapErr(err)
	}
	if len(notificationIDs) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO monitor_notifications (monitor_id, notification_id)
		SELECT $1, n FROM unnest($2::bigint[]) AS n
		ON CONFLICT DO NOTHING`, id, notificationIDs)
	return mapErr(err)
}

// SetMonitorSchedule sets a monitor's check timing in one statement. The
// interval is always written; timeout, retries and retry interval use
// COALESCE($n, column) so a nil override keeps the stored value rather than
// nulling it — that is what makes a bulk schedule able to change only the
// interval. The single UPDATE is atomic, so the row never sits half-rewritten.
func (s *PgStore) SetMonitorSchedule(ctx context.Context, id int64, sched MonitorSchedule) error {
	ct, err := s.pool.Exec(ctx, `
		UPDATE monitors SET
			interval_seconds       = $2,
			timeout_seconds        = COALESCE($3, timeout_seconds),
			max_retries            = COALESCE($4, max_retries),
			retry_interval_seconds = COALESCE($5, retry_interval_seconds),
			updated_at             = now()
		WHERE id = $1`+liveMonitor,
		id, sched.Interval, sched.Timeout, sched.MaxRetries, sched.RetryInterval)
	return affected(ct, err)
}

// --- monitor groups ---

const monitorGroupCols = `id, name, sort_order, created_at`

func scanMonitorGroup(row pgx.Row) (MonitorGroup, error) {
	var g MonitorGroup
	err := row.Scan(&g.ID, &g.Name, &g.SortOrder, &g.CreatedAt)
	return g, err
}

func (s *PgStore) CreateMonitorGroup(ctx context.Context, name string, sortOrder int) (MonitorGroup, error) {
	g, err := scanMonitorGroup(s.pool.QueryRow(ctx,
		`INSERT INTO monitor_groups (name, sort_order) VALUES ($1, $2) RETURNING `+monitorGroupCols, name, sortOrder))
	return g, mapErr(err)
}

func (s *PgStore) ListMonitorGroups(ctx context.Context) ([]MonitorGroup, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+monitorGroupCols+` FROM monitor_groups ORDER BY sort_order, id`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []MonitorGroup{}
	for rows.Next() {
		g, err := scanMonitorGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *PgStore) GetMonitorGroup(ctx context.Context, id int64) (MonitorGroup, error) {
	g, err := scanMonitorGroup(s.pool.QueryRow(ctx, `SELECT `+monitorGroupCols+` FROM monitor_groups WHERE id = $1`, id))
	return g, mapErr(err)
}

func (s *PgStore) UpdateMonitorGroup(ctx context.Context, id int64, name string, sortOrder int) (MonitorGroup, error) {
	g, err := scanMonitorGroup(s.pool.QueryRow(ctx,
		`UPDATE monitor_groups SET name = $2, sort_order = $3 WHERE id = $1 RETURNING `+monitorGroupCols, id, name, sortOrder))
	return g, mapErr(err)
}

// DeleteMonitorGroup relies on monitors.group_id being ON DELETE SET NULL: the
// monitors filed in the group are unfiled by the database, not deleted.
func (s *PgStore) DeleteMonitorGroup(ctx context.Context, id int64) error {
	ct, err := s.pool.Exec(ctx, `DELETE FROM monitor_groups WHERE id = $1`, id)
	return affected(ct, err)
}

// SetPushToken implements pushTokenStore. The raw token is hashed before it is
// written: the row stores only push_token_hash, so a database dump yields no
// working heartbeat credential. The caller (regenerate) reveals the plaintext
// once from its own copy.
func (s *PgStore) SetPushToken(ctx context.Context, id int64, token string) error {
	ct, err := s.pool.Exec(ctx, `UPDATE monitors SET push_token_hash = $2, updated_at = now() WHERE id = $1`+liveMonitor, id, push.HashToken(token))
	return affected(ct, err)
}

// MonitorLinks reads every requested monitor's associations in two queries —
// one per join table — regardless of how many monitors were asked for.
func (s *PgStore) MonitorLinks(ctx context.Context, ids []int64) (map[int64]MonitorLinks, error) {
	out := make(map[int64]MonitorLinks, len(ids))
	for _, id := range ids {
		out[id] = MonitorLinks{TagIDs: []int64{}, NotificationIDs: []int64{}}
	}
	if len(ids) == 0 {
		return out, nil
	}
	collect := func(q string, assign func(l *MonitorLinks, id int64)) error {
		rows, err := s.pool.Query(ctx, q, ids)
		if err != nil {
			return mapErr(err)
		}
		defer rows.Close()
		for rows.Next() {
			var monitorID, linked int64
			if err := rows.Scan(&monitorID, &linked); err != nil {
				return err
			}
			l := out[monitorID]
			assign(&l, linked)
			out[monitorID] = l
		}
		return rows.Err()
	}
	if err := collect(
		`SELECT monitor_id, tag_id FROM monitor_tags WHERE monitor_id = ANY($1) ORDER BY monitor_id, tag_id`,
		func(l *MonitorLinks, id int64) { l.TagIDs = append(l.TagIDs, id) },
	); err != nil {
		return nil, err
	}
	if err := collect(
		`SELECT monitor_id, notification_id FROM monitor_notifications WHERE monitor_id = ANY($1) ORDER BY monitor_id, notification_id`,
		func(l *MonitorLinks, id int64) { l.NotificationIDs = append(l.NotificationIDs, id) },
	); err != nil {
		return nil, err
	}
	return out, nil
}

// MonitorStats summarises heartbeats for every requested monitor in three
// fixed queries — the uptime aggregates, the newest beat, and the sparkline
// window — never one query per monitor. That bound is the whole point: the
// dashboard list calls this once with a full page of ids.
//
// The uptime fractions count only conclusive (up/down) beats, mirroring
// report.Uptime, and are computed straight off the heartbeats table. Heartbeat
// retention therefore bounds uptime_1y: once a beat is pruned it stops
// counting. Reading the long windows from monitor_rollups instead would survive
// pruning and is the natural next step; it is not required to make the field
// exist, which is what was broken here.
func (s *PgStore) MonitorStats(ctx context.Context, ids []int64, now time.Time, scope StatsScope) (map[int64]MonitorStats, error) {
	out := make(map[int64]MonitorStats, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	// Uptime as up/total pairs per monitor.
	//
	// The OUTER bound is what this costs, not the FILTERs: an aggregate has to
	// read every row in the window, and the window used to be a year for every
	// request — including the monitor LIST, which renders 24 h and 30 d and has
	// never shown the yearly figure (MonitorCard.svelte). At ~49 000 beats per
	// monitor that was twelve times more rows than the answer needed, on every
	// dashboard load, multiplied by the number of monitors.
	//
	// StatsList therefore stops at 30 days and reports no yearly uptime at all
	// — a null the client already renders as "no data", and which it never asks
	// for in that view. StatsDetail keeps the full year for the one monitor
	// whose page actually shows it.
	var rows pgx.Rows
	var err error
	if scope == StatsDetail {
		rows, err = s.pool.Query(ctx, `
		SELECT monitor_id,
			count(*) FILTER (WHERE checked_at >= $2 AND status = 'up')   AS up_24h,
			count(*) FILTER (WHERE checked_at >= $2 AND status IN ('up','down')) AS tot_24h,
			count(*) FILTER (WHERE checked_at >= $3 AND status = 'up')   AS up_30d,
			count(*) FILTER (WHERE checked_at >= $3 AND status IN ('up','down')) AS tot_30d,
			count(*) FILTER (WHERE status = 'up')                        AS up_1y,
			count(*) FILTER (WHERE status IN ('up','down'))              AS tot_1y
		FROM heartbeats
		WHERE monitor_id = ANY($1) AND checked_at >= $4
		GROUP BY monitor_id`,
			ids, now.Add(-24*time.Hour), now.AddDate(0, 0, -30), now.AddDate(-1, 0, 0))
	} else {
		// The 30 d counters need no FILTER of their own here: the outer bound
		// IS thirty days, so every row read belongs to that window. up_1y and
		// tot_1y are selected as 0 so one scan path serves both scopes; ratio()
		// turns 0/0 into the same null an empty window produces.
		rows, err = s.pool.Query(ctx, `
		SELECT monitor_id,
			count(*) FILTER (WHERE checked_at >= $2 AND status = 'up')   AS up_24h,
			count(*) FILTER (WHERE checked_at >= $2 AND status IN ('up','down')) AS tot_24h,
			count(*) FILTER (WHERE status = 'up')                        AS up_30d,
			count(*) FILTER (WHERE status IN ('up','down'))              AS tot_30d,
			0 AS up_1y,
			0 AS tot_1y
		FROM heartbeats
		WHERE monitor_id = ANY($1) AND checked_at >= $3
		GROUP BY monitor_id`,
			ids, now.Add(-24*time.Hour), now.AddDate(0, 0, -30))
	}
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var up24, tot24, up30, tot30, up1y, tot1y int64
		if err := rows.Scan(&id, &up24, &tot24, &up30, &tot30, &up1y, &tot1y); err != nil {
			return nil, err
		}
		out[id] = MonitorStats{
			Uptime24h: ratio(up24, tot24),
			Uptime30d: ratio(up30, tot30),
			Uptime1y:  ratio(up1y, tot1y),
			Sparkline: []int{},
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Newest beat per monitor: latency and last-checked-at.
	//
	// LATERAL, for the same reason the sparkline below uses one — and this query
	// is the reason that fix only got halfway. It looked index-friendly:
	// DISTINCT ON with ORDER BY (monitor_id, checked_at DESC) matches the index
	// exactly. What it does NOT have is a bound, so the planner is free to read
	// every heartbeat of every requested monitor and sort, and on this data it
	// does: ~49 000 rows per monitor to return one.
	//
	// Measured on a live instance after the first round of fixes: 3 542 ms for
	// 39 monitors, ~91 ms each, still linear in history length. Narrowing the
	// uptime window and rewriting the sparkline had removed the two obvious
	// full scans; this was the third, and it had been dismissed on the strength
	// of how the SQL reads rather than what it costs.
	//
	// The LATERAL asks the index for one row per monitor and stops.
	latest, err := s.pool.Query(ctx, `
		SELECT m.id AS monitor_id, t.checked_at, t.latency_ms
		FROM unnest($1::bigint[]) AS m(id)
		CROSS JOIN LATERAL (
			SELECT checked_at, latency_ms
			FROM heartbeats
			WHERE monitor_id = m.id
			ORDER BY checked_at DESC
			LIMIT 1
		) t`, ids)
	if err != nil {
		return nil, mapErr(err)
	}
	defer latest.Close()
	for latest.Next() {
		var id int64
		var checkedAt time.Time
		var latencyMs *int32
		if err := latest.Scan(&id, &checkedAt, &latencyMs); err != nil {
			return nil, err
		}
		st := out[id]
		if st.Sparkline == nil {
			st.Sparkline = []int{}
		}
		at := checkedAt
		st.LastCheckedAt = &at
		if latencyMs != nil {
			ms := int(*latencyMs)
			st.LatencyMs = &ms
		}
		out[id] = st
	}
	if err := latest.Err(); err != nil {
		return nil, err
	}

	// Trailing latency series, oldest first, forty points per monitor.
	//
	// LATERAL, not row_number(). The window-function form had no time bound, so
	// PARTITION BY … ORDER BY checked_at DESC had to read and rank EVERY beat a
	// monitor ever recorded in order to keep the newest forty — roughly 49 000
	// rows to produce 40, per monitor, per request. The index on
	// (monitor_id, checked_at DESC) could not help, because a window function
	// cannot stop early.
	//
	// The LATERAL walks that same index per monitor and stops at LIMIT. Same
	// forty points, same oldest-first order, bounded work. A time bound would
	// have been the cheaper edit and the wrong one: a monitor on a six-hour
	// interval would silently return fewer points.
	spark, err := s.pool.Query(ctx, `
		SELECT m.id AS monitor_id, t.latency_ms
		FROM unnest($1::bigint[]) AS m(id)
		CROSS JOIN LATERAL (
			SELECT latency_ms, checked_at
			FROM heartbeats
			WHERE monitor_id = m.id AND latency_ms IS NOT NULL
			ORDER BY checked_at DESC
			LIMIT $2
		) t
		ORDER BY m.id, t.checked_at`, ids, sparklinePoints)
	if err != nil {
		return nil, mapErr(err)
	}
	defer spark.Close()
	for spark.Next() {
		var id int64
		var latencyMs int32
		if err := spark.Scan(&id, &latencyMs); err != nil {
			return nil, err
		}
		st := out[id]
		st.Sparkline = append(st.Sparkline, int(latencyMs))
		out[id] = st
	}
	if err := spark.Err(); err != nil {
		return nil, err
	}

	// Trailing status series for the per-check bar, oldest first. The same
	// bounded LATERAL walk as the sparkline, but WITHOUT the latency filter: a
	// DOWN beat usually carries no latency, and the bar exists to show it.
	// $1 ids, $2 limit — two placeholders, two args.
	recent, err := s.pool.Query(ctx, `
		SELECT m.id AS monitor_id, t.status
		FROM unnest($1::bigint[]) AS m(id)
		CROSS JOIN LATERAL (
			SELECT status, checked_at
			FROM heartbeats
			WHERE monitor_id = m.id
			ORDER BY checked_at DESC
			LIMIT $2
		) t
		ORDER BY m.id, t.checked_at`, ids, recentBeatPoints)
	if err != nil {
		return nil, mapErr(err)
	}
	defer recent.Close()
	for recent.Next() {
		var id int64
		var status string
		if err := recent.Scan(&id, &status); err != nil {
			return nil, err
		}
		st := out[id]
		if st.Sparkline == nil {
			st.Sparkline = []int{}
		}
		st.RecentBeats = append(st.RecentBeats, status)
		out[id] = st
	}
	return out, recent.Err()
}

// ratio is up/total as a 0..1 fraction, or nil when the window held no
// conclusive heartbeat — a null the client renders as "no data" rather than 0%.
func ratio(up, total int64) *float64 {
	if total == 0 {
		return nil
	}
	r := float64(up) / float64(total)
	return &r
}

// replaceMonitorLinks rewrites the monitor_tags and monitor_notifications
// association rows for a monitor.
func replaceMonitorLinks(ctx context.Context, tx pgx.Tx, monitorID int64, in MonitorInput) error {
	if _, err := tx.Exec(ctx, `DELETE FROM monitor_tags WHERE monitor_id = $1`, monitorID); err != nil {
		return err
	}
	for _, tagID := range in.TagIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO monitor_tags (monitor_id, tag_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, monitorID, tagID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM monitor_notifications WHERE monitor_id = $1`, monitorID); err != nil {
		return err
	}
	for _, nid := range in.NotificationIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO monitor_notifications (monitor_id, notification_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, monitorID, nid); err != nil {
			return err
		}
	}
	return nil
}

// --- tags ---

func (s *PgStore) CreateTag(ctx context.Context, name, color string) (Tag, error) {
	var t Tag
	err := s.pool.QueryRow(ctx, `INSERT INTO tags (name, color) VALUES ($1, $2) RETURNING id, name, color, created_at`, name, color).
		Scan(&t.ID, &t.Name, &t.Color, &t.CreatedAt)
	return t, mapErr(err)
}

func (s *PgStore) ListTags(ctx context.Context) ([]Tag, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name, color, created_at FROM tags ORDER BY id`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []Tag{}
	for rows.Next() {
		var t Tag
		if err := rows.Scan(&t.ID, &t.Name, &t.Color, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *PgStore) GetTag(ctx context.Context, id int64) (Tag, error) {
	var t Tag
	err := s.pool.QueryRow(ctx, `SELECT id, name, color, created_at FROM tags WHERE id = $1`, id).
		Scan(&t.ID, &t.Name, &t.Color, &t.CreatedAt)
	return t, mapErr(err)
}

func (s *PgStore) UpdateTag(ctx context.Context, id int64, name, color string) (Tag, error) {
	var t Tag
	err := s.pool.QueryRow(ctx, `UPDATE tags SET name = $2, color = $3 WHERE id = $1 RETURNING id, name, color, created_at`, id, name, color).
		Scan(&t.ID, &t.Name, &t.Color, &t.CreatedAt)
	return t, mapErr(err)
}

func (s *PgStore) DeleteTag(ctx context.Context, id int64) error {
	ct, err := s.pool.Exec(ctx, `DELETE FROM tags WHERE id = $1`, id)
	return affected(ct, err)
}

// --- notifications ---

const notifCols = `id, name, type, config, is_default, active, created_at, updated_at`

func scanNotification(row pgx.Row) (Notification, error) {
	var n Notification
	var config []byte
	err := row.Scan(&n.ID, &n.Name, &n.Type, &config, &n.IsDefault, &n.Active, &n.CreatedAt, &n.UpdatedAt)
	n.Config = json.RawMessage(config)
	return n, err
}

func (s *PgStore) CreateNotification(ctx context.Context, n Notification) (Notification, error) {
	out, err := scanNotification(s.pool.QueryRow(ctx, `
		INSERT INTO notifications (name, type, config, is_default, active)
		VALUES ($1, $2, $3, $4, $5) RETURNING `+notifCols,
		n.Name, n.Type, jsonOrEmpty(n.Config), n.IsDefault, n.Active))
	return out, mapErr(err)
}

func (s *PgStore) ListNotifications(ctx context.Context) ([]Notification, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+notifCols+` FROM notifications ORDER BY id`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []Notification{}
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *PgStore) GetNotification(ctx context.Context, id int64) (Notification, error) {
	n, err := scanNotification(s.pool.QueryRow(ctx, `SELECT `+notifCols+` FROM notifications WHERE id = $1`, id))
	return n, mapErr(err)
}

func (s *PgStore) UpdateNotification(ctx context.Context, id int64, n Notification) (Notification, error) {
	out, err := scanNotification(s.pool.QueryRow(ctx, `
		UPDATE notifications SET name=$2, type=$3, config=$4, is_default=$5, active=$6, updated_at=now()
		WHERE id=$1 RETURNING `+notifCols,
		id, n.Name, n.Type, jsonOrEmpty(n.Config), n.IsDefault, n.Active))
	return out, mapErr(err)
}

func (s *PgStore) DeleteNotification(ctx context.Context, id int64) error {
	ct, err := s.pool.Exec(ctx, `DELETE FROM notifications WHERE id = $1`, id)
	return affected(ct, err)
}

// --- settings ---

func (s *PgStore) GetSetting(ctx context.Context, key string) (Setting, error) {
	var st Setting
	var value []byte
	err := s.pool.QueryRow(ctx, `SELECT key, value, updated_at FROM settings WHERE key = $1`, key).
		Scan(&st.Key, &value, &st.UpdatedAt)
	st.Value = json.RawMessage(value)
	return st, mapErr(err)
}

func (s *PgStore) ListSettings(ctx context.Context) ([]Setting, error) {
	rows, err := s.pool.Query(ctx, `SELECT key, value, updated_at FROM settings ORDER BY key`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []Setting{}
	for rows.Next() {
		var st Setting
		var value []byte
		if err := rows.Scan(&st.Key, &value, &st.UpdatedAt); err != nil {
			return nil, err
		}
		st.Value = json.RawMessage(value)
		out = append(out, st)
	}
	return out, rows.Err()
}

func (s *PgStore) SetSetting(ctx context.Context, key string, value []byte) (Setting, error) {
	var st Setting
	var stored []byte
	err := s.pool.QueryRow(ctx, `
		INSERT INTO settings (key, value, updated_at) VALUES ($1, $2, now())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()
		RETURNING key, value, updated_at`, key, jsonOrEmpty(value)).
		Scan(&st.Key, &stored, &st.UpdatedAt)
	st.Value = json.RawMessage(stored)
	return st, mapErr(err)
}

// --- audit ---

func (s *PgStore) InsertAudit(ctx context.Context, e AuditEntry) (AuditEntry, error) {
	var entityType *string
	if e.EntityType != "" {
		entityType = &e.EntityType
	}
	// RETURNING id, created_at rather than a bare Exec: the caller forwards the
	// entry to the SIEM feed, where the row id is the collector's deduplication
	// key, and created_at comes back because the column has a now() default the
	// caller's clock does not necessarily match.
	err := s.pool.QueryRow(ctx, `
		INSERT INTO audit_log (user_id, action, entity_type, entity_id, details, ip, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, created_at`,
		e.UserID, e.Action, entityType, e.EntityID, jsonOrNil(e.Details), ipPtr(e.IP), e.CreatedAt).
		Scan(&e.ID, &e.CreatedAt)
	if err != nil {
		return AuditEntry{}, mapErr(err)
	}
	return e, nil
}

func (s *PgStore) ListAudit(ctx context.Context, p Page) ([]AuditEntry, int, error) {
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM audit_log`).Scan(&total); err != nil {
		return nil, 0, mapErr(err)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+auditCols+`
		FROM audit_log ORDER BY created_at DESC, id DESC LIMIT $1 OFFSET $2`, p.Limit, p.Offset)
	if err != nil {
		return nil, 0, mapErr(err)
	}
	out, err := scanAuditRows(rows)
	if err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

const auditCols = `id, user_id, action, entity_type, entity_id, details, ip, created_at`

// scanAuditRows drains rows into entries and closes them. Both audit reads
// select the same columns in the same order, so they scan them the same way.
func scanAuditRows(rows pgx.Rows) ([]AuditEntry, error) {
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var entityType *string
		var details []byte
		var ip *netip.Addr
		if err := rows.Scan(&e.ID, &e.UserID, &e.Action, &entityType, &e.EntityID, &details, &ip, &e.CreatedAt); err != nil {
			return nil, err
		}
		if entityType != nil {
			e.EntityType = *entityType
		}
		e.Details = json.RawMessage(details)
		if ip != nil {
			e.IP = ip.String()
		}
		out = append(out, e)
	}
	return out, mapErr(rows.Err())
}

func (s *PgStore) ExportAudit(ctx context.Context, from, to time.Time, limit int) ([]AuditEntry, error) {
	// A zero `from` is "since the beginning". It is passed as a real lower bound
	// rather than by dropping the predicate, so the statement below has one
	// shape and one argument count — the placeholder/args mismatch CLAUDE.md
	// warns about is a silent column swap here, not a compile error.
	rows, err := s.pool.Query(ctx, `
		SELECT `+auditCols+`
		FROM audit_log
		WHERE created_at >= $1 AND created_at <= $2
		ORDER BY created_at DESC, id DESC
		LIMIT $3`, from, to, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	return scanAuditRows(rows)
}

// --- heartbeats ---

func (s *PgStore) ListHeartbeats(ctx context.Context, monitorID int64, limit int, importantOnly bool) ([]HeartbeatView, error) {
	// The important-only branch is a separate literal so each query still
	// matches an index by itself (heartbeats_important_idx is partial) — the two
	// stay separate statements, they just no longer spell the projection out
	// twice. heartbeatCols documents itself as the list "both heartbeat reads
	// share, so their column lists cannot drift apart from scanHeartbeat", and
	// this read did not use it: the columns were written out by hand here and
	// again below, three copies of an ordering that scanHeartbeat consumes
	// positionally. They happened to agree; nothing made them.
	q := `SELECT ` + heartbeatCols + `
		FROM heartbeats WHERE monitor_id = $1 ORDER BY checked_at DESC LIMIT $2`
	if importantOnly {
		q = `SELECT ` + heartbeatCols + `
		FROM heartbeats WHERE monitor_id = $1 AND important ORDER BY checked_at DESC LIMIT $2`
	}
	rows, err := s.pool.Query(ctx, q, monitorID, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []HeartbeatView{}
	for rows.Next() {
		hb, err := scanHeartbeat(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, hb)
	}
	return out, rows.Err()
}

// heartbeatCols is the projection both heartbeat reads share, so their column
// lists cannot drift apart from scanHeartbeat.
const heartbeatCols = `monitor_id, checked_at, status, latency_ms, message, important, retries, extra`

// ListHeartbeatWindow implements Store.
//
// The stride is computed in SQL from the window's own row count, so the result
// spans the whole window instead of its newest end. Reading the newest N rows
// and filtering afterwards — what the handler used to do — meant a "1 year"
// view on a 60-second monitor really showed the last ~35 days.
//
// The oldest row is kept explicitly, and the stride is sized to leave room for
// it.
//
// A stride of ceil(total/limit) does not reach the oldest row unless the row
// count happens to be a multiple of it: 200 rows at a limit of 20 gives 10 and
// stops at rn=191, so the window silently ended 9 minutes short of where it was
// asked to. Simply adding rn=total on top would return limit+1 rows and break
// the other half of the contract. Sizing the stride over the limit-1 gaps
// BETWEEN the endpoints — ceil((total-1)/(limit-1)) — leaves exactly one slot
// for the oldest, so both ends are present and the count never exceeds limit.
//
// The important-only branch is a separate literal so each query still matches
// an index on its own: heartbeats_important_idx is partial.
func (s *PgStore) ListHeartbeatWindow(ctx context.Context, monitorID int64, from, to time.Time, limit int, importantOnly bool) ([]HeartbeatView, error) {
	if limit <= 0 {
		limit = defaultHeartbeatLimit
	}
	// $1 monitor_id, $2 from, $3 to, $4 limit — four placeholders, four args in
	// both branches.
	where := `monitor_id = $1 AND checked_at >= $2 AND checked_at <= $3`
	if importantOnly {
		where += ` AND important`
	}
	// A limit of 1 has no room for two endpoints; it takes the newest and says
	// so, rather than dividing by zero below.
	pick := `rn = 1`
	if limit >= 2 {
		// stride = ceil((total-1) / (limit-1)), written with integer arithmetic.
		pick = `(rn - 1) % GREATEST((total + $4::bigint - 3) / ($4::bigint - 1), 1) = 0 OR rn = total`
	}
	q := `SELECT ` + heartbeatCols + ` FROM (
			SELECT ` + heartbeatCols + `,
				row_number() OVER (ORDER BY checked_at DESC) AS rn,
				count(*)     OVER ()                        AS total
			FROM heartbeats WHERE ` + where + `
		) t
		WHERE total <= $4::bigint OR ` + pick + `
		ORDER BY checked_at DESC`
	rows, err := s.pool.Query(ctx, q, monitorID, from, to, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []HeartbeatView{}
	for rows.Next() {
		hb, err := scanHeartbeat(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, hb)
	}
	return out, rows.Err()
}

// ListImportantHeartbeats implements Store.
//
// Important beats are the state transitions only, so the set is small next to
// the heartbeats table and the partial heartbeats_important_idx covers it. The
// IN-subquery rather than a JOIN keeps heartbeatCols unqualified: monitors has
// a status column too, and a join would make the shared projection ambiguous.
// The count takes no placeholders; the page read takes $1 limit, $2 offset.
func (s *PgStore) ListImportantHeartbeats(ctx context.Context, p Page) ([]HeartbeatView, int, error) {
	const live = `important AND monitor_id IN (SELECT id FROM monitors WHERE deleted_at IS NULL)`
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM heartbeats WHERE `+live).Scan(&total); err != nil {
		return nil, 0, mapErr(err)
	}
	rows, err := s.pool.Query(ctx, `SELECT `+heartbeatCols+`
		FROM heartbeats WHERE `+live+`
		ORDER BY checked_at DESC, monitor_id LIMIT $1 OFFSET $2`, p.Limit, p.Offset)
	if err != nil {
		return nil, 0, mapErr(err)
	}
	defer rows.Close()
	out := []HeartbeatView{}
	for rows.Next() {
		hb, err := scanHeartbeat(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, hb)
	}
	return out, total, rows.Err()
}

func (s *PgStore) LatestHeartbeat(ctx context.Context, monitorID int64) (HeartbeatView, bool, error) {
	hb, err := scanHeartbeat(s.pool.QueryRow(ctx, `
		SELECT monitor_id, checked_at, status, latency_ms, message, important, retries, extra
		FROM heartbeats WHERE monitor_id = $1 ORDER BY checked_at DESC LIMIT 1`, monitorID))
	if err != nil {
		if mapErr(err) == ErrNotFound {
			return HeartbeatView{}, false, nil
		}
		return HeartbeatView{}, false, mapErr(err)
	}
	return hb, true, nil
}

func scanHeartbeat(row pgx.Row) (HeartbeatView, error) {
	var hb HeartbeatView
	var latency *int32
	var extra []byte
	err := row.Scan(&hb.MonitorID, &hb.CheckedAt, &hb.Status, &latency, &hb.Message, &hb.Important, &hb.Retries, &extra)
	if err != nil {
		return HeartbeatView{}, err
	}
	if latency != nil {
		v := int(*latency)
		hb.LatencyMs = &v
	}
	hb.Extra = json.RawMessage(extra)
	return hb, nil
}

// --- small helpers ---

func jsonOrEmpty(b []byte) []byte {
	if len(b) == 0 {
		return []byte("{}")
	}
	return b
}

func jsonOrNil(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

func itoa(n int) string {
	return strconv.Itoa(n)
}

// escapeLikePattern neutralises the LIKE wildcards in a user's search term.
// Without it "50%" matched every monitor and "a_b" matched "axb", because the
// term was interpolated straight into an ILIKE pattern — MemStore meanwhile
// does a literal substring match, so the two stores disagreed on what a search
// even means. Backslash is PostgreSQL's default LIKE escape character, so it is
// escaped first to keep a literal backslash literal.
func escapeLikePattern(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// parentCycleInTx walks the stored parent chain from parentID and reports
// ErrParentCycle if it reaches id. It duplicates the API's validation walk on
// purpose: that one exists to give a helpful message before any work is done,
// this one runs inside the transaction that writes and is the one that cannot
// be raced.
func parentCycleInTx(ctx context.Context, tx pgx.Tx, id, parentID int64) error {
	if parentID == id {
		return ErrParentCycle
	}
	seen := make(map[int64]bool)
	for cur := parentID; ; {
		if cur == id {
			return ErrParentCycle
		}
		if seen[cur] || len(seen) >= maxParentChain {
			// The stored chain already loops, independently of this request.
			return ErrParentCycle
		}
		seen[cur] = true
		var next *int64
		if err := tx.QueryRow(ctx, `SELECT parent_id FROM monitors WHERE id = $1`, cur).Scan(&next); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return mapErr(err)
		}
		if next == nil {
			return nil
		}
		cur = *next
	}
}
