-- 0009_encrypt_monitor_config.down.sql — drop the schema comment added by the
-- up migration.
--
-- Only the documentation comment is reverted. Config blobs that were sealed
-- while the up migration was in force stay sealed: the application still opens
-- them through the SecretsBox on read (and this migration has no key to open
-- them anyway), so clearing the comment is the whole of the reversal.

BEGIN;

COMMENT ON COLUMN monitors.config IS NULL;

COMMIT;
