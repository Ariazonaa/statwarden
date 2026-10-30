package api

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Ariazonaa/statwarden/internal/auth"
)

// totpIssuer names the account in the authenticator app.
const totpIssuer = "Statwarden"

// handleTOTPEnroll generates (but does not yet activate) a TOTP secret and
// returns the otpauth URI for the enrolment QR code. Activation happens in
// handleTOTPVerify once the user proves they scanned it.
func (s *server) handleTOTPEnroll(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	var req stepUp
	if !decodeJSON(w, r, &req) {
		return
	}
	// Enrolling writes a new secret to the account, so a borrowed session could
	// attach an authenticator the owner knows nothing about — and, since
	// disabling 2FA needs the password the attacker does not have, lock them
	// out of their own account with it.
	if !s.reauthenticate(w, r, req) {
		return
	}
	u, err := s.store.GetUserByID(r.Context(), p.userID)
	if storeError(w, err) {
		return
	}
	if u.TOTPEnabled {
		writeProblemDetail(w, http.StatusConflict, "two-factor authentication is already enabled")
		return
	}
	secret, err := auth.NewTOTPSecret()
	if err != nil {
		writeProblemDetail(w, http.StatusInternalServerError, "generate secret")
		return
	}
	// Store the pending secret with enabled=false; verify flips it on. The write
	// goes through setUserTOTP, which seals it at rest and FAILS the request if
	// it cannot — a 500 here is strictly better than an enrolment whose secret
	// sits in the database in the clear.
	if err := s.setUserTOTP(r.Context(), p.userID, secret, false, nil); storeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"secret":      secret,
		"otpauth_uri": auth.TOTPURI(secret, totpIssuer, u.Email),
	})
}

type totpVerifyRequest struct {
	Code string `json:"code"`
}

// handleTOTPVerify confirms enrolment: on a valid code it enables 2FA and
// returns fresh single-use backup codes (shown once).
func (s *server) handleTOTPVerify(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	var req totpVerifyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// credentialsByID hands back the OPENED secret, so the verification below
	// works against the base32 value whether the row is sealed or legacy
	// plaintext.
	creds, err := s.credentialsByID(r.Context(), p.userID)
	if storeError(w, err) {
		return
	}
	if creds.TOTPSecret == "" {
		writeProblemDetail(w, http.StatusBadRequest, "start enrolment first")
		return
	}
	if !auth.VerifyTOTP(creds.TOTPSecret, req.Code, s.now()) {
		writeProblemDetail(w, http.StatusBadRequest, "code did not verify")
		return
	}
	plain, hashed, err := auth.GenerateBackupCodes()
	if err != nil {
		writeProblemDetail(w, http.StatusInternalServerError, "generate backup codes")
		return
	}
	if err := s.setUserTOTP(r.Context(), p.userID, creds.TOTPSecret, true, hashed); storeError(w, err) {
		return
	}
	s.audit(r.Context(), &p.userID, "user.totp_enable", "user", &p.userID, clientIP(r), nil)
	writeJSON(w, http.StatusOK, map[string]any{"backup_codes": plain})
}

// handleTOTPDisable turns off 2FA after re-authenticating.
//
// It takes the same proof as enrolling, which means the code as well as the
// password. The two were inconsistent the other way round for a while — adding
// a factor demanded both, REMOVING one demanded only the password — and that is
// backwards: taking the second factor away is the more dangerous of the two,
// and it is what an attacker holding a stolen password would reach for first.
//
// Losing the authenticator is not a dead end: consumeSecondFactor accepts a
// backup code in place of a generated one, which is what backup codes are for.
func (s *server) handleTOTPDisable(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	var req stepUp
	if !decodeJSON(w, r, &req) {
		return
	}
	if !s.reauthenticate(w, r, req) {
		return
	}
	if err := s.setUserTOTP(r.Context(), p.userID, "", false, nil); storeError(w, err) {
		return
	}
	s.audit(r.Context(), &p.userID, "user.totp_disable", "user", &p.userID, clientIP(r), nil)
	w.WriteHeader(http.StatusNoContent)
}

// handleRegenerateBackupCodes issues a fresh set of backup codes, invalidating
// the old ones. 2FA must be enabled.
func (s *server) handleRegenerateBackupCodes(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	var req stepUp
	if !decodeJSON(w, r, &req) {
		return
	}
	// Regenerating invalidates the owner's printed codes and hands the caller a
	// fresh set — a way to take the second factor away from someone while
	// keeping it for yourself.
	if !s.reauthenticate(w, r, req) {
		return
	}
	creds, err := s.credentialsByID(r.Context(), p.userID)
	if storeError(w, err) {
		return
	}
	if !creds.TOTPEnabled {
		writeProblemDetail(w, http.StatusBadRequest, "two-factor authentication is not enabled")
		return
	}
	plain, hashed, err := auth.GenerateBackupCodes()
	if err != nil {
		writeProblemDetail(w, http.StatusInternalServerError, "generate backup codes")
		return
	}
	// The secret round-trips opened → re-sealed, so regenerating backup codes
	// also upgrades a legacy plaintext row.
	if err := s.setUserTOTP(r.Context(), p.userID, creds.TOTPSecret, true, hashed); storeError(w, err) {
		return
	}
	s.audit(r.Context(), &p.userID, "user.backup_codes_regenerate", "user", &p.userID, clientIP(r), nil)
	writeJSON(w, http.StatusOK, map[string]any{"backup_codes": plain})
}

// --- API keys ---

// apiKeyMaxExpiryDays bounds expires_in_days. It is not a retention policy —
// an absent value still means "never", which is what every key minted before
// this existed got — but a typo guard: a client that sends milliseconds or a
// year number instead of a day count should be told so, not handed a key that
// expires in the year 5000 and reads as permanent to whoever audits it later.
const apiKeyMaxExpiryDays = 3650

type createAPIKeyRequest struct {
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
	// ExpiresInDays sets the key's lifetime, in whole days from now. Absent or
	// null means the key never expires.
	//
	// Days rather than an absolute timestamp, matching session_ttl_days: the
	// caller does not have to agree with the server about what time it is, and
	// there is no timezone to get wrong. A POINTER rather than a plain int,
	// because 0 and "not sent" have to stay distinguishable — a plain int would
	// make every existing client, which sends no such field, look like it asked
	// for a key that expires immediately.
	//
	// The default stays "never" deliberately. Making expiry mandatory is a
	// separate decision with a migration attached (every key in the database
	// predates the field), and shipping it silently here would revoke working
	// automation on a timer nobody set.
	ExpiresInDays *int `json:"expires_in_days"`
	// Embedded, not nested, so the wire shape stays {"name":…,"password":…}.
	stepUp
}

// apiKeyExpiry converts the request's day count into the absolute instant the
// store persists, or nil for a key that never expires.
func (s *server) apiKeyExpiry(req createAPIKeyRequest) (*time.Time, error) {
	if req.ExpiresInDays == nil {
		return nil, nil
	}
	d := *req.ExpiresInDays
	if d < 1 || d > apiKeyMaxExpiryDays {
		return nil, fmt.Errorf("expires_in_days must be between 1 and %d, or omitted for a key that never expires", apiKeyMaxExpiryDays)
	}
	// AddDate, not a multiplication by 24h: it keeps the wall-clock time of day
	// across a DST boundary, which is what "90 days from now" means to the
	// person who typed it.
	t := s.now().AddDate(0, 0, d)
	return &t, nil
}

// handleCreateAPIKey mints a scoped API key. The secret is returned once, in
// this response only; the store keeps just its hash.
func (s *server) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	var req createAPIKeyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// An API key outlives every session and is not revoked by a password
	// change, so minting one from a borrowed session is how a temporary
	// foothold becomes permanent.
	if !s.reauthenticate(w, r, req.stepUp) {
		return
	}
	if req.Name == "" {
		writeProblemDetail(w, http.StatusBadRequest, "name is required")
		return
	}
	scopes, err := validateScopes(req.Scopes)
	if err != nil {
		writeProblemDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	// Cap the requested scopes to the caller's own rights: a key may never mint a
	// key more privileged than itself. This closes the escalation where a
	// read-only key mints a write/admin key through this very endpoint.
	for _, sc := range scopes {
		if !p.grantsScope(sc) {
			writeProblemDetail(w, http.StatusForbidden, "cannot grant a scope beyond your own rights")
			return
		}
	}
	expiresAt, err := s.apiKeyExpiry(req)
	if err != nil {
		writeProblemDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	secret, prefix, err := auth.NewAPIKey()
	if err != nil {
		writeProblemDetail(w, http.StatusInternalServerError, "generate key")
		return
	}
	key, err := s.store.CreateAPIKey(r.Context(), APIKey{
		UserID:    p.userID,
		Name:      req.Name,
		Prefix:    prefix,
		KeyHash:   auth.HashToken(secret),
		Scopes:    scopes,
		ExpiresAt: expiresAt,
	})
	if storeError(w, err) {
		return
	}
	// The expiry goes in the audit detail: "who minted a key that never
	// expires" is the question this log exists to answer, and it cannot be
	// reconstructed later from a row that has since been revoked.
	detail := map[string]any{"name": key.Name, "scopes": scopes}
	if key.ExpiresAt != nil {
		detail["expires_at"] = key.ExpiresAt.UTC().Format(time.RFC3339)
	} else {
		detail["expires_at"] = nil
	}
	s.audit(r.Context(), &p.userID, "apikey.create", "api_key", &key.ID, clientIP(r), detail)
	writeJSON(w, http.StatusCreated, map[string]any{"api_key": key, "secret": secret})
}

// handleListAPIKeys lists the caller's keys (never their secrets).
func (s *server) handleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	keys, err := s.store.ListAPIKeys(r.Context(), p.userID)
	if storeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, keys)
}

// handleRevokeAPIKey deletes one of the caller's keys.
func (s *server) handleRevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.store.DeleteAPIKey(r.Context(), id, p.userID); storeError(w, err) {
		return
	}
	s.audit(r.Context(), &p.userID, "apikey.revoke", "api_key", &id, clientIP(r), nil)
	w.WriteHeader(http.StatusNoContent)
}

func validateScopes(scopes []string) ([]string, error) {
	if len(scopes) == 0 {
		return []string{ScopeRead}, nil
	}
	out := make([]string, 0, len(scopes))
	for _, sc := range scopes {
		switch sc {
		case ScopeRead, ScopeWrite, ScopeAdmin:
			out = append(out, sc)
		default:
			return nil, errors.New("unknown scope; allowed: read, write, admin")
		}
	}
	return out, nil
}
