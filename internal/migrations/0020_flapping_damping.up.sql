-- 0020_flapping_damping.up.sql — confirmed recoveries and flapping summaries.
--
-- recovery_checks is the way-back counterpart of max_retries: how many
-- consecutive successful checks a DOWN monitor needs before it counts as UP.
-- The default 1 is exactly the old behaviour (one success recovers), so every
-- existing monitor keeps working as it did; 0 is refused because it has no
-- meaning distinct from 1 and would only be a second spelling of it.
--
-- The delivery log gains the two flapping kinds. The constraint is widened in
-- the same migration as the Go constants (see 0016 for why the two must never
-- drift, and migrations/event_kind_sync_test.go for the test that holds them
-- together).

BEGIN;

ALTER TABLE monitors
    ADD COLUMN recovery_checks integer NOT NULL DEFAULT 1 CHECK (recovery_checks >= 1);

ALTER TABLE notification_deliveries
    DROP CONSTRAINT notification_deliveries_event_kind_check;

ALTER TABLE notification_deliveries
    ADD CONSTRAINT notification_deliveries_event_kind_check
    CHECK (event_kind IN (
        'down', 'up', 'cert-expiry', 'maintenance-start', 'maintenance-end',
        'test', 'digest', 'latency-anomaly', 'latency-normal',
        'flapping', 'flapping-stable'
    ));

COMMIT;
