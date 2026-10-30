-- 0007_monitor_groups.down.sql — revert organisational folders for monitors.
-- The column goes before the table: monitors.group_id is the only reference to
-- monitor_groups, so dropping it first leaves the DROP TABLE unblocked.

BEGIN;

DROP INDEX IF EXISTS monitors_group_id_idx;
ALTER TABLE monitors DROP COLUMN IF EXISTS group_id;
DROP TABLE IF EXISTS monitor_groups;

COMMIT;
