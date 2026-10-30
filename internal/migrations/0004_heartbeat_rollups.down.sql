-- 0004_heartbeat_rollups.down.sql

BEGIN;

DROP TABLE IF EXISTS heartbeat_rollups_daily;
DROP TABLE IF EXISTS heartbeat_rollups_hourly;

COMMIT;
