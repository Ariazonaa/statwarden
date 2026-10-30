package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PgStore implements Store against PostgreSQL. It uses pgx directly (the same
// pool the engine and dispatcher share); the query surface is small and
// hand-written for clarity. MemStore mirrors its behaviour for tests and
// DEV_MODE.
type PgStore struct {
	pool *pgxpool.Pool
}

// NewPgStore wraps a pgx pool as a Store.
func NewPgStore(pool *pgxpool.Pool) *PgStore { return &PgStore{pool: pool} }

// mapErr normalises pgx/postgres errors into the store sentinels.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
		return ErrConflict
	}
	return err
}

func ipPtr(ip string) *netip.Addr {
	if addr, err := netip.ParseAddr(ip); err == nil {
		return &addr
	}
	return nil
}

func ipString(addr *netip.Addr) string {
	if addr == nil || !addr.IsValid() {
		return ""
	}
	return addr.String()
}

// ErrLastAdmin is returned by the guarded user mutations when the write would
// leave the instance with no active administrator. Handlers translate it to a
// 409. It is a store sentinel like ErrNotFound/ErrConflict, kept here because
// it is only ever produced by the guarded store operations below.
var ErrLastAdmin = errors.New("at least one active admin must remain")

// Compile-time assertions that both stores satisfy the admin extension
// interfaces the handlers type-assert for. The base Store interface lives
// elsewhere; these guarantee a missing guarded method is a build error rather
// than a runtime type-assertion miss.
var (
	_ adminGuardStore     = (*PgStore)(nil)
	_ adminGuardStore     = (*MemStore)(nil)
	_ adminBootstrapStore = (*PgStore)(nil)
	_ adminBootstrapStore = (*MemStore)(nil)
)

// usersAdminLockKey is the pg_advisory_xact_lock key that serialises the two
// admin invariants — first-admin bootstrap and the last-active-admin guard —
// so a check and the write that depends on it cannot interleave with a
// concurrent one and both pass. The value is arbitrary ("statwarden" in ASCII); it
// only has to be stable and unshared with other advisory-lock users.
const usersAdminLockKey int64 = 0x757074757261

// --- users ---

func (s *PgStore) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, mapErr(err)
}

func (s *PgStore) CreateUser(ctx context.Context, email, username, hash, displayName, role string) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx, `
		INSERT INTO users (email, username, password_hash, display_name, role)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, email, username, display_name, role, totp_enabled, is_active, created_at, updated_at`,
		email, username, hash, displayName, role).
		Scan(&u.ID, &u.Email, &u.Username, &u.DisplayName, &u.Role, &u.TOTPEnabled, &u.IsActive, &u.CreatedAt, &u.UpdatedAt)
	return u, mapErr(err)
}

// StoredTOTPSecrets implements Store.
func (s *PgStore) StoredTOTPSecrets(ctx context.Context) (map[int64]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, totp_secret FROM users WHERE totp_secret IS NOT NULL AND totp_secret <> ''`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var (
			id     int64
			secret string
		)
		if err := rows.Scan(&id, &secret); err != nil {
			return nil, mapErr(err)
		}
		out[id] = secret
	}
	return out, mapErr(rows.Err())
}

// UpdateTOTPSecret implements Store.
func (s *PgStore) UpdateTOTPSecret(ctx context.Context, userID int64, secret string) error {
	ct, err := s.pool.Exec(ctx,
		`UPDATE users SET totp_secret = $2, updated_at = now() WHERE id = $1`, userID, secret)
	return affected(ct, err)
}

// CreateUserWithIdentity implements Store.
func (s *PgStore) CreateUserWithIdentity(
	ctx context.Context, email, username, displayName, role string, id ExternalIdentity,
) (User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, mapErr(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var u User
	// $1 email, $2 username, $3 display_name, $4 role — the password hash is
	// deliberately the empty string: auth.VerifyPassword rejects anything that
	// is not a well-formed argon2id value, so this is "no local password"
	// rather than a weak one.
	err = tx.QueryRow(ctx, `
		INSERT INTO users (email, username, password_hash, display_name, role)
		VALUES ($1, $2, '', $3, $4)
		RETURNING id, email, username, display_name, role, totp_enabled, is_active, created_at, updated_at`,
		email, username, displayName, role).
		Scan(&u.ID, &u.Email, &u.Username, &u.DisplayName, &u.Role, &u.TOTPEnabled,
			&u.IsActive, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return User{}, mapErr(err)
	}

	// $1..$6 — six placeholders, six args.
	if _, err := tx.Exec(ctx, `
		INSERT INTO user_identities (user_id, provider, issuer, subject, email, display_name, last_login_at)
		VALUES ($1, $2, $3, $4, $5, $6, now())`,
		u.ID, id.Provider, id.Issuer, id.Subject, id.Email, id.DisplayName); err != nil {
		return User{}, mapErr(err)
	}
	if err := mapErr(tx.Commit(ctx)); err != nil {
		return User{}, err
	}
	return u, nil
}

// CreateInitialAdmin creates the first-run admin atomically: the "no users yet"
// check and the insert run in one advisory-locked transaction, so two
// concurrent setup requests that both observed an empty table cannot each
// create an admin — the loser gets ErrConflict. It is the only writer the setup
// endpoint uses.
func (s *PgStore) CreateInitialAdmin(ctx context.Context, email, username, hash, displayName string) (User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, mapErr(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, usersAdminLockKey); err != nil {
		return User{}, mapErr(err)
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		return User{}, mapErr(err)
	}
	if n > 0 {
		return User{}, ErrConflict
	}
	var u User
	err = tx.QueryRow(ctx, `
		INSERT INTO users (email, username, password_hash, display_name, role)
		VALUES ($1, $2, $3, $4, 'admin')
		RETURNING id, email, username, display_name, role, totp_enabled, is_active, created_at, updated_at`,
		email, username, hash, displayName).
		Scan(&u.ID, &u.Email, &u.Username, &u.DisplayName, &u.Role, &u.TOTPEnabled, &u.IsActive, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return User{}, mapErr(err)
	}
	if err := mapErr(tx.Commit(ctx)); err != nil {
		return User{}, err
	}
	return u, nil
}

const userCols = `id, email, username, display_name, role, totp_enabled, avatar_path, locale, is_active, created_at, updated_at`

// avatarURLPrefix is the public URL path uploaded avatars are served under, and
// the only prefix a stored avatar_path may carry. Avatars live in their own
// directory and URL space so they never share the logo store's temp-file
// namespace and cannot be served under the logo route.
const avatarURLPrefix = "/uploads/avatars/"

// sanitizeAvatarPath returns p when it is a path this server issued, else "".
// It is the read-boundary guard scanUser applies, mirroring what
// sanitizeLogoPath does for status-page logos: avatar_path has been a free
// nullable text column since migration 0001 with no writer at all, so any value
// a real database holds today came from outside this application — and the
// frontend interpolates it straight into an <img src>. The name shape is the
// same as a logo's (server-generated id-hex.ext), so logoNameRe is reused.
func sanitizeAvatarPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	name, ok := strings.CutPrefix(p, avatarURLPrefix)
	if !ok || !logoNameRe.MatchString(name) {
		return ""
	}
	return p
}

func scanUser(row pgx.Row) (User, error) {
	var u User
	var avatar, locale *string
	err := row.Scan(&u.ID, &u.Email, &u.Username, &u.DisplayName, &u.Role, &u.TOTPEnabled, &avatar, &locale, &u.IsActive, &u.CreatedAt, &u.UpdatedAt)
	if avatar != nil {
		u.AvatarPath = sanitizeAvatarPath(*avatar)
	}
	if locale != nil {
		u.Locale = *locale
	}
	return u, err
}

func (s *PgStore) GetUserByID(ctx context.Context, id int64) (User, error) {
	u, err := scanUser(s.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id = $1`, id))
	return u, mapErr(err)
}

func (s *PgStore) scanCredentials(row pgx.Row) (Credentials, error) {
	var c Credentials
	var totpSecret *string
	var backupCodes []byte
	err := row.Scan(&c.ID, &c.Email, &c.Username, &c.DisplayName, &c.Role, &c.TOTPEnabled, &c.IsActive,
		&c.CreatedAt, &c.UpdatedAt, &c.PasswordHash, &totpSecret, &backupCodes)
	if err != nil {
		return Credentials{}, err
	}
	if totpSecret != nil {
		c.TOTPSecret = *totpSecret
	}
	if len(backupCodes) > 0 {
		_ = json.Unmarshal(backupCodes, &c.BackupCodes)
	}
	return c, nil
}

const credCols = `id, email, username, display_name, role, totp_enabled, is_active, created_at, updated_at, password_hash, totp_secret, backup_codes`

func (s *PgStore) GetCredentialsByEmail(ctx context.Context, email string) (Credentials, error) {
	c, err := s.scanCredentials(s.pool.QueryRow(ctx, `SELECT `+credCols+` FROM users WHERE lower(email) = lower($1)`, email))
	return c, mapErr(err)
}

// GetCredentialsByLogin resolves an account by e-mail OR username, both matched
// case-insensitively — the login path's single lookup. Each column carries its
// own case-insensitive unique index, so no more than one row matches per column.
func (s *PgStore) GetCredentialsByLogin(ctx context.Context, login string) (Credentials, error) {
	c, err := s.scanCredentials(s.pool.QueryRow(ctx,
		`SELECT `+credCols+` FROM users WHERE lower(email) = lower($1) OR lower(username) = lower($1)`, login))
	return c, mapErr(err)
}

func (s *PgStore) GetCredentialsByID(ctx context.Context, id int64) (Credentials, error) {
	c, err := s.scanCredentials(s.pool.QueryRow(ctx, `SELECT `+credCols+` FROM users WHERE id = $1`, id))
	return c, mapErr(err)
}

func (s *PgStore) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+userCols+` FROM users ORDER BY id`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *PgStore) UpdateUser(ctx context.Context, id int64, email, username, displayName, role string, isActive bool) (User, error) {
	u, err := scanUser(s.pool.QueryRow(ctx, `
		UPDATE users SET email = $2, username = $3, display_name = $4, role = $5, is_active = $6, updated_at = now()
		WHERE id = $1
		RETURNING `+userCols, id, email, username, displayName, role, isActive))
	return u, mapErr(err)
}

func (s *PgStore) UpdateUserPassword(ctx context.Context, id int64, hash string) error {
	ct, err := s.pool.Exec(ctx, `UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1`, id, hash)
	return affected(ct, err)
}

// ChangePassword implements Store. The three writes share one transaction so
// there is no state in which the password has moved but what the old one could
// still reach has not.
func (s *PgStore) ChangePassword(ctx context.Context, id int64, hash, keepSessionID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return mapErr(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// $1 id, $2 hash — two placeholders, two args.
	ct, err := tx.Exec(ctx,
		`UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1`, id, hash)
	if err := affected(ct, err); err != nil {
		return err
	}
	// $1 user_id, $2 the session to keep. An empty keepSessionID matches no row,
	// so every session goes — which is what the recovery path wants.
	if _, err := tx.Exec(ctx,
		`DELETE FROM sessions WHERE user_id = $1 AND id <> $2`, id, keepSessionID); err != nil {
		return mapErr(err)
	}
	// API keys go too.
	//
	// This was missing while the password-RESET path did it, and that path's own
	// comment says why: "A surviving API key is arguably the worse of them: it
	// outlives every session by design and the owner cannot see it." Someone who
	// borrows a session for a minute mints a key; the victim changes their
	// password, is told their other sessions are gone, and the key keeps full
	// access indefinitely with nothing on screen to suggest it exists.
	if _, err := tx.Exec(ctx, `DELETE FROM api_keys WHERE user_id = $1`, id); err != nil {
		return mapErr(err)
	}
	// $1 user_id. now() rather than a passed clock: this is the same statement
	// ConsumePasswordReset uses, and both are about wall-clock invalidation.
	if _, err := tx.Exec(ctx,
		`UPDATE password_resets SET used_at = now() WHERE user_id = $1 AND used_at IS NULL`,
		id); err != nil {
		return mapErr(err)
	}
	return mapErr(tx.Commit(ctx))
}

// InvalidatePasswordResets implements Store.
func (s *PgStore) InvalidatePasswordResets(ctx context.Context, userID int64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE password_resets SET used_at = now() WHERE user_id = $1 AND used_at IS NULL`, userID)
	return mapErr(err)
}

func (s *PgStore) SetUserTOTP(ctx context.Context, id int64, secret string, enabled bool, codes []string) error {
	var secretArg *string
	if secret != "" {
		secretArg = &secret
	}
	var codesArg []byte
	if len(codes) > 0 {
		codesArg, _ = json.Marshal(codes)
	}
	ct, err := s.pool.Exec(ctx, `
		UPDATE users SET totp_secret = $2, totp_enabled = $3, backup_codes = $4, updated_at = now()
		WHERE id = $1`, id, secretArg, enabled, codesArg)
	return affected(ct, err)
}

// SetUserAvatar updates only avatar_path and reports the path it replaced. It
// mirrors SetStatusPageLogo: UpdateUser is full-replace and never touches the
// column, so this is the only writer of avatar_path.
func (s *PgStore) SetUserAvatar(ctx context.Context, id int64, avatarPath string) (User, string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, "", mapErr(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	// The prior path is read under the row lock the update takes anyway, so two
	// overlapping uploads serialise here: each learns the file the other actually
	// left behind rather than a value read before the body was consumed.
	var prev *string
	if err := tx.QueryRow(ctx, `SELECT avatar_path FROM users WHERE id=$1 FOR UPDATE`, id).Scan(&prev); err != nil {
		return User{}, "", mapErr(err)
	}
	u, err := scanUser(tx.QueryRow(ctx,
		`UPDATE users SET avatar_path=$2, updated_at=now() WHERE id=$1 RETURNING `+userCols,
		id, nilIfEmpty(avatarPath)))
	if err != nil {
		return User{}, "", mapErr(err)
	}
	if err := mapErr(tx.Commit(ctx)); err != nil {
		return User{}, "", err
	}
	// The replaced path is returned raw, not sanitised: it names a file on disk,
	// and the store's Remove already ignores anything this server did not issue.
	return u, strDeref(prev), nil
}

func (s *PgStore) DeleteUser(ctx context.Context, id int64) error {
	ct, err := s.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
	return affected(ct, err)
}

// assertActiveAdminTx returns ErrLastAdmin unless at least one active admin
// remains in the transaction's view. The advisory lock the caller holds makes
// this check-then-commit atomic against concurrent admin mutations, so two
// operations cannot each see the other's admin and both proceed.
func (s *PgStore) assertActiveAdminTx(ctx context.Context, tx pgx.Tx) error {
	var admins int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM users WHERE role = 'admin' AND is_active`).Scan(&admins); err != nil {
		return mapErr(err)
	}
	if admins == 0 {
		return ErrLastAdmin
	}
	return nil
}

// UpdateUserGuarded is UpdateUser plus the last-active-admin invariant: the
// write and the check that an active admin still remains happen in one
// advisory-locked transaction, so concurrent demotions/deactivations cannot
// each observe the other's admin and both commit. Returns ErrLastAdmin when the
// write would leave no active admin.
func (s *PgStore) UpdateUserGuarded(ctx context.Context, id int64, email, username, displayName, role string, isActive bool) (User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, mapErr(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, usersAdminLockKey); err != nil {
		return User{}, mapErr(err)
	}
	u, err := scanUser(tx.QueryRow(ctx, `
		UPDATE users SET email = $2, username = $3, display_name = $4, role = $5, is_active = $6, updated_at = now()
		WHERE id = $1
		RETURNING `+userCols, id, email, username, displayName, role, isActive))
	if err != nil {
		return User{}, mapErr(err)
	}
	if err := s.assertActiveAdminTx(ctx, tx); err != nil {
		return User{}, err
	}
	if err := mapErr(tx.Commit(ctx)); err != nil {
		return User{}, err
	}
	return u, nil
}

// DeleteUserGuarded is DeleteUser plus the last-active-admin invariant, enforced
// in the same advisory-locked transaction as the delete.
func (s *PgStore) DeleteUserGuarded(ctx context.Context, id int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return mapErr(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, usersAdminLockKey); err != nil {
		return mapErr(err)
	}
	ct, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
	if err := affected(ct, err); err != nil {
		return err
	}
	if err := s.assertActiveAdminTx(ctx, tx); err != nil {
		return err
	}
	return mapErr(tx.Commit(ctx))
}

// --- sessions ---

func (s *PgStore) CreateSession(ctx context.Context, sess Session) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO sessions (id, user_id, created_at, expires_at, last_seen_at, ip, user_agent)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		sess.ID, sess.UserID, sess.CreatedAt, sess.ExpiresAt, sess.LastSeenAt, ipPtr(sess.IP), sess.UserAgent)
	return mapErr(err)
}

func (s *PgStore) GetSession(ctx context.Context, id string) (Session, error) {
	var sess Session
	var ip *netip.Addr
	var ua *string
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, created_at, expires_at, last_seen_at, ip, user_agent
		FROM sessions WHERE id = $1`, id).
		Scan(&sess.ID, &sess.UserID, &sess.CreatedAt, &sess.ExpiresAt, &sess.LastSeenAt, &ip, &ua)
	if err != nil {
		return Session{}, mapErr(err)
	}
	sess.IP = ipString(ip)
	if ua != nil {
		sess.UserAgent = *ua
	}
	return sess, nil
}

func (s *PgStore) TouchSession(ctx context.Context, id string, lastSeen, expires time.Time) error {
	ct, err := s.pool.Exec(ctx, `UPDATE sessions SET last_seen_at = $2, expires_at = $3 WHERE id = $1`, id, lastSeen, expires)
	return affected(ct, err)
}

func (s *PgStore) DeleteSession(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, id)
	return mapErr(err)
}

func (s *PgStore) ListSessionsByUser(ctx context.Context, userID int64) ([]Session, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, created_at, expires_at, last_seen_at, ip, user_agent
		FROM sessions WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		var sess Session
		var ip *netip.Addr
		var ua *string
		if err := rows.Scan(&sess.ID, &sess.UserID, &sess.CreatedAt, &sess.ExpiresAt, &sess.LastSeenAt, &ip, &ua); err != nil {
			return nil, err
		}
		sess.IP = ipString(ip)
		if ua != nil {
			sess.UserAgent = *ua
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

func (s *PgStore) DeleteSessionsByUser(ctx context.Context, userID int64, exceptID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1 AND id <> $2`, userID, exceptID)
	return mapErr(err)
}

// --- api keys ---

func (s *PgStore) CreateAPIKey(ctx context.Context, k APIKey) (APIKey, error) {
	scopes, _ := json.Marshal(k.Scopes)
	err := s.pool.QueryRow(ctx, `
		INSERT INTO api_keys (user_id, name, prefix, key_hash, scopes, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, created_at`,
		k.UserID, k.Name, k.Prefix, k.KeyHash, scopes, k.ExpiresAt).
		Scan(&k.ID, &k.CreatedAt)
	return k, mapErr(err)
}

func scanAPIKey(row pgx.Row) (APIKey, error) {
	var k APIKey
	var scopes []byte
	err := row.Scan(&k.ID, &k.UserID, &k.Name, &k.Prefix, &k.KeyHash, &scopes, &k.LastUsedAt, &k.ExpiresAt, &k.CreatedAt)
	if err != nil {
		return APIKey{}, err
	}
	if len(scopes) > 0 {
		_ = json.Unmarshal(scopes, &k.Scopes)
	}
	return k, nil
}

const apiKeyCols = `id, user_id, name, prefix, key_hash, scopes, last_used_at, expires_at, created_at`

func (s *PgStore) GetAPIKeyByHash(ctx context.Context, hash string) (APIKey, error) {
	k, err := scanAPIKey(s.pool.QueryRow(ctx, `SELECT `+apiKeyCols+` FROM api_keys WHERE key_hash = $1`, hash))
	return k, mapErr(err)
}

func (s *PgStore) ListAPIKeys(ctx context.Context, userID int64) ([]APIKey, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+apiKeyCols+` FROM api_keys WHERE user_id = $1 ORDER BY id`, userID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []APIKey{}
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *PgStore) TouchAPIKey(ctx context.Context, id int64, at time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE api_keys SET last_used_at = $2 WHERE id = $1`, id, at)
	return mapErr(err)
}

func (s *PgStore) DeleteAPIKey(ctx context.Context, id, userID int64) error {
	ct, err := s.pool.Exec(ctx, `DELETE FROM api_keys WHERE id = $1 AND user_id = $2`, id, userID)
	return affected(ct, err)
}

func (s *PgStore) DeleteAPIKeysByUser(ctx context.Context, userID int64) (int, error) {
	// One placeholder, one arg. Deleting nothing is success here, unlike
	// DeleteAPIKey: an account with no keys is a perfectly good outcome for a
	// revoke-everything call.
	ct, err := s.pool.Exec(ctx, `DELETE FROM api_keys WHERE user_id = $1`, userID)
	if err != nil {
		return 0, mapErr(err)
	}
	return int(ct.RowsAffected()), nil
}

// --- badge tokens ---

func (s *PgStore) EnsureBadgeToken(ctx context.Context, monitorID int64, candidate string) (string, error) {
	var token string
	// The no-op DO UPDATE makes RETURNING fire for both the insert and the
	// conflict case, so the effective token (existing or freshly minted) comes
	// back in one round trip without a separate read.
	err := s.pool.QueryRow(ctx, `
		INSERT INTO badge_tokens (monitor_id, token)
		VALUES ($1, $2)
		ON CONFLICT (monitor_id) DO UPDATE SET monitor_id = EXCLUDED.monitor_id
		RETURNING token`, monitorID, candidate).Scan(&token)
	return token, mapErr(err)
}

func (s *PgStore) DeleteBadgeToken(ctx context.Context, monitorID int64) error {
	// Idempotent revoke: a missing row is not an error.
	_, err := s.pool.Exec(ctx, `DELETE FROM badge_tokens WHERE monitor_id = $1`, monitorID)
	return mapErr(err)
}

func (s *PgStore) MonitorIDByBadgeToken(ctx context.Context, token string) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `SELECT monitor_id FROM badge_tokens WHERE token = $1`, token).Scan(&id)
	return id, mapErr(err)
}

// affected turns a zero-row-affected exec into ErrNotFound.
func affected(ct pgconn.CommandTag, err error) error {
	if err != nil {
		return mapErr(err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
