-- 0021_heartbeats_checked_at_brin.up.sql — let retention find old heartbeats.
--
-- The retention job deletes `WHERE checked_at < $cutoff`. The only indexes on
-- heartbeats lead with monitor_id, so that predicate had no index to use and
-- every pass was a sequential scan of the largest table in the schema — the
-- whole table, every six hours, to find the few hours of rows that just aged
-- out.
--
-- BRIN rather than B-tree. Heartbeats are appended in checked_at order, so the
-- physical order of the table follows the column closely, which is exactly the
-- case BRIN is built for: it stores one min/max pair per range of pages, a few
-- hundred kilobytes for a table of hundreds of millions of rows, against
-- several gigabytes for a B-tree on the same column. Its cost is precision — a
-- page range is either skipped or read whole — and for a range delete that
-- touches the oldest edge of the table that is no cost at all. Pages freed by
-- retention and refilled later carry rows from a short, recent window, so the
-- ranges stay narrow after pruning starts.
--
-- Built inside a transaction like every other migration here. A BRIN build is
-- one sequential pass with a tiny output, so the write lock it takes is short;
-- CONCURRENTLY would need a migration outside a transaction and leaves an
-- invalid index behind if it fails.

BEGIN;

CREATE INDEX IF NOT EXISTS heartbeats_checked_at_brin_idx
    ON heartbeats USING brin (checked_at);

COMMIT;
