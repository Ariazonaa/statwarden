-- Push tokens become password-like: only their SHA-256 hash is stored, never the
-- raw token. The push heartbeat endpoint used to match the token against the
-- plaintext monitors.push_token column, so a database dump handed out working
-- heartbeat credentials for every push monitor. The recorder now hashes the
-- incoming token and looks the hash up instead; the raw token is shown once at
-- mint time and never persisted.
--
-- Existing tokens keep working: the current plaintext is hashed in place, so the
-- same token an operator already configured in their cron/backup job still
-- resolves.
--
-- The hash is PostgreSQL's built-in sha256() (11+), over the UTF-8 bytes, which
-- is exactly what push.HashToken computes. This migration first shipped with
-- pgcrypto's digest() and a CREATE EXTENSION — superuser-only on many managed
-- databases, for a function the server has built in. Editing a released
-- migration is safe here: golang-migrate records only the version number, never
-- a checksum, so databases already past 0011 never run it again, and the result
-- is byte-for-byte the same for those that do.
--
-- The UPDATE skips values that are already a 64-hex-digit SHA-256. The down
-- migration keeps the hashes (they cannot be reversed), so without the guard a
-- down followed by an up hashed them a second time and every push token stopped
-- matching. A real token is never 64 hex digits: push.GenerateToken mints 32.

ALTER TABLE monitors RENAME COLUMN push_token TO push_token_hash;
ALTER INDEX monitors_push_token_idx RENAME TO monitors_push_token_hash_idx;

UPDATE monitors
   SET push_token_hash = encode(sha256(convert_to(push_token_hash, 'UTF8')), 'hex')
 WHERE push_token_hash IS NOT NULL AND push_token_hash <> ''
   AND push_token_hash !~ '^[0-9a-f]{64}$';
