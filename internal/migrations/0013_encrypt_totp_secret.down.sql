-- 0013_encrypt_totp_secret.down.sql — drop the schema comment added by the up
-- migration.
--
-- Only the documentation comment is reverted. Secrets that were sealed while the
-- up migration was in force stay sealed: the application still opens them
-- through the SecretsBox on read (and this migration has no key to open them
-- anyway), so clearing the comment is the whole of the reversal.

BEGIN;

COMMENT ON COLUMN users.totp_secret IS NULL;

COMMIT;
