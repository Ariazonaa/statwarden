-- 0016_delivery_latency_kinds.up.sql — let the delivery log record latency alerts.
--
-- notification_deliveries.event_kind carries a CHECK listing every kind the
-- dispatcher may record (0002). The response-time anomaly work added two kinds —
-- 'latency-anomaly' and 'latency-normal' — and did not widen the constraint, so
-- every anomaly delivery hit a check violation on the audit-log INSERT.
--
-- The failure mode was quiet and lopsided: the alert itself still went out,
-- because RecordDelivery runs AFTER the provider Send, so an operator got the
-- message while the log kept no record of it and the dispatcher logged an error
-- per delivery. Nothing in the fast suite could see it either — that runs against
-- MemStore, which has no constraints at all (CLAUDE.md).
--
-- The constraint is kept rather than dropped. It is what makes an unknown kind a
-- loud failure at the boundary instead of an unrenderable row discovered later,
-- and domain.NotificationEventKind.Valid() is the same list in Go. Both change
-- together, on purpose.

BEGIN;

ALTER TABLE notification_deliveries
    DROP CONSTRAINT notification_deliveries_event_kind_check;

ALTER TABLE notification_deliveries
    ADD CONSTRAINT notification_deliveries_event_kind_check
    CHECK (event_kind IN (
        'down', 'up', 'cert-expiry', 'maintenance-start', 'maintenance-end',
        'test', 'digest', 'latency-anomaly', 'latency-normal'
    ));

COMMIT;
