package api

import (
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/auth"
)

// enableTOTPDirect switches 2FA on for a user straight in the store and returns
// the secret and the plaintext backup codes, for tests that are about what
// happens AFTER enrolment.
func enableTOTPDirect(t *testing.T, h *harness, userID int64) (string, []string) {
	t.Helper()
	secret, err := auth.NewTOTPSecret()
	if err != nil {
		t.Fatalf("NewTOTPSecret: %v", err)
	}
	plain, hashed, err := auth.GenerateBackupCodes()
	if err != nil {
		t.Fatalf("GenerateBackupCodes: %v", err)
	}
	if err := h.store.SetUserTOTP(t.Context(), userID, secret, true, hashed); err != nil {
		t.Fatalf("enable 2FA: %v", err)
	}
	return secret, plain
}

// The reset mail goes to the address the account HOLDS, not to the spelling the
// caller typed. The lookup is case-insensitive, so the typed string can match a
// stored address without being it — and anything that compares the two more
// loosely than byte-for-byte (Unicode case folding in the database's lower(),
// a mail server that treats them differently) turns "mail the typed address"
// into "mail an address that merely resembles the account's".
func TestPasswordResetMailsTheStoredAddress(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin()

	h.requestReset("ADMIN@Example.COM")

	if got := h.mails.count(); got != 1 {
		t.Fatalf("sent %d mails, want 1", got)
	}
	h.mails.mu.Lock()
	to := h.mails.sent[0].to
	h.mails.mu.Unlock()
	if to != "admin@example.com" {
		t.Fatalf("reset mail went to %q, want the stored address admin@example.com", to)
	}
}

// /me/totp/verify is the second half of ENROLMENT. Once 2FA is on, a valid code
// there used to mint a fresh set of backup codes and silently void the owner's
// printed ones — with a session and one authenticator code, no password. That is
// what /me/totp/backup-codes exists for, behind the full step-up.
func TestTOTPVerifyRefusesOnceTwoFactorIsEnabled(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	secret, _ := enableTOTPDirect(t, h, admin.userID)

	before, err := h.store.GetCredentialsByID(t.Context(), admin.userID)
	if err != nil {
		t.Fatalf("GetCredentialsByID: %v", err)
	}

	code, err := auth.TOTPCode(secret, time.Now())
	if err != nil {
		t.Fatalf("TOTPCode: %v", err)
	}
	rec := h.requestWith(http.MethodPost, "/api/v1/me/totp/verify", totpVerifyRequest{Code: code}, admin)
	if rec.Code != http.StatusConflict {
		t.Fatalf("verify with 2FA already enabled = %d, want 409; body %s", rec.Code, rec.Body.String())
	}

	after, err := h.store.GetCredentialsByID(t.Context(), admin.userID)
	if err != nil {
		t.Fatalf("GetCredentialsByID: %v", err)
	}
	if !slices.Equal(before.BackupCodes, after.BackupCodes) {
		t.Fatal("the backup codes were replaced by a verify call on an already-enabled account")
	}
}

// One IPv6 subscriber is handed a whole /64 — eighteen quintillion addresses
// that each looked like a separate client to the per-IP login limiter and the
// (login, IP) lockout. Walking the low 64 bits gave an attacker a fresh budget on
// every request, so neither brake ever engaged.
func TestLoginBrakesTreatAnIPv6Slash64AsOneClient(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin()

	got429 := false
	for i := 1; i <= 25; i++ {
		peer := fmt.Sprintf("[2001:db8:1:2::%x]:40000", i)
		rec := h.requestFrom(http.MethodPost, "/api/v1/auth/login",
			loginRequest{Email: "admin@example.com", Password: "wrong-password"}, peer)
		if rec.Code == http.StatusTooManyRequests {
			got429 = true
			break
		}
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401 or 429", i, rec.Code)
		}
	}
	if !got429 {
		t.Fatal("25 failed logins from one /64 never hit a 429: every address got its own budget")
	}

	// A different /64 is a different client and still gets in.
	rec := h.requestFrom(http.MethodPost, "/api/v1/auth/login",
		loginRequest{Email: "admin@example.com", Password: "supersecret"}, "[2001:db8:1:3::1]:40000")
	if rec.Code != http.StatusOK {
		t.Fatalf("login from a neighbouring /64 = %d, want 200", rec.Code)
	}
}

func TestRateLimitAddr(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"203.0.113.9", "203.0.113.9"},
		{"2001:db8:1:2::1", "2001:db8:1:2::/64"},
		{"2001:db8:1:2:ffff:ffff:ffff:ffff", "2001:db8:1:2::/64"},
		{"::ffff:203.0.113.9", "203.0.113.9"},
		// Not an address at all (an unparsable RemoteAddr): passed through, so
		// it still gets a bucket of its own rather than sharing one with
		// everything else that failed to parse.
		{"pipe", "pipe"},
	} {
		if got := rateLimitAddr(tc.in); got != tc.want {
			t.Errorf("rateLimitAddr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// password-reset/confirm is unauthenticated, and it used to run the argon2 hash
// of the new password BEFORE looking at the token — so any caller with a made-up
// token got the server to spend a full KDF derivation, the most expensive thing
// it does, per request. The token is checked first now; only a redeemable one
// earns the hash.
func TestResetConfirmChecksTheTokenBeforeHashing(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin()

	calls := 0
	orig := hashNewPassword
	hashNewPassword = func(pw string) (string, error) {
		calls++
		return orig(pw)
	}
	t.Cleanup(func() { hashNewPassword = orig })

	rec := h.request(http.MethodPost, "/api/v1/auth/password-reset/confirm",
		passwordResetConfirmRequest{Token: "made-up-token", NewPassword: "a-long-new-password"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("confirm with an unknown token = %d, want 400", rec.Code)
	}
	if calls != 0 {
		t.Fatalf("the new password was hashed %d time(s) for a token that does not exist", calls)
	}

	// A real token still works, and is hashed exactly once.
	h.requestReset("admin@example.com")
	rec = h.request(http.MethodPost, "/api/v1/auth/password-reset/confirm",
		passwordResetConfirmRequest{Token: h.lastResetToken(), NewPassword: "a-long-new-password"})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("confirm with a real token = %d, body %s", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Fatalf("hashed %d times for one redemption, want 1", calls)
	}
}
