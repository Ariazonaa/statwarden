-- 0002_notification_deliveries.down.sql — revert notification core additions.

BEGIN;

ALTER TABLE monitor_notifications
    DROP COLUMN escalation_after_minutes;

ALTER TABLE notifications
    DROP COLUMN templates;

DROP TABLE notification_deliveries;

COMMIT;
