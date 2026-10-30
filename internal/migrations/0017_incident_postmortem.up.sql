-- 0017_incident_postmortem.up.sql — retrospectives on resolved incidents.
--
-- An incident already carries a body and a timeline of updates, and both are
-- written WHILE it is happening: short, hurried, addressed to people refreshing
-- the page. A post-mortem is the opposite document — written afterwards, once,
-- explaining what broke and what changes because of it — so it does not belong
-- in the timeline. Appending it as one more update would bury it under the
-- "monitoring" and "resolved" posts it is meant to summarise, and would make it
-- impossible to publish separately from the incident itself.
--
-- Hence two columns rather than one:
--
--   postmortem              the write-up, Markdown, NULL when none was started
--   postmortem_published_at when it became visible to anonymous visitors
--
-- The second column is what makes a draft possible, and a draft is the whole
-- point: a post-mortem is written over days, is often wrong in its first pass,
-- and names internal systems. NULL means "written but not published", and the
-- public status page filters on exactly that. Publishing is therefore a
-- timestamp, not a boolean, so the page can also say WHEN the retrospective was
-- posted — which for a reader is the difference between a fresh write-up and one
-- that appeared three months after the outage.
--
-- No CHECK ties publication to incidents.status = 'resolved'. The API enforces
-- that (a retrospective on an ongoing incident is not a retrospective), but a
-- constraint here would also forbid REOPENING a resolved incident that already
-- has a published post-mortem — the row would become unwritable, and the
-- operator would meet a constraint violation on an unrelated status change.

BEGIN;

ALTER TABLE incidents
    ADD COLUMN postmortem              text,
    ADD COLUMN postmortem_published_at timestamptz;

-- No index on postmortem_published_at, deliberately.
--
-- There was one, a partial index on (status_page_id, postmortem_published_at)
-- WHERE postmortem_published_at IS NOT NULL, described as backing the public
-- render. It backed nothing. The public page reads its incidents through
-- ListIncidents, which selects every incident for the page ordered by
-- (pinned, started_at) and does the resolved/published split in Go — there is no
-- statement anywhere in the tree with `postmortem_published_at IS NOT NULL` in
-- its WHERE clause. What the index actually did was add a write to every
-- incident insert and every publish, to be read by no query at all.
--
-- If that split ever moves into SQL, the index comes back with the statement
-- that needs it, and the comment can be checked against the query.

COMMIT;
