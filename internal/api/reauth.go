package api

import (
	"net/http"

	"github.com/Ariazonaa/statwarden/internal/auth"
)

// stepUp is the re-authentication block embedded in the requests that change
// how an account can be logged into. It is a struct rather than two loose
// fields so the JSON names stay identical everywhere.
type stepUp struct {
	// Password is the caller's current password. Ignored for an account that
	// has none (single sign-on only) — see reauthenticate.
	Password string `json:"password"`
	// TOTPCode is a current authenticator code, or a backup code, for an
	// account with 2FA enabled.
	TOTPCode string `json:"totp_code"`
}

// reauthenticate proves the caller is still the account owner rather than
// merely the holder of a session cookie, and writes the failure response
// itself. A false return means the handler must stop.
//
// A session plus CSRF token used to be enough to mint an admin API key, enrol a
// new authenticator or attach a new single sign-on identity. Each of those
// converts a borrowed session — an unlocked laptop, an XSS, a stolen cookie —
// into standing access that outlives the session and survives the owner
// changing their password. Re-proving a factor at that moment is what keeps
// "someone used my computer" from becoming "someone owns my account".
//
// Both available factors are demanded, not one: an attacker at an unlocked
// machine may well be able to read a code from the authenticator sitting next
// to it, and password-only leaves 2FA doing nothing at the moment it matters
// most.
//
// LIMITATION, deliberate and audited: an account provisioned through single
// sign-on has no password and, unless it enrolled one, no second factor. There
// is nothing local to re-prove, so the step-up is skipped and recorded. Closing
// that gap means bouncing the caller back through the identity provider, which
// is a different piece of work; skipping it silently would be worse than
// saying so.
func (s *server) reauthenticate(w http.ResponseWriter, r *http.Request, su stepUp) bool {
	p := principalFrom(r)
	creds, err := s.credentialsByID(r.Context(), p.userID)
	if storeError(w, err) {
		return false
	}

	if creds.PasswordHash == "" && !creds.TOTPEnabled {
		s.audit(r.Context(), &p.userID, "user.reauth_skipped_no_local_factor",
			"user", &p.userID, clientIP(r), nil)
		return true
	}

	if creds.PasswordHash != "" {
		if ok, _ := auth.VerifyPassword(su.Password, creds.PasswordHash); !ok {
			s.audit(r.Context(), &p.userID, "user.reauth_failed", "user", &p.userID, clientIP(r),
				map[string]any{"factor": "password"})
			writeProblemDetail(w, http.StatusForbidden, "password is incorrect")
			return false
		}
	}

	if creds.TOTPEnabled {
		if !s.consumeSecondFactor(r, creds, su.TOTPCode) {
			s.audit(r.Context(), &p.userID, "user.reauth_failed", "user", &p.userID, clientIP(r),
				map[string]any{"factor": "totp"})
			writeProblemDetail(w, http.StatusForbidden, "two-factor code did not verify")
			return false
		}
	}
	return true
}

// consumeSecondFactor accepts a current authenticator code or an unused backup
// code, burning the backup code when it is the one that matched.
func (s *server) consumeSecondFactor(r *http.Request, creds Credentials, code string) bool {
	if code == "" {
		return false
	}
	if auth.VerifyTOTP(creds.TOTPSecret, code, s.now()) {
		return true
	}
	remaining, ok := auth.ConsumeBackupCode(creds.BackupCodes, code)
	if !ok {
		return false
	}
	// Single-use: a backup code that survived being accepted is not a backup
	// code. A failure to persist the burn is treated as a failed factor rather
	// than swallowed, so the code cannot be replayed.
	p := principalFrom(r)
	return s.setUserTOTP(r.Context(), p.userID, creds.TOTPSecret, true, remaining) == nil
}
