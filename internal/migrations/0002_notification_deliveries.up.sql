-- 0002_notification_deliveries.up.sql — notification core additions.
--
-- 1. notification_deliveries: the delivery audit log — one row per attempted
--    delivery (event x channel) answering "why did no alert arrive?".
-- 2. notifications.templates: per-notification message-template overrides,
--    keyed by event kind ({"down": {"title": "...", "body": "..."}, ...}).
-- 3. monitor_notifications.escalation_after_minutes: escalation level of an
--    assignment — 0 = alert immediately, N > 0 = additionally alert this
--    channel once the monitor has been DOWN for N minutes.

BEGIN;

CREATE TABLE notification_deliveries (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    notification_id bigint      REFERENCES notifications (id) ON DELETE SET NULL,
    monitor_id      bigint      REFERENCES monitors (id) ON DELETE SET NULL,
    provider_type   text        NOT NULL, -- registry type name at send time
    event_kind      text        NOT NULL CHECK (event_kind IN ('down', 'up', 'cert-expiry', 'maintenance-start', 'maintenance-end', 'test', 'digest')),
    status          text        NOT NULL CHECK (status IN ('success', 'failed')),
    error           text        NOT NULL DEFAULT '',
    attempts        integer     NOT NULL DEFAULT 1 CHECK (attempts >= 0),
    started_at      timestamptz NOT NULL,
    finished_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX notification_deliveries_finished_at_idx ON notification_deliveries (finished_at DESC);
CREATE INDEX notification_deliveries_notification_idx ON notification_deliveries (notification_id, finished_at DESC);
CREATE INDEX notification_deliveries_monitor_idx ON notification_deliveries (monitor_id, finished_at DESC);

ALTER TABLE notifications
    ADD COLUMN templates jsonb NOT NULL DEFAULT '{}'::jsonb;

ALTER TABLE monitor_notifications
    ADD COLUMN escalation_after_minutes integer NOT NULL DEFAULT 0 CHECK (escalation_after_minutes >= 0);

COMMIT;
