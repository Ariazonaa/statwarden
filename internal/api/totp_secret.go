package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
)

// users.totp_secret is sealed at rest with the same SecretsBox that seals
// monitors.config and notifications.config; migration 0013 records the boundary.
//
// Backup codes have always been hashed, but the TOTP secret cannot be: verifying
// a code requires the shared secret itself, so the only protection available is
// encryption. Without it a database leak alone hands over every account's second
// factor, which is exactly the property 2FA is bought for.
//
// The box seals JSON documents, so the base32 secret is marshalled to a JSON
// string first and the stored form is the familiar {"$encrypted":"…"} envelope —
// the same shape configEncrypted already recognises.
//
// Four properties are load-bearing:
//
//   - Legacy rows are PLAINTEXT and must keep working. The AES key is derived
//     from ENCRYPTION_KEY, which lives only in the application process, so no SQL
//     migration can re-encrypt existing rows. Migration 0009 documented this
//     exact rollout for monitors.config and 0013 repeats it here: a value that is
//     not an envelope is read through unchanged, and every write re-seals, so a
//     legacy row is upgraded the next time it is touched.
//   - Writes fail CLOSED. setUserTOTP propagates a seal error instead of falling
//     back to persisting the plaintext.
//   - DEV_MODE has no box (s.box == nil). Both directions are then identities and
//     the column keeps the plaintext it has always held.
//   - open never yields the EMPTY string for a non-empty column. auth.VerifyTOTP
//     base32-decodes "" successfully and then verifies against an empty HMAC key,
//     so an unreadable secret degraded to "" would be a 2FA bypass anyone could
//     compute. An envelope that cannot be opened is therefore returned verbatim:
//     it contains '{' and '"', which base32 rejects, so it can never verify.

// sealTOTPSecret converts a plaintext TOTP secret into the value stored in
// users.totp_secret. With no box (DEV_MODE) or an empty secret it is the
// identity, matching what the column held before this boundary existed.
//
// Sealing is idempotent: a value that is ALREADY an envelope is one
// openTOTPSecret could not open (a changed ENCRYPTION_KEY) and handed back as it
// found it. Wrapping it a second time would bury an already unreadable secret
// under another layer, so it is stored exactly as it was read.
func (s *server) sealTOTPSecret(secret string) (string, error) {
	if s.box == nil || secret == "" || configEncrypted(json.RawMessage(secret)) {
		return secret, nil
	}
	raw, err := json.Marshal(secret)
	if err != nil {
		return "", fmt.Errorf("seal totp secret: %w", err)
	}
	sealed, err := s.box.EncryptConfig(raw)
	if err != nil {
		return "", fmt.Errorf("seal totp secret: %w", err)
	}
	return string(sealed), nil
}

// openTOTPSecret converts a stored users.totp_secret back to the base32 secret
// auth.VerifyTOTP needs. A value that is not an envelope — a legacy plaintext
// row, or anything written while DEV_MODE ran without a box — passes through
// unchanged. An envelope that cannot be opened is returned as-is rather than as
// "", so it fails verification instead of matching an empty-key HOTP.
func (s *server) openTOTPSecret(stored string) string {
	if stored == "" || !configEncrypted(json.RawMessage(stored)) {
		return stored
	}
	if s.box == nil {
		return stored
	}
	plain, err := s.box.DecryptConfig(json.RawMessage(stored))
	if err != nil {
		return stored
	}
	var secret string
	if err := json.Unmarshal(plain, &secret); err != nil {
		return stored
	}
	return secret
}

// credentialsByID is the ONLY way a handler reads a user's credentials by id.
// Going through it is what makes "every read of the TOTP secret opens it" true
// by construction rather than by review; totp_secret_test.go guards the rule at
// the source level.
func (s *server) credentialsByID(ctx context.Context, id int64) (Credentials, error) {
	c, err := s.store.GetCredentialsByID(ctx, id)
	if err != nil {
		return Credentials{}, err
	}
	c.TOTPSecret = s.openTOTPSecret(c.TOTPSecret)
	return c, nil
}

// credentialsByLogin is credentialsByID's counterpart for the password login
// path, which resolves an account by e-mail OR username. The login path reads
// the TOTP secret too, so it needs the same opening.
//
// There is deliberately no credentialsByEmail: GetCredentialsByEmail is the name
// the OIDC source-level guard in oidc_test.go greps for, and hiding it behind a
// wrapper would blunt that guard. Its one caller (the password-reset request
// handler) never touches the TOTP secret.
func (s *server) credentialsByLogin(ctx context.Context, login string) (Credentials, error) {
	c, err := s.store.GetCredentialsByLogin(ctx, login)
	if err != nil {
		return Credentials{}, err
	}
	c.TOTPSecret = s.openTOTPSecret(c.TOTPSecret)
	return c, nil
}

// setUserTOTP is the ONLY way a handler writes users.totp_secret. It takes the
// PLAINTEXT secret and seals it, so a legacy plaintext row is re-sealed by the
// first write that touches it. A seal failure aborts the write: persisting the
// plaintext instead would silently undo the whole boundary.
func (s *server) setUserTOTP(ctx context.Context, id int64, secret string, enabled bool, codes []string) error {
	sealed, err := s.sealTOTPSecret(secret)
	if err != nil {
		return err
	}
	return s.store.SetUserTOTP(ctx, id, sealed, enabled, codes)
}

// ResealTOTPSecrets seals any TOTP secret still stored in plaintext and reports
// how many it changed. Call it once at startup, after migrations.
//
// A package-level function rather than a server method: it needs a store, a box
// and somewhere to log, and running it should not require standing up an HTTP
// handler first.
//
// Migration 0013 introduced at-rest encryption for users.totp_secret but could
// not rewrite existing rows: the key is derived from ENCRYPTION_KEY, which lives
// in this process and is deliberately never available to SQL. It relied instead
// on each account's NEXT write to re-seal — enrolment, verification, disable,
// backup-code regeneration. An account with 2FA enabled that nobody touches has
// no next write, so its secret would sit in the clear for as long as the
// account exists. Those are also the accounts least likely to notice, and a
// database leak takes the whole second factor with it, which is the precise
// property 2FA is bought for.
//
// Idempotent: sealTOTPSecret returns an existing envelope unchanged, so a
// second run finds nothing to do. With no box configured (DEV_MODE) it does
// nothing at all.
func ResealTOTPSecrets(ctx context.Context, store Store, box SecretsBox, logger *slog.Logger) (int, error) {
	if box == nil {
		return 0, nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &server{store: store, box: box, logger: logger}
	stored, err := s.store.StoredTOTPSecrets(ctx)
	if err != nil {
		return 0, fmt.Errorf("list TOTP secrets: %w", err)
	}
	sealed := 0
	for userID, secret := range stored {
		if configEncrypted(json.RawMessage(secret)) {
			continue
		}
		env, serr := s.sealTOTPSecret(secret)
		if serr != nil {
			// One unreadable row must not stop the rest: the point of the pass
			// is to reduce the number of plaintext secrets, and stopping at the
			// first problem leaves the others as they were.
			logger.Error("could not seal a legacy TOTP secret", "user_id", userID, "err", serr)
			continue
		}
		if uerr := s.store.UpdateTOTPSecret(ctx, userID, env); uerr != nil {
			logger.Error("could not store a sealed TOTP secret", "user_id", userID, "err", uerr)
			continue
		}
		sealed++
	}
	return sealed, nil
}
