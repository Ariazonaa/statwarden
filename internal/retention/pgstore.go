package retention

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Ariazonaa/statwarden/internal/db/sqlcgen"
)

// PgStore implements Store against PostgreSQL through the sqlc-generated
// DeleteHeartbeatsBefore query.
type PgStore struct {
	q *sqlcgen.Queries
	// pool backs the hand-written delivery prune; see PruneDeliveriesBefore.
	pool *pgxpool.Pool
}

// NewPgStore wraps a pgx pool in a Store.
func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{q: sqlcgen.New(pool), pool: pool}
}

// PruneBefore implements Store.
func (s *PgStore) PruneBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	return s.q.DeleteHeartbeatsBefore(ctx, pgtype.Timestamptz{Time: cutoff, Valid: true})
}

// PruneDeliveriesBefore implements Store.
//
// Hand-written rather than added to internal/db/queries: sqlcgen is already
// stale against later migrations and regenerating it drags unrelated model
// changes across the tree (see CLAUDE.md). One placeholder, one arg.
//
// A monitor that is currently DOWN keeps all of its delivery rows, whatever the
// window says. The log is not only an audit trail: notify.ChannelsAwaitingRecovery
// reads it to answer "which channels were told this monitor is down and not yet
// told otherwise", and Dispatcher.Restore asks that once per open outage after a
// restart. Prune the DOWN delivery and the answer becomes "nobody", so the
// recovery alert for that outage is never sent — the exact silence the outbox and
// the restore path were built to eliminate.
//
// It only bites when the window is shorter than an outage, so the 365-day default
// makes it near-unreachable; an operator who sets 7 days to keep the table small
// makes it ordinary. Rather than depend on the window being generous, the rows
// the lookup needs are simply not eligible.
//
// The predicate mirrors notify.OpenOutages (status 'down' AND active) so the two
// cannot disagree about which outages are open. What is retained is bounded by the
// resend interval — a handful of rows per channel per outage — and a NULL
// monitor_id (a test send) has no outage to protect and is pruned normally.
func (s *PgStore) PruneDeliveriesBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	ct, err := s.pool.Exec(ctx,
		`DELETE FROM notification_deliveries d
		 WHERE d.finished_at < $1
		   AND NOT EXISTS (
		       SELECT 1 FROM monitors m
		       WHERE m.id = d.monitor_id AND m.status = 'down' AND m.active
		   )`, cutoff)
	if err != nil {
		return 0, err
	}
	return ct.RowsAffected(), nil
}

// PruneBin implements Store.
//
// Hand-written for the reason PruneDeliveriesBefore is: sqlcgen is stale against
// later migrations and regenerating it drags unrelated changes across the tree
// (CLAUDE.md). One placeholder, one arg.
//
// The cutoff is computed HERE, in the statement, against the same clock that
// wrote deleted_at. Passing an instant from Go instead made the sweep depend on
// two clocks agreeing, and the failure was silent and one-directional: an
// application clock running fast empties the bin early, which is exactly what
// the grace period exists to prevent.
//
// `deleted_at IS NOT NULL` is not redundant next to the comparison. A NULL never
// satisfies `<`, so the comparison alone would already exclude live monitors —
// but this is the single most destructive statement in the product, and it
// should be obvious from reading the line, not from knowing SQL's three-valued
// logic. The heartbeats go with the rows through the schema's ON DELETE CASCADE,
// which is the space this sweep exists to reclaim.
func (s *PgStore) PruneBin(ctx context.Context, olderThanDays int) ([]PrunedMonitor, error) {
	rows, err := s.pool.Query(ctx,
		`DELETE FROM monitors
		  WHERE deleted_at IS NOT NULL
		    AND deleted_at < now() - make_interval(days => $1::int)
		 RETURNING id, name`, olderThanDays)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PrunedMonitor
	for rows.Next() {
		var m PrunedMonitor
		if err := rows.Scan(&m.ID, &m.Name); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
