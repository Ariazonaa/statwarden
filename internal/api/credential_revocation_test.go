package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/auth"
)

// requestReset + lastResetToken live in password_reset_test.go.

// TestChangingThePasswordKillsOutstandingResetTokens
//
// A reset token is a bearer credential for the whole account, valid for an
// hour. Redeeming one invalidated the others, but an ordinary password change
// did not — so the sequence that matters went: someone requests a reset for
// your address, you notice and change your password to lock them out, and the
// mail sitting in the intercepted inbox still works for the rest of the hour.
// The defensive action left the hole open.
func TestChangingThePasswordKillsOutstandingResetTokens(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	h.requestReset("admin@example.com")
	token := h.lastResetToken()

	rec := h.requestWith(http.MethodPut, "/api/v1/me/password", changePasswordRequest{
		CurrentPassword: "supersecret",
		NewPassword:     "a-deliberately-new-password",
	}, admin)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("change password = %d, body %s", rec.Code, rec.Body.String())
	}

	used := h.request(http.MethodPost, "/api/v1/auth/password-reset/confirm",
		passwordResetConfirmRequest{Token: token, NewPassword: "attacker-chosen"})
	if used.Code != http.StatusBadRequest {
		t.Fatalf("reset token after a password change = %d, want 400 — the token "+
			"outlived the change it should have been cancelled by", used.Code)
	}

	// And the password the owner set is the one that works.
	if _, code := h.login("admin@example.com", "a-deliberately-new-password", ""); code != http.StatusOK {
		t.Fatalf("login with the new password = %d, want 200", code)
	}
}

// The same for the address: tokens were mailed to the mailbox being moved away
// from, which is the mailbox someone changes their address because of.
func TestChangingTheEmailKillsOutstandingResetTokens(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	h.requestReset("admin@example.com")
	token := h.lastResetToken()

	rec := h.requestWith(http.MethodPut, "/api/v1/me/email", changeEmailRequest{
		CurrentPassword: "supersecret",
		Email:           "moved@example.com",
	}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("change email = %d, body %s", rec.Code, rec.Body.String())
	}

	used := h.request(http.MethodPost, "/api/v1/auth/password-reset/confirm",
		passwordResetConfirmRequest{Token: token, NewPassword: "attacker-chosen"})
	if used.Code != http.StatusBadRequest {
		t.Fatalf("reset token after an address change = %d, want 400 — a link "+
			"mailed to the OLD address still worked", used.Code)
	}
}

// Changing the password signs the other sessions out, and now says so
// truthfully: the write and the revocation are one transaction, so there is no
// outcome where the caller is told the sessions are gone and they are not.
func TestChangingThePasswordRevokesOtherSessions(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	// A second session for the same account — the one an attacker would hold.
	other, code := h.login("admin@example.com", "supersecret", "")
	if code != http.StatusOK {
		t.Fatalf("second login = %d", code)
	}
	if got := h.requestWith(http.MethodGet, "/api/v1/me", nil, other); got.Code != http.StatusOK {
		t.Fatalf("second session before the change = %d, want 200", got.Code)
	}

	rec := h.requestWith(http.MethodPut, "/api/v1/me/password", changePasswordRequest{
		CurrentPassword: "supersecret",
		NewPassword:     "a-deliberately-new-password",
	}, admin)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("change password = %d, body %s", rec.Code, rec.Body.String())
	}

	if got := h.requestWith(http.MethodGet, "/api/v1/me", nil, other); got.Code != http.StatusUnauthorized {
		t.Fatalf("the other session after the change = %d, want 401", got.Code)
	}
	// The caller's own session survives: signing yourself out of the screen you
	// are standing on is not what "revoke the others" means.
	if got := h.requestWith(http.MethodGet, "/api/v1/me", nil, admin); got.Code != http.StatusOK {
		t.Fatalf("the caller's own session = %d, want 200", got.Code)
	}
}

// TestActivityRefreshesTheSessionCookie
//
// Sliding expiry was only half implemented. The row's expires_at moved forward
// on every request and the browser was never told, so the cookie was deleted at
// the Max-Age set when the user logged in — signing an active user out mid-use
// while the server still considered the session perfectly valid. That is the
// exact outcome "sliding" exists to prevent.
func TestActivityRefreshesTheSessionCookie(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	// The first request after login is inside the once-a-minute throttle, so
	// nothing should be re-sent: a busy client must not get a Set-Cookie on
	// every response.
	rec := h.requestWith(http.MethodGet, "/api/v1/me", nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("me = %d", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatalf("cookies re-sent inside the throttle window: %v", rec.Result().Cookies())
	}

	// Past the throttle, the session slides — and the browser has to hear it.
	// Backdating last_seen is how the passage of time is expressed here: the
	// API harness has no injected clock, and the throttle is measured against
	// this field.
	backdateSession(t, h, admin, 2*time.Minute)
	rec = h.requestWith(http.MethodGet, "/api/v1/me", nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("me = %d", rec.Code)
	}

	got := map[string]*http.Cookie{}
	for _, c := range rec.Result().Cookies() {
		got[c.Name] = c
	}
	for _, name := range []string{sessionCookie, csrfCookie} {
		c, ok := got[name]
		if !ok {
			t.Fatalf("%s was not re-sent, so the browser still drops it at the "+
				"original Max-Age while the server keeps extending the session", name)
		}
		if c.MaxAge <= 0 {
			t.Fatalf("%s re-sent with Max-Age %d", name, c.MaxAge)
		}
	}
	// Nothing is minted here — the same tokens, a new lifetime. A rotated CSRF
	// value would silently break the SPA, which echoes the cookie it holds.
	if got[csrfCookie].Value != admin.csrf {
		t.Fatalf("the CSRF cookie value changed on refresh: %q -> %q",
			admin.csrf, got[csrfCookie].Value)
	}
}

// backdateSession moves a session's last-seen marker into the past, which is
// what the sliding-expiry throttle measures against.
func backdateSession(t *testing.T, h *harness, sess *session, by time.Duration) {
	t.Helper()
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	var raw string
	for _, c := range sess.cookies {
		if c.Name == sessionCookie {
			raw = c.Value
		}
	}
	if raw == "" {
		t.Fatal("the session carries no session cookie")
	}
	id := auth.HashToken(raw)
	stored, ok := h.store.sessions[id]
	if !ok {
		t.Fatalf("session %q is not in the store", id)
	}
	stored.LastSeenAt = stored.LastSeenAt.Add(-by)
	h.store.sessions[id] = stored
}
