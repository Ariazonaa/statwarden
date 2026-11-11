package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Ariazonaa/statwarden/internal/auth"
)

// This file holds the PgStore side of the account-security surface: external
// (OIDC) identities and password-reset tokens. It follows the split-file
// convention of pgstore_pages.go and pgstore_incidents.go rather than growing
// pgstore.go.

// --- external identities ---

const identityCols = `id, user_id, provider, issuer, subject, email, display_name, created_at, last_login_at`

func scanIdentity(row pgx.Row) (ExternalIdentity, error) {
	var e ExternalIdentity
	err := row.Scan(&e.ID, &e.UserID, &e.Provider, &e.Issuer, &e.Subject, &e.Email, &e.DisplayName,
		&e.CreatedAt, &e.LastLoginAt)
	return e, err
}

// GetIdentity resolves an assertion to a local account by (issuer, subject) and
// by nothing else. There is deliberately no lookup by e-mail on this table: the
// address is IdP-asserted display data, and matching on it would let a hostile
// or compromised provider take over any local account.
func (s *PgStore) GetIdentity(ctx context.Context, issuer, subject string) (ExternalIdentity, error) {
	if issuer == "" || subject == "" {
		return ExternalIdentity{}, ErrNotFound
	}
	e, err := scanIdentity(s.pool.QueryRow(ctx,
		`SELECT `+identityCols+` FROM user_identities WHERE issuer = $1 AND subject = $2`, issuer, subject))
	return e, mapErr(err)
}

func (s *PgStore) CreateIdentity(ctx context.Context, in ExternalIdentity) (ExternalIdentity, error) {
	e, err := scanIdentity(s.pool.QueryRow(ctx, `
		INSERT INTO user_identities (user_id, provider, issuer, subject, email, display_name, last_login_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING `+identityCols,
		in.UserID, in.Provider, in.Issuer, in.Subject, in.Email, in.DisplayName, in.LastLoginAt))
	return e, mapErr(err)
}

func (s *PgStore) ListIdentitiesByUser(ctx context.Context, userID int64) ([]ExternalIdentity, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+identityCols+` FROM user_identities WHERE user_id = $1 ORDER BY id`, userID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []ExternalIdentity{}
	for rows.Next() {
		e, err := scanIdentity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *PgStore) TouchIdentity(ctx context.Context, id int64, at time.Time, email, displayName string) error {
	ct, err := s.pool.Exec(ctx, `
		UPDATE user_identities SET last_login_at = $2, email = $3, display_name = $4 WHERE id = $1`,
		id, at, email, displayName)
	return affected(ct, err)
}

func (s *PgStore) DeleteIdentity(ctx context.Context, id, userID int64) error {
	ct, err := s.pool.Exec(ctx, `DELETE FROM user_identities WHERE id = $1 AND user_id = $2`, id, userID)
	return affected(ct, err)
}

// --- password resets ---

const passwordResetCols = `id, user_id, token_hash, expires_at, used_at, requested_ip, created_at`

func scanPasswordReset(row pgx.Row) (PasswordReset, error) {
	var r PasswordReset
	var ip *netip.Addr
	err := row.Scan(&r.ID, &r.UserID, &r.TokenHash, &r.ExpiresAt, &r.UsedAt, &ip, &r.CreatedAt)
	if err != nil {
		return PasswordReset{}, err
	}
	r.RequestedIP = ipString(ip)
	return r, nil
}

func (s *PgStore) CreatePasswordReset(ctx context.Context, r PasswordReset) (PasswordReset, error) {
	out, err := scanPasswordReset(s.pool.QueryRow(ctx, `
		INSERT INTO password_resets (user_id, token_hash, expires_at, requested_ip, created_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+passwordResetCols,
		r.UserID, r.TokenHash, r.ExpiresAt, ipPtr(r.RequestedIP), r.CreatedAt))
	return out, mapErr(err)
}

func (s *PgStore) ConsumePasswordReset(ctx context.Context, tokenHash, passwordHash string, now time.Time) (PasswordReset, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PasswordReset{}, mapErr(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// The guard lives in the UPDATE's WHERE clause, not in a preceding SELECT, so
	// the row lock serialises two concurrent redemptions of the same token: the
	// loser matches zero rows because used_at is already set. Unknown, expired
	// and already-used all land on the same pgx.ErrNoRows and become ErrNotFound.
	r, err := scanPasswordReset(tx.QueryRow(ctx, `
		UPDATE password_resets SET used_at = $2
		WHERE token_hash = $1 AND used_at IS NULL AND expires_at > $2
		RETURNING `+passwordResetCols, tokenHash, now))
	if err != nil {
		return PasswordReset{}, mapErr(err)
	}

	ct, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1`, r.UserID, passwordHash)
	if err := affected(ct, err); err != nil {
		return PasswordReset{}, err
	}

	// A stale mail in an old inbox must not be replayable once the account has
	// been recovered.
	if _, err := tx.Exec(ctx, `
		UPDATE password_resets SET used_at = $2 WHERE user_id = $1 AND used_at IS NULL`, r.UserID, now); err != nil {
		return PasswordReset{}, mapErr(err)
	}

	if err := mapErr(tx.Commit(ctx)); err != nil {
		return PasswordReset{}, err
	}
	return r, nil
}

// PasswordResetRedeemable implements Store. The predicate is the one
// ConsumePasswordReset's UPDATE uses, as a read; it takes no lock because the
// redemption re-checks under its own.
func (s *PgStore) PasswordResetRedeemable(ctx context.Context, tokenHash string, now time.Time) (bool, error) {
	var ok bool
	// $1 token_hash, $2 now — two placeholders, two args.
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM password_resets
			WHERE token_hash = $1 AND used_at IS NULL AND expires_at > $2
		)`, tokenHash, now).Scan(&ok)
	return ok, mapErr(err)
}

// ConsumeBackupCode implements Store. SELECT … FOR UPDATE holds the row from the
// read of the codes to the write of what remains, so concurrent redemptions
// serialise: the second sees the list the first left behind.
func (s *PgStore) ConsumeBackupCode(ctx context.Context, userID int64, code string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, mapErr(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var raw []byte
	// $1 id — one placeholder, one arg.
	err = tx.QueryRow(ctx,
		`SELECT backup_codes FROM users WHERE id = $1 AND totp_enabled FOR UPDATE`, userID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, mapErr(err)
	}
	var stored []string
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &stored); err != nil {
			return false, err
		}
	}
	remaining, ok := auth.ConsumeBackupCode(stored, code)
	if !ok {
		return false, nil
	}
	// Same encoding SetUserTOTP writes: an empty list is stored as NULL.
	var codesArg []byte
	if len(remaining) > 0 {
		if codesArg, err = json.Marshal(remaining); err != nil {
			return false, err
		}
	}
	// $1 id, $2 backup_codes — two placeholders, two args.
	ct, err := tx.Exec(ctx,
		`UPDATE users SET backup_codes = $2, updated_at = now() WHERE id = $1`, userID, codesArg)
	if err := affected(ct, err); err != nil {
		return false, err
	}
	if err := mapErr(tx.Commit(ctx)); err != nil {
		return false, err
	}
	return true, nil
}
