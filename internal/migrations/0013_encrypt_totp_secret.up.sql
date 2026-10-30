-- 0013_encrypt_totp_secret.up.sql — record the at-rest encryption boundary for
-- users.totp_secret.
--
-- users.backup_codes has always held hashes, but users.totp_secret sat next to
-- it in the clear. A TOTP secret cannot be hashed — verifying a code needs the
-- shared secret itself — so the only protection available is encryption, and
-- without it a single database leak hands an attacker every account's second
-- factor, which is the exact property 2FA is bought for.
--
-- The application now seals the column with the SecretsBox, exactly as
-- monitors.config (migration 0009) and notifications.config are sealed: the
-- base32 secret is marshalled to a JSON string, sealed, and stored as the
-- envelope {"$encrypted":"<base64(nonce||ciphertext)>"} written on every write;
-- read paths open it back before auth.VerifyTOTP sees it.
--
-- This migration CANNOT rewrite existing rows itself: the AES-256-GCM key is
-- derived from ENCRYPTION_KEY, which lives only in the application process and
-- is never available to a SQL migration. Doing crypto here would either need the
-- key in the database (defeating the purpose) or produce blobs the app could not
-- open. Legacy plaintext secrets are therefore handled the way migration 0009
-- handled monitor configs: a value that is not an envelope is read through
-- unchanged (plaintext passthrough), so 2FA keeps working, and it is re-sealed
-- on its next write — enrolment, verification, disable, backup-code
-- regeneration, or consuming a backup code at login. A one-shot backfill, if
-- wanted, belongs in the application where the key is, not here.
--
-- The COMMENT is the reversible schema record of that contract.

BEGIN;

COMMENT ON COLUMN users.totp_secret IS
    'TOTP shared secret (base32). Sealed at rest by the application SecretsBox as {"$encrypted":"..."}, mirroring monitors.config and notifications.config. The AES-256-GCM key (ENCRYPTION_KEY) is application-only, so pre-existing plaintext secrets are read through unchanged and re-sealed on their next write.';

COMMIT;
