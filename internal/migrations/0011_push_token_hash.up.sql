-- Push tokens become password-like: only their SHA-256 hash is stored, never the
-- raw token. The push heartbeat endpoint used to match the token against the
-- plaintext monitors.push_token column, so a database dump handed out working
-- heartbeat credentials for every push monitor. The recorder now hashes the
-- incoming token and looks the hash up instead; the raw token is shown once at
-- mint time and never persisted.
--
-- Existing tokens keep working: the current plaintext is hashed in place, so the
-- same token an operator already configured in their cron/backup job still
-- resolves. pgcrypto supplies the one-shot digest for that backfill.
CREATE EXTENSION IF NOT EXISTS pgcrypto;

ALTER TABLE monitors RENAME COLUMN push_token TO push_token_hash;
ALTER INDEX monitors_push_token_idx RENAME TO monitors_push_token_hash_idx;

UPDATE monitors
   SET push_token_hash = encode(digest(push_token_hash, 'sha256'), 'hex')
 WHERE push_token_hash IS NOT NULL AND push_token_hash <> '';
