-- 0020_flapping_damping.down.sql
--
-- As in 0016: the narrower constraint cannot be added while rows carry the kinds
-- it would forbid, so the flapping rows are deleted first. That is a loss in the
-- delivery audit log only, stated here rather than discovered later. Dropping
-- recovery_checks returns every monitor to single-success recovery.

BEGIN;

DELETE FROM notification_deliveries
    WHERE event_kind IN ('flapping', 'flapping-stable');

ALTER TABLE notification_deliveries
    DROP CONSTRAINT notification_deliveries_event_kind_check;

ALTER TABLE notification_deliveries
    ADD CONSTRAINT notification_deliveries_event_kind_check
    CHECK (event_kind IN (
        'down', 'up', 'cert-expiry', 'maintenance-start', 'maintenance-end',
        'test', 'digest', 'latency-anomaly', 'latency-normal'
    ));

ALTER TABLE monitors DROP COLUMN recovery_checks;

COMMIT;
