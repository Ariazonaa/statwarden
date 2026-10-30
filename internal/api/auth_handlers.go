package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/Ariazonaa/statwarden/internal/auth"
)

// adminBootstrapStore is the atomic first-admin creation the setup endpoint
// needs on top of the base Store: it serialises the "no users yet" check with
// the insert so concurrent setup requests cannot each create an admin. Both
// MemStore and PgStore implement it.
type adminBootstrapStore interface {
	CreateInitialAdmin(ctx context.Context, email, username, passwordHash, displayName string) (User, error)
}

// handleSetupStatus reports whether the first-run admin account still needs to
// be created. Public: the SPA calls it before showing the setup wizard.
func (s *server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	n, err := s.store.CountUsers(r.Context())
	if storeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"needs_setup": n == 0})
}

type setupRequest struct {
	Email       string `json:"email"`
	Username    string `json:"username"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
}

// handleSetup creates the initial admin account. It only works while no users
// exist, so it cannot be used to add accounts later.
func (s *server) handleSetup(w http.ResponseWriter, r *http.Request) {
	n, err := s.store.CountUsers(r.Context())
	if storeError(w, err) {
		return
	}
	if n > 0 {
		writeProblemDetail(w, http.StatusConflict, "setup already completed")
		return
	}
	var req setupRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := validateCredentials(req.Email, req.Password); err != nil {
		writeProblemDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	// A username is a real login key now. An explicit one is format-checked; an
	// omitted one is derived from the e-mail so the wizard still works headless.
	username := strings.TrimSpace(req.Username)
	if username == "" {
		username = deriveUsername(strings.TrimSpace(req.Email))
	} else if err := validateUsername(username); err != nil {
		writeProblemDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeProblemDetail(w, http.StatusInternalServerError, "hash password")
		return
	}
	bootstrap, ok := s.store.(adminBootstrapStore)
	if !ok {
		writeProblemDetail(w, http.StatusInternalServerError, "store does not support setup")
		return
	}
	// The initial admin is created under a serialised check-and-insert, so two
	// concurrent setup requests that both saw an empty table cannot both create
	// an admin: the loser gets ErrConflict here, which is the same "already
	// completed" outcome as the fast-path check above.
	u, err := bootstrap.CreateInitialAdmin(r.Context(), strings.TrimSpace(req.Email), username, hash, req.DisplayName)
	if errors.Is(err, ErrConflict) {
		writeProblemDetail(w, http.StatusConflict, "setup already completed")
		return
	}
	if storeError(w, err) {
		return
	}
	s.audit(r.Context(), &u.ID, "user.setup", "user", &u.ID, clientIP(r), map[string]string{"email": u.Email})
	if _, err := s.issueSession(w, r, u.ID); err != nil {
		writeProblemDetail(w, http.StatusInternalServerError, "create session")
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

type loginRequest struct {
	// Email is the login identifier and may be either an e-mail address or a
	// username; the field keeps its historical json key so the client contract is
	// unchanged. It is resolved by GetCredentialsByLogin, which matches either.
	Email      string `json:"email"`
	Password   string `json:"password"`
	TOTPCode   string `json:"totp_code"`
	BackupCode string `json:"backup_code"`
}

// handleLogin authenticates a username-or-email identifier plus password (and
// TOTP when enabled), enforcing per-IP rate limiting and per-account lockout. On
// success it issues a session.
func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.loginLimiter.allow("login:" + clientIP(r)) {
		writeProblemDetail(w, http.StatusTooManyRequests, "too many login attempts, slow down")
		return
	}
	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	// Checked BEFORE the account is resolved, and keyed on what was typed.
	//
	// Consulting it after the lookup made it an existence oracle: five failures
	// against a real address started answering 429 while an invented one kept
	// answering 401, which sorts real accounts from imaginary ones without ever
	// guessing a password.
	lockKey := loginKey(req.Email, clientIP(r))
	if s.lockout.locked(lockKey) {
		writeProblemDetail(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
		return
	}

	creds, err := s.credentialsByLogin(r.Context(), strings.TrimSpace(req.Email))
	if err != nil {
		// Still run a hash to blunt user-enumeration timing, and still count the
		// attempt — an unknown address has to reach the lock at the same rate a
		// real one does, or the counter itself becomes the oracle.
		_, _ = auth.VerifyPassword(req.Password, dummyHash)
		s.failLogin(r, lockKey, 0)
		writeProblemDetail(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	ok, _ := auth.VerifyPassword(req.Password, creds.PasswordHash)
	if !ok || !creds.IsActive {
		s.failLogin(r, lockKey, creds.ID)
		writeProblemDetail(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	if creds.TOTPEnabled {
		if !s.verifySecondFactor(r, &creds, req.TOTPCode, req.BackupCode) {
			s.failLogin(r, lockKey, creds.ID)
			writeProblemDetail(w, http.StatusUnauthorized, "invalid or missing two-factor code")
			return
		}
	}

	s.lockout.reset(lockKey)
	csrfToken, err := s.issueSession(w, r, creds.ID)
	if err != nil {
		writeProblemDetail(w, http.StatusInternalServerError, "create session")
		return
	}
	s.audit(r.Context(), &creds.ID, "user.login", "user", &creds.ID, clientIP(r), nil)
	writeJSON(w, http.StatusOK, map[string]any{"user": creds.User, "csrf_token": csrfToken})
}

// verifySecondFactor accepts either a valid TOTP code or an unused backup code
// (which is then consumed).
//
// creds.TOTPSecret arrives PLAINTEXT: credentialsByLogin opened it. The empty
// check in front of VerifyTOTP is not redundant — base32 decodes "" happily and
// the verification then runs against an empty HMAC key, whose codes anybody can
// compute, so an account that somehow carries totp_enabled with no secret must
// refuse rather than accept.
//
// The backup-code branch writes through setUserTOTP, which re-seals, so
// consuming a backup code also upgrades a legacy plaintext row.
func (s *server) verifySecondFactor(r *http.Request, creds *Credentials, totpCode, backupCode string) bool {
	if totpCode != "" && creds.TOTPSecret != "" && auth.VerifyTOTP(creds.TOTPSecret, totpCode, s.now()) {
		return true
	}
	if backupCode != "" {
		if remaining, ok := auth.ConsumeBackupCode(creds.BackupCodes, backupCode); ok {
			if err := s.setUserTOTP(r.Context(), creds.ID, creds.TOTPSecret, true, remaining); err == nil {
				return true
			}
		}
	}
	return false
}

// failLogin records a failed attempt against the lockout bucket. userID is 0
// when the login did not resolve to an account; the audit entry then has no
// actor, which is correct — nobody failed to log in, an address that does not
// exist was tried.
func (s *server) failLogin(r *http.Request, key string, userID int64) {
	if !s.lockout.fail(key) {
		return
	}
	var actor *int64
	var entity *int64
	if userID != 0 {
		actor, entity = &userID, &userID
	}
	s.audit(r.Context(), actor, "user.lockout", "user", entity, clientIP(r), nil)
}

// handleLogout destroys the current session and clears the cookies.
func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if p := s.resolvePrincipal(r); p != nil && p.kind == principalSession {
		_ = s.store.DeleteSession(r.Context(), p.sessionID)
		s.audit(r.Context(), &p.userID, "user.logout", "user", &p.userID, clientIP(r), nil)
	}
	s.clearCookie(w, sessionCookie, true)
	s.clearCookie(w, csrfCookie, false)
	w.WriteHeader(http.StatusNoContent)
}

// handleMe returns the authenticated user's profile.
func (s *server) handleMe(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	u, err := s.store.GetUserByID(r.Context(), p.userID)
	if storeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// handleListSessions lists the caller's active sessions.
func (s *server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	sessions, err := s.store.ListSessionsByUser(r.Context(), p.userID)
	if storeError(w, err) {
		return
	}
	// Mark which one is the current session, and replace the stored hash with
	// its public fingerprint. The hash is the credential the session cookie has
	// to reproduce, so it never leaves the server; the fingerprint is derived
	// from it one-way and is what the client addresses a session by.
	type view struct {
		Session
		Current bool `json:"current"`
	}
	out := make([]view, 0, len(sessions))
	for _, sess := range sessions {
		v := view{Session: sess, Current: sess.ID == p.sessionID}
		v.ID = auth.SessionFingerprint(sess.ID)
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRevokeSession deletes one of the caller's sessions, addressed by the
// public fingerprint from the session list, or all but the current one when the
// selector is the literal "others".
func (s *server) handleRevokeSession(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	selector := chi.URLParam(r, "id")
	if selector == "others" {
		if err := s.store.DeleteSessionsByUser(r.Context(), p.userID, p.sessionID); storeError(w, err) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Resolve the fingerprint against the caller's OWN sessions only. Scoping
	// the search this way is what makes the endpoint IDOR-safe: a fingerprint
	// belonging to another user simply never matches.
	sessions, err := s.store.ListSessionsByUser(r.Context(), p.userID)
	if storeError(w, err) {
		return
	}
	for _, sess := range sessions {
		if auth.SessionFingerprint(sess.ID) != selector {
			continue
		}
		if err := s.store.DeleteSession(r.Context(), sess.ID); storeError(w, err) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeProblemDetail(w, http.StatusNotFound, "unknown session")
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// handleChangePassword updates the caller's password after verifying the
// current one, and revokes all other sessions.
func (s *server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	var req changePasswordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	creds, err := s.credentialsByID(r.Context(), p.userID)
	if storeError(w, err) {
		return
	}
	if ok, _ := auth.VerifyPassword(req.CurrentPassword, creds.PasswordHash); !ok {
		writeProblemDetail(w, http.StatusForbidden, "current password is incorrect")
		return
	}
	if err := validatePassword(req.NewPassword); err != nil {
		writeProblemDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		writeProblemDetail(w, http.StatusInternalServerError, "hash password")
		return
	}
	// One call, one transaction: the new password, the other sessions and any
	// outstanding reset tokens move together.
	//
	// These used to be three statements with the last two best-effort. Someone
	// changing their password because they believe another person has it is
	// told the other sessions are gone; when the delete failed and its error was
	// dropped, they were told something untrue at precisely the moment it
	// mattered. A failure is now a failure, and nothing is half-applied.
	if err := s.store.ChangePassword(r.Context(), p.userID, hash, p.sessionID); storeError(w, err) {
		return
	}
	s.audit(r.Context(), &p.userID, "user.password_change", "user", &p.userID, clientIP(r), nil)
	w.WriteHeader(http.StatusNoContent)
}

type changeEmailRequest struct {
	CurrentPassword string `json:"current_password"`
	Email           string `json:"email"`
}

// noLocalPasswordDetail is the answer an account with no local password gets
// when it tries to re-authenticate with one. It names the way out rather than
// leaving the user at a dead end, the same way handleUnlinkIdentity's refusal
// does for the last-identity case.
const noLocalPasswordDetail = "this account signs in through single sign-on and has no password; " +
	"set one with the password-reset link on the sign-in page before changing your e-mail address"

// handleChangeEmail updates the caller's email address after re-verifying the
// current password.
//
// The re-check is the point of this handler. The e-mail address IS the account
// recovery channel: /auth/password-reset mails a single-use token to whatever
// address the row holds. Without proof of the password, a borrowed browser
// session (an unlocked laptop, a stolen cookie, an XSS that rides the session)
// could repoint that channel at the attacker and then reset the password
// through the front door — turning a temporary session into permanent
// ownership. handleChangePassword has always required the current password
// before rotating a credential; this endpoint rotates the credential that can
// reset the password, so it uses the same auth.VerifyPassword check and answers
// with the same 403.
//
// SSO-provisioned accounts have NO local password. PasswordHash == "" is the
// canonical predicate for that (it is what provisionOIDCUser writes and what
// auth.VerifyPassword rejects outright), the same one handleUnlinkIdentity
// tests. They cannot satisfy a password re-check at all, so they are refused
// with 409 and told how to get one — a password reset works for them, because
// their address was verified by the provider at provisioning time. Letting them
// through unchecked instead would leave the whole hole open for exactly the
// accounts that never chose a password.
func (s *server) handleChangeEmail(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	var req changeEmailRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	creds, err := s.credentialsByID(r.Context(), p.userID)
	if storeError(w, err) {
		return
	}
	if creds.PasswordHash == "" {
		writeProblemDetail(w, http.StatusConflict, noLocalPasswordDetail)
		return
	}
	// Verified BEFORE the address is even looked at, so a caller who cannot
	// prove the password learns nothing about the address rules either.
	if ok, _ := auth.VerifyPassword(req.CurrentPassword, creds.PasswordHash); !ok {
		writeProblemDetail(w, http.StatusForbidden, "current password is incorrect")
		return
	}
	if err := validateEmail(req.Email); err != nil {
		writeProblemDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	u, err := s.store.GetUserByID(r.Context(), p.userID)
	if storeError(w, err) {
		return
	}
	updated, err := s.store.UpdateUser(r.Context(), p.userID, strings.TrimSpace(req.Email), u.Username, u.DisplayName, string(u.Role), u.IsActive)
	if storeError(w, err) {
		return
	}
	// Outstanding reset tokens were mailed to the address that has just been
	// moved away from, and stayed redeemable for their full hour afterwards.
	// Someone who changes their address because the old mailbox is no longer
	// theirs is doing exactly the thing those tokens must not survive.
	if err := s.store.InvalidatePasswordResets(r.Context(), p.userID); err != nil {
		s.logger.Error("could not invalidate reset tokens after an address change",
			"user_id", p.userID, "err", err)
		writeProblemDetail(w, http.StatusInternalServerError,
			"the address was changed but old password-reset links could not be invalidated; "+
				"change your password to invalidate them")
		return
	}
	s.audit(r.Context(), &p.userID, "user.email_change", "user", &p.userID, clientIP(r), nil)
	writeJSON(w, http.StatusOK, updated)
}

// dummyHash is a valid argon2id hash, used to equalise timing on
// unknown-email logins (its exact value is irrelevant; it never matches).
var dummyHash, _ = auth.HashPassword("statwarden-timing-equaliser")
