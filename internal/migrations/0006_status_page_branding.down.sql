-- 0006_status_page_branding.down.sql — revert per-page branding colour.

BEGIN;

ALTER TABLE status_pages DROP CONSTRAINT IF EXISTS status_pages_accent_color_chk;
ALTER TABLE status_pages DROP COLUMN IF EXISTS accent_color;

COMMIT;
