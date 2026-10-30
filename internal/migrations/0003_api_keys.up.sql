-- 0003_api_keys.up.sql — scoped programmatic-access credentials.
--
-- API keys authenticate scripts via `Authorization: Bearer <secret>` as an
-- alternative to cookie sessions. Only the SHA-256 of the secret is stored
-- (key_hash); the plaintext secret is shown once at creation. scopes is a
-- JSON array drawn from ('read','write','admin'). last_used_at powers the
-- "last used" display; expires_at is an optional hard expiry.

BEGIN;

CREATE TABLE api_keys (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id      bigint      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name         text        NOT NULL,
    prefix       text        NOT NULL, -- non-secret display prefix (first chars)
    key_hash     text        NOT NULL UNIQUE, -- SHA-256 of the secret
    scopes       jsonb       NOT NULL DEFAULT '[]'::jsonb,
    last_used_at timestamptz,
    expires_at   timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX api_keys_user_id_idx ON api_keys (user_id);

COMMIT;
