-- 0005_status_pages_ext.up.sql — status-page extensions.
-- The core status-page/incident/subscriber tables already exist in 0001. This
-- migration adds the pieces the public rendering + incident work needs on top:
--   * incident_monitors: which monitors an incident affects ("components").
--   * subscriber double-opt-in bookkeeping (when the verify mail was last sent).
--   * a per-incident "pinned" flag so an incident can stay at the top of a page.
-- Everything here is additive and safe to roll back.

BEGIN;

-- Affected components: an incident may reference the monitors it impacts so the
-- public page can badge them and subscriber mails can name them.
CREATE TABLE incident_monitors (
    incident_id bigint NOT NULL REFERENCES incidents (id) ON DELETE CASCADE,
    monitor_id  bigint NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    PRIMARY KEY (incident_id, monitor_id)
);

CREATE INDEX incident_monitors_monitor_idx ON incident_monitors (monitor_id);

-- Keep an incident pinned to the top of the page regardless of recency.
ALTER TABLE incidents ADD COLUMN pinned boolean NOT NULL DEFAULT false;

-- Record when the last double-opt-in verification mail went out so a resend can
-- be rate-limited without inspecting the mailer.
ALTER TABLE status_page_subscribers ADD COLUMN verify_sent_at timestamptz;

COMMIT;
