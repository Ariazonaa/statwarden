-- 0018_monitor_soft_delete.down.sql
--
-- Rolling back removes the marker, which would make every monitor in the bin
-- visible again — as a paused monitor nobody expects, carrying links to tags,
-- status pages and notification channels that were deliberately taken away.
-- That is worse than the deletion the operator asked for, so this direction
-- finishes the job first and hard-deletes them.
--
-- Said plainly because it is a real data loss: everything still in the bin at
-- rollback time, and its heartbeat history with it, is gone. Anything worth
-- keeping should be restored through the UI before migrating down.
--
-- `active` is not restored. The up migration set it to false on delete and
-- cannot know what it was before; the rows are being removed here anyway.

BEGIN;

DELETE FROM monitors WHERE deleted_at IS NOT NULL;

DROP INDEX IF EXISTS monitors_live_idx;

ALTER TABLE monitors
    DROP COLUMN IF EXISTS deleted_at;

COMMIT;
