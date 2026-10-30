-- 0015_delivery_recovery_idx.down.sql — drop the recovery-lookup index.
--
-- Dropping it loses no data. ChannelsAwaitingRecovery keeps working and falls
-- back to sorting over the monitor's whole delivery history, which is where it
-- started.

BEGIN;

DROP INDEX IF EXISTS notification_deliveries_recovery_idx;

COMMIT;
