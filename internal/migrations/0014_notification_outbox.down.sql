-- 0014_notification_outbox.down.sql
--
-- Dropping the table discards any alert still waiting to be delivered. That is
-- acceptable for a rollback — the code that reads it is going away in the same
-- step — but it is a data loss, not a no-op, so it is worth saying out loud.

BEGIN;

DROP INDEX IF EXISTS notification_outbox_due_idx;
DROP TABLE IF EXISTS notification_outbox;

COMMIT;
