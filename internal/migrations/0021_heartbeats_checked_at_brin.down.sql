-- 0021_heartbeats_checked_at_brin.down.sql — drop the retention index.
--
-- Dropping it loses no data. The retention delete keeps working and goes back
-- to scanning the whole table.

BEGIN;

DROP INDEX IF EXISTS heartbeats_checked_at_brin_idx;

COMMIT;
