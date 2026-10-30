package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// TestChangeEmailRequiresCurrentPassword is the session-hijack-to-ownership
// regression.
//
// PUT /me/email used to require nothing beyond the session: no password, no
// re-authentication, no confirmation to the old address. The e-mail address is
// the account recovery channel — /auth/password-reset mails a single-use token
// to whatever the row holds — so an attacker riding a borrowed session could
// point it at an inbox they own, request a reset, and walk in through the front
// door with a password of their choosing. A temporary session became permanent
// ownership, and the real owner was locked out of their own recovery channel.
//
// The endpoint now runs the same auth.VerifyPassword check handleChangePassword
// has always run, and answers with the same 403.
func TestChangeEmailRequiresCurrentPassword(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	for _, tc := range []struct {
		name string
		body changeEmailRequest
	}{
		{"no password at all", changeEmailRequest{Email: "attacker@evil.test"}},
		{"wrong password", changeEmailRequest{CurrentPassword: "hunter2", Email: "attacker@evil.test"}},
		{"the new address as the password", changeEmailRequest{
			CurrentPassword: "attacker@evil.test", Email: "attacker@evil.test"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.requestWith(http.MethodPut, "/api/v1/me/email", tc.body, admin)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("PUT /me/email = %d, want 403; body %s", rec.Code, rec.Body.String())
			}
		})
	}

	u, err := h.store.GetUserByID(context.Background(), admin.userID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if u.Email != "admin@example.com" {
		t.Fatalf("address = %q after refused changes, want the original", u.Email)
	}

	// The second half of the attack must therefore still be pointed at the real
	// owner: a reset mail goes to the original address, not the attacker's.
	rec := h.request(http.MethodPost, "/api/v1/auth/password-reset", passwordResetRequest{Email: "admin@example.com"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("password reset request = %d, want 202", rec.Code)
	}
	// The mail is sent after the response, so that a known address and an
	// unknown one cost the same; wait for that work before reading the mailbox.
	h.srv.resetInflight.Wait()
	if len(h.mails.sent) == 0 {
		t.Fatal("no reset mail was sent")
	}
	if to := h.mails.sent[len(h.mails.sent)-1].to; to != "admin@example.com" {
		t.Fatalf("reset mail went to %q, want the original address", to)
	}

	// With the correct password the change goes through, unchanged behaviour.
	rec = h.requestWith(http.MethodPut, "/api/v1/me/email",
		changeEmailRequest{CurrentPassword: "supersecret", Email: "new@example.com"}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /me/email with the right password = %d, body %s", rec.Code, rec.Body.String())
	}
	var got User
	mustJSON(t, rec, &got)
	if got.Email != "new@example.com" {
		t.Errorf("response address = %q, want the new one", got.Email)
	}
	u, err = h.store.GetUserByID(context.Background(), admin.userID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if u.Email != "new@example.com" {
		t.Errorf("stored address = %q, want the new one", u.Email)
	}
}

// TestChangeEmailRefusedWithoutLocalPassword pins how an SSO-provisioned account
// is handled. provisionOIDCUser writes an EMPTY password hash, which
// auth.VerifyPassword rejects for every input — including the empty string, so
// there is no accidental "" == "" bypass. Rather than 403-ing such a user
// forever with "current password is incorrect", the endpoint refuses with 409
// and names the way out, mirroring handleUnlinkIdentity's refusal for the last
// linked identity.
func TestChangeEmailRefusedWithoutLocalPassword(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	user := h.createUser(admin, "sso@example.com", "viewer")

	// Reduce the account to the state provisioning leaves behind: a session, an
	// address, and no local password.
	if err := h.store.UpdateUserPassword(context.Background(), user.userID, ""); err != nil {
		t.Fatalf("clear password hash: %v", err)
	}

	for _, pw := range []string{"", "supersecret", "$argon2id$"} {
		rec := h.requestWith(http.MethodPut, "/api/v1/me/email",
			changeEmailRequest{CurrentPassword: pw, Email: "attacker@evil.test"}, user)
		if rec.Code != http.StatusConflict {
			t.Fatalf("PUT /me/email with password %q on a passwordless account = %d, want 409; body %s",
				pw, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "password-reset link") {
			t.Errorf("the refusal does not tell the user how to proceed: %s", rec.Body.String())
		}
	}

	u, err := h.store.GetUserByID(context.Background(), user.userID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if u.Email != "sso@example.com" {
		t.Fatalf("address = %q, want the SSO account's address untouched", u.Email)
	}
}
