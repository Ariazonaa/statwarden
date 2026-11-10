package api

import (
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
