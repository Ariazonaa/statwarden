-- 0005_status_pages_ext.down.sql

BEGIN;

ALTER TABLE status_page_subscribers DROP COLUMN IF EXISTS verify_sent_at;
ALTER TABLE incidents DROP COLUMN IF EXISTS pinned;
DROP TABLE IF EXISTS incident_monitors;

COMMIT;
