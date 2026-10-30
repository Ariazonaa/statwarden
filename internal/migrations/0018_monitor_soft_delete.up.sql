-- 0018_monitor_soft_delete.up.sql — deleting a monitor becomes reversible.
--
-- Deleting a monitor took its entire heartbeat history with it, through the
-- ON DELETE CASCADE on `heartbeats`. That is the correct schema for a hard
-- delete and the wrong outcome for a misclick — especially inside a bulk action,
-- where one confirmation removes a whole selection and there is nothing to undo
-- it with. A monitor is cheap to recreate; three years of its history is not.
--
-- So `deleted_at` marks the row instead. Every read in the API filters on it,
-- the row keeps its id, and everything hanging off that id — heartbeats, tags,
-- notification links, status-page placements, certificate info — simply stays
-- where it is. Restoring is one UPDATE, and it restores the history with it.
--
-- The scheduler is NOT taught about the column. cmd/statwarden's engine reads
-- monitors through `ListActiveMonitors` in internal/db/queries, which sqlc
-- generates and which CLAUDE.md warns against regenerating for unrelated work.
-- Instead the delete sets `active = false` in the same statement: a monitor in
-- the bin is by definition not scheduled, so the existing `WHERE active = true`
-- already excludes it. The push recorder (`AND active = true`) and the alert
-- dispatcher inherit the same exclusion for free.
--
-- Two consequences of that, both deliberate and both tested:
--
--   * A restored monitor comes back PAUSED. The column cannot say what `active`
--     was before, and guessing "it was probably on" would restart checks — and
--     alerts — for something that has been gone for a week, at the moment
--     somebody is only trying to look at its history.
--   * A monitor in the bin is not a parent. The engine looks its parent up in
--     the scheduled set, finds nothing, and suppresses nothing; deleting a
--     parent therefore UN-suppresses its children rather than muting them
--     forever, which is the safe direction.

BEGIN;

ALTER TABLE monitors
    ADD COLUMN deleted_at timestamptz;

-- Every hot read is "the monitors that are not in the bin", so the index is
-- partial on exactly that predicate and stays the size of the live set. The bin
-- itself is listed rarely and by one person, and needs no index of its own.
CREATE INDEX monitors_live_idx ON monitors (id) WHERE deleted_at IS NULL;

COMMIT;
