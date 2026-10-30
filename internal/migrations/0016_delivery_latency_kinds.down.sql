-- 0016_delivery_latency_kinds.down.sql
--
-- Narrowing the constraint again cannot succeed while rows carry the two kinds it
-- would forbid, and ADD CONSTRAINT validates existing rows. So the latency rows
-- are deleted first: this direction goes back to a schema that has no way to
-- express them, and leaving them behind would make the migration fail instead of
-- roll back. That is a data loss, in the audit log only, and it is stated out loud
-- rather than left to be discovered.

BEGIN;

DELETE FROM notification_deliveries
    WHERE event_kind IN ('latency-anomaly', 'latency-normal');

ALTER TABLE notification_deliveries
    DROP CONSTRAINT notification_deliveries_event_kind_check;

ALTER TABLE notification_deliveries
    ADD CONSTRAINT notification_deliveries_event_kind_check
    CHECK (event_kind IN (
        'down', 'up', 'cert-expiry', 'maintenance-start', 'maintenance-end',
        'test', 'digest'
    ));

COMMIT;
