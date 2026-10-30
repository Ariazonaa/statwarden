-- 0008_auth_methods.up.sql — the persistence every additional way of proving
-- who you are needs. Three features land in one migration because they are one
-- concern (authentication) and because splitting them would give three
-- migrations that must be applied together anyway:
--
--   * password_resets       — single-use, expiring, mailed recovery tokens.
--   * user_identities       — links a local account to an external OIDC subject.
--   * webauthn_credentials  — registered passkeys (public material only).
--   * webauthn_challenges   — short-lived server-side ceremony state.
--   * users.webauthn_handle — the opaque, stable WebAuthn user handle.
--
-- Three rules run through all of it:
--   1. A secret handed to a client is stored only as its SHA-256, exactly as
--      sessions.id (0001) and api_keys.key_hash (0003) already do. No column
--      here holds a plaintext token.
--   2. Everything hangs off users with ON DELETE CASCADE. Deleting a user must
--      not leave a live credential, a redeemable reset token or a dangling
--      ceremony behind — a credential outliving its account is a login for an
--      account that no longer exists.
--   3. Expiring rows carry an explicit expires_at and an index on it. There is
--      no reaper in this codebase; readers treat an expired row as absent and
--      delete it there (the shape principalFromSession already uses for
--      sessions), and the index keeps that read cheap.

BEGIN;

-- ---------------------------------------------------------------------------
-- Password reset by e-mail
-- ---------------------------------------------------------------------------

-- token_hash is the SHA-256 of the token that went out in the mail; the
-- plaintext exists only in the handler variable and the mail body. used_at is
-- what makes a token single-use — it is set in the same transaction as the
-- password write, so a token cannot be redeemed twice under concurrency.
-- Consumed rows are kept rather than deleted so a replay is distinguishable
-- from an unknown token internally, even though both answer identically.
CREATE TABLE password_resets (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id      bigint      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash   text        NOT NULL UNIQUE, -- SHA-256 of the mailed token
    expires_at   timestamptz NOT NULL,
    used_at      timestamptz,                 -- non-NULL = consumed, single-use
    requested_ip inet,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX password_resets_user_id_idx    ON password_resets (user_id);
CREATE INDEX password_resets_expires_at_idx ON password_resets (expires_at);

-- ---------------------------------------------------------------------------
-- External identities (OIDC / SSO)
-- ---------------------------------------------------------------------------

-- The unique index is on (issuer, subject), NOT (provider, subject): provider
-- is a local label an operator may rename, while issuer is the identity
-- provider's own cryptographic identity and is what the flow pins. Two
-- configured providers pointing at the same issuer therefore collide, which is
-- correct — they describe the same authority.
--
-- email here is display only. It is refreshed on each login and must NEVER be
-- a lookup key: resolving a local account from an IdP-asserted address would
-- let a hostile or compromised provider take over any account by asserting its
-- address. (issuer, subject) is the only join key from an assertion to a user.
CREATE TABLE user_identities (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id       bigint      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    provider      text        NOT NULL, -- local config label, renameable
    issuer        text        NOT NULL, -- IdP issuer URL, pinned
    subject       text        NOT NULL, -- IdP 'sub' claim, stable per user
    email         text        NOT NULL DEFAULT '', -- display only, never a key
    display_name  text        NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_login_at timestamptz
);

CREATE UNIQUE INDEX user_identities_issuer_subject_idx ON user_identities (issuer, subject);
CREATE INDEX user_identities_user_id_idx ON user_identities (user_id);

-- ---------------------------------------------------------------------------
-- Passkeys (WebAuthn)
-- ---------------------------------------------------------------------------

-- The WebAuthn user handle is echoed to any page on the relying-party origin
-- during a discoverable assertion, so it must be opaque and non-enumerable:
-- users.id and users.email are both disqualified. It must also be stable
-- across all of a user's credentials, which is why it lives here and not on
-- the credential row. Nullable because it is minted lazily, with crypto/rand,
-- on the first passkey registration.
ALTER TABLE users ADD COLUMN webauthn_handle bytea UNIQUE;

-- Only public material is stored: the credential ID, the COSE public key, the
-- AAGUID, the signature counter and the backup flags. Nothing in this table
-- would let the server impersonate the authenticator.
--
-- credential_id is UNIQUE and that is load-bearing, not hygiene: discoverable
-- (usernameless) login resolves a credential by raw ID alone, with no user
-- context, so a duplicate would be ambiguous.
--
-- sign_count is written after every successful assertion. A counter that moves
-- backwards means a cloned authenticator; clone_warning records that verdict
-- so it survives the request that discovered it.
CREATE TABLE webauthn_credentials (
    id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id          bigint      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    credential_id    bytea       NOT NULL UNIQUE, -- raw authenticator credential ID
    public_key       bytea       NOT NULL,        -- COSE public key, public material only
    attestation_type text        NOT NULL DEFAULT 'none',
    transports       jsonb       NOT NULL DEFAULT '[]'::jsonb,
    aaguid           bytea,
    sign_count       bigint      NOT NULL DEFAULT 0,
    backup_eligible  boolean     NOT NULL DEFAULT false,
    backup_state     boolean     NOT NULL DEFAULT false,
    clone_warning    boolean     NOT NULL DEFAULT false,
    name             text        NOT NULL DEFAULT '',
    last_used_at     timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX webauthn_credentials_user_id_idx ON webauthn_credentials (user_id);

-- Both WebAuthn ceremonies are two round trips, so the challenge the server
-- issued must survive between them. It is kept server-side and keyed by the
-- SHA-256 of an opaque cookie token — the same never-store-the-token rule as
-- sessions.id — rather than being handed to the client, which would make it
-- forgeable.
--
-- user_id is NULLABLE: a discoverable-login ceremony has no known user at the
-- moment the challenge is issued. It is still ON DELETE CASCADE so a
-- registration ceremony cannot outlive the account it was started from.
--
-- Rows are single-use (deleted as part of the consuming read) and expire after
-- a few minutes; an expired row is treated as absent.
CREATE TABLE webauthn_challenges (
    id         text        PRIMARY KEY, -- SHA-256 of the ceremony cookie token
    user_id    bigint      REFERENCES users (id) ON DELETE CASCADE, -- NULL for discoverable login
    kind       text        NOT NULL CHECK (kind IN ('register', 'login')),
    data       jsonb       NOT NULL,    -- marshalled ceremony session data
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);

CREATE INDEX webauthn_challenges_expires_at_idx ON webauthn_challenges (expires_at);
CREATE INDEX webauthn_challenges_user_id_idx    ON webauthn_challenges (user_id);

COMMIT;
