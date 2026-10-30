-- Opaque, revocable tokens for the embeddable monitor badges. The public badge
-- endpoints used to address a monitor by its sequential id, which let anyone
-- enumerate every monitor in the instance by walking /badge/1, /badge/2, …. A
-- badge is now addressed by this unguessable token instead, and a token can be
-- revoked (deleted) to stop a badge from resolving.
--
-- One token per monitor (monitor_id is the primary key); the token itself is
-- unique so a lookup by token hits a single row. Deleting a monitor drops its
-- badge token with it.
CREATE TABLE badge_tokens (
    monitor_id BIGINT PRIMARY KEY REFERENCES monitors(id) ON DELETE CASCADE,
    token      TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
