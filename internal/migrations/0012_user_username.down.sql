-- Reverse 0012: drop the case-insensitive unique index and the username column.
-- The usernames were derived from display_name / e-mail on the way up, so
-- nothing that cannot be recomputed by re-applying the up migration is lost.
BEGIN;

DROP INDEX IF EXISTS users_username_lower_idx;
ALTER TABLE users DROP COLUMN IF EXISTS username;

COMMIT;
