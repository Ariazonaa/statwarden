-- 0019_maintenance_scope.up.sql — make "applies to every monitor" explicit.
--
-- Until now the scope of a maintenance window was implied by the CONTENTS of
-- maintenance_monitors: no rows meant "all monitors", any rows meant "just
-- these". That convention is fine while a human is the only thing that edits
-- the set, and unsafe the moment anything else can.
--
-- maintenance_monitors.monitor_id is ON DELETE CASCADE. So destroying the last
-- monitor a window covered — by purging it from the recycle bin, or by the
-- retention sweep doing it automatically on a timer roughly two weeks after
-- somebody deleted it — emptied the set, and an emptied set read as the
-- fleet-wide sentinel. A window titled "planned DB maintenance, Sunday 02:00"
-- then began suppressing alerts for EVERY monitor and announcing itself on every
-- public status page. Nobody performed an action at that moment; the window's
-- own row was never touched.
--
-- The scope is now a column, so it can only change when someone changes it.
--
-- The backfill preserves today's meaning exactly: a window that currently has no
-- monitor rows is fleet-wide, which is what the running system already believes
-- about it. That includes windows whose scope was emptied by the cascade this
-- migration exists to prevent — this migration cannot know which those are, and
-- guessing would silently narrow a window an operator may be relying on. What it
-- does guarantee is that no FURTHER window can change scope on its own.

BEGIN;

ALTER TABLE maintenance_windows
    ADD COLUMN applies_to_all boolean NOT NULL DEFAULT false;

UPDATE maintenance_windows w
   SET applies_to_all = true
 WHERE NOT EXISTS (
       SELECT 1 FROM maintenance_monitors mm WHERE mm.maintenance_id = w.id
 );

COMMIT;
