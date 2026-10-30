-- 0008_auth_methods.down.sql — revert the additional authentication methods.
-- Reverse dependency order: the four new tables all reference users, so they go
-- first and the users column last. Each table's indexes and unique constraints
-- are dropped with it, and users.webauthn_handle's implicit unique index goes
-- with the column, so nothing needs dropping by name.

BEGIN;

DROP TABLE IF EXISTS webauthn_challenges;
DROP TABLE IF EXISTS webauthn_credentials;
ALTER TABLE users DROP COLUMN IF EXISTS webauthn_handle;
DROP TABLE IF EXISTS user_identities;
DROP TABLE IF EXISTS password_resets;

COMMIT;
