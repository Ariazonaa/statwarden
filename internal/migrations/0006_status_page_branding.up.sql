-- 0006_status_page_branding.up.sql — per-page branding colour for status pages.
-- The logo itself lives on disk (UPLOADS_DIR) and is referenced by the existing
-- status_pages.logo_path column from 0001; only the accent colour is new here.
-- The CHECK is defence in depth: Go already normalises to lowercase #rrggbb
-- before the value can reach an inline style attribute on the public page.

BEGIN;

ALTER TABLE status_pages ADD COLUMN accent_color text;

ALTER TABLE status_pages
    ADD CONSTRAINT status_pages_accent_color_chk
    CHECK (accent_color IS NULL OR accent_color ~ '^#[0-9a-f]{6}$');

COMMIT;
