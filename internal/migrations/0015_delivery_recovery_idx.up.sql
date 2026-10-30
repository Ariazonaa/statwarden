-- 0015_delivery_recovery_idx.up.sql — serve ChannelsAwaitingRecovery directly.
--
-- Dispatcher.Restore adopts every outage that was already in progress when the
-- process started, and for each one asks the delivery log which channels were
-- told about it and not yet told it ended:
--
--   SELECT DISTINCT ON (notification_id) notification_id, event_kind
--   FROM notification_deliveries
--   WHERE monitor_id = $1 AND notification_id IS NOT NULL
--     AND status = 'success' AND event_kind IN ('down', 'up')
--   ORDER BY notification_id, finished_at DESC, id DESC
--
-- The existing index is (monitor_id, finished_at DESC), which serves the filter
-- and not the ordering, so the answer needed a sort over every delivery ever
-- recorded for that monitor. That put work proportional to the AGE OF THE
-- DEPLOYMENT on the startup path of a process whose job is to come back
-- quickly — and it only got slower, because nothing pruned the table.
--
-- This index matches the DISTINCT ON directly. It is partial on the same
-- predicate the query uses, which is most of the point: successful down/up
-- deliveries are a small slice of a log that also holds every failure, every
-- test send, every digest.

BEGIN;

CREATE INDEX notification_deliveries_recovery_idx
    ON notification_deliveries (monitor_id, notification_id, finished_at DESC, id DESC)
    WHERE notification_id IS NOT NULL
      AND status = 'success'
      AND event_kind IN ('down', 'up');

COMMIT;
