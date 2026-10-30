-- 0009_encrypt_monitor_config.up.sql — record the at-rest encryption boundary
-- for monitors.config.
--
-- monitors.config carries credentials (HTTP/WebSocket headers, Basic/Bearer
-- tokens, DB/MQTT/Redis/MongoDB passwords, the RADIUS shared secret, SNMP
-- community and v3 auth/priv passwords). The application now seals it with the
-- SecretsBox exactly as notifications.config is sealed: the whole blob becomes
-- an envelope {"$encrypted":"<base64(nonce||ciphertext)>"} written on every
-- create/update, and read paths open it back to plaintext.
--
-- This migration CANNOT rewrite existing rows itself: the AES-256-GCM key is
-- derived from ENCRYPTION_KEY, which lives only in the application process and
-- is never available to a SQL migration. Doing crypto here would either need
-- the key in the database (defeating the purpose) or produce blobs the app
-- could not open. Legacy plaintext rows are therefore handled the same way the
-- notification rollout handled them — the SecretsBox reads a non-enveloped
-- config through unchanged (plaintext passthrough) and it is re-sealed on its
-- next write. A one-shot backfill, if wanted, belongs in the application where
-- the key is, not here.
--
-- The COMMENT is the reversible schema record of that contract.

BEGIN;

COMMENT ON COLUMN monitors.config IS
    'Monitor type configuration (jsonb). Credential-bearing fields are sealed at rest by the application SecretsBox as {"$encrypted":"..."}, mirroring notifications.config. The AES-256-GCM key (ENCRYPTION_KEY) is application-only, so pre-existing plaintext rows are read through unchanged and re-sealed on their next write.';

COMMIT;
