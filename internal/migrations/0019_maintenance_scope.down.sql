-- 0019_maintenance_scope.down.sql
--
-- Dropping the column restores the implied-scope convention, and with it the
-- defect: a scoped window whose last monitor is destroyed silently becomes
-- fleet-wide again. Stated rather than left to be discovered.
--
-- No data is lost in the other direction — a fleet-wide window has no
-- maintenance_monitors rows either way, so the convention reads it identically
-- once the column is gone.

BEGIN;

ALTER TABLE maintenance_windows
    DROP COLUMN IF EXISTS applies_to_all;

COMMIT;
