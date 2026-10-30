-- 0017_incident_postmortem.down.sql
--
-- Dropping the columns discards every post-mortem ever written, published or
-- draft. That is stated rather than left to be discovered: there is nowhere else
-- in the schema for the text to go, so this direction genuinely loses it. Anyone
-- rolling back past this migration with retrospectives worth keeping should take
-- them out first — the CSV export does not cover incidents, but
-- `SELECT id, title, postmortem FROM incidents WHERE postmortem IS NOT NULL`
-- does.
--
-- IF EXISTS on the index drop, because the up direction no longer creates it:
-- an instance that applied the earlier version of 0017 still has it, and this
-- direction has to work on both.

BEGIN;

DROP INDEX IF EXISTS incidents_postmortem_published_idx;

ALTER TABLE incidents
    DROP COLUMN IF EXISTS postmortem_published_at,
    DROP COLUMN IF EXISTS postmortem;

COMMIT;
