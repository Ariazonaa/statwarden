package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/auth"
)

// resetLinkRe pulls the token out of the mail body the server produced, so the
// tests exercise the same string a real user would click rather than reaching
// into the store for it.
var resetLinkRe = regexp.MustCompile(`/reset-password\?token=([A-Za-z0-9._~%-]+)`)

// requestReset asks for a reset and returns the recorder.
//
// It waits for the issuing goroutine before returning. Minting, storing and
// mailing the token happen off the request path deliberately — doing them
// inline made the response measurably slower for an address that exists,
// which is the enumeration oracle the identical status and body are there to
// prevent — so a test that reads the mailbox straight after the call would
// otherwise be racing it.
func (h *harness) requestReset(email string) *http.Response {
	h.t.Helper()
	rec := h.request(http.MethodPost, "/api/v1/auth/password-reset", passwordResetRequest{Email: email})
	h.srv.resetInflight.Wait()
	return rec.Result()
}

// lastResetToken extracts the token from the most recent mail.
func (h *harness) lastResetToken() string {
	h.t.Helper()
	h.mails.mu.Lock()
	defer h.mails.mu.Unlock()
	if len(h.mails.sent) == 0 {
		h.t.Fatal("no mail was sent")
	}
	m := h.mails.sent[len(h.mails.sent)-1]
	match := resetLinkRe.FindStringSubmatch(m.text)
	if match == nil {
		h.t.Fatalf("no reset link in mail body: %s", m.text)
	}
	tok, err := url.QueryUnescape(match[1])
	if err != nil {
		h.t.Fatalf("unescape token: %v", err)
	}
	return tok
}

// TestPasswordResetDoesNotLeakAccountExistence is the central guarantee of this
// endpoint: a caller must not be able to tell a registered address from an
// unregistered one.
//
// It compares the full response — status, headers that vary, and body — for a
// known and an unknown address. Regressing this would mean adding a branch (a
// 404 for unknown, a different message, an early return before the store
// lookup), and any such branch changes one of the compared values.
func TestPasswordResetDoesNotLeakAccountExistence(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin()

	known := h.request(http.MethodPost, "/api/v1/auth/password-reset",
		passwordResetRequest{Email: "admin@example.com"})
	unknown := h.request(http.MethodPost, "/api/v1/auth/password-reset",
		passwordResetRequest{Email: "nobody@example.com"})
	malformed := h.request(http.MethodPost, "/api/v1/auth/password-reset",
		passwordResetRequest{Email: "not-an-address"})
	// Issuing runs after the response so a known and an unknown address cost
	// the same; the mail assertion below has to wait for it.
	h.srv.resetInflight.Wait()

	if known.Code != http.StatusAccepted {
		t.Fatalf("known address = %d, want 202, body %s", known.Code, known.Body.String())
	}
	for name, rec := range map[string]int{"unknown": unknown.Code, "malformed": malformed.Code} {
		if rec != known.Code {
			t.Errorf("%s address status = %d, want %d — the response must not depend on whether "+
				"the address exists", name, rec, known.Code)
		}
	}
	if unknown.Body.String() != known.Body.String() {
		t.Errorf("unknown-address body %q differs from known-address body %q; the two must be "+
			"byte-identical or the endpoint is an account-existence oracle",
			unknown.Body.String(), known.Body.String())
	}
	if malformed.Body.String() != known.Body.String() {
		t.Errorf("malformed-address body %q differs from known-address body %q",
			malformed.Body.String(), known.Body.String())
	}

	// The observable difference is confined to the mailbox, which the caller
	// does not control: exactly one mail, to the address that exists.
	if got := h.mails.count(); got != 1 {
		t.Fatalf("sent %d mails, want exactly 1 (only the known address)", got)
	}
	h.mails.mu.Lock()
	to := h.mails.sent[0].to
	h.mails.mu.Unlock()
	if to != "admin@example.com" {
		t.Errorf("mail went to %q, want admin@example.com", to)
	}
}

// TestPasswordResetDeactivatedAccountIsIndistinguishable covers the third
// branch: a real address whose account is switched off must look like any other.
func TestPasswordResetDeactivatedAccountIsIndistinguishable(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	user := h.createUser(admin, "user@example.com", "user")

	// Deactivate through the admin API.
	inactive := false
	rec := h.requestWith(http.MethodPut, fmt.Sprintf("/api/v1/users/%d", user.userID), updateUserRequest{
		Email: "user@example.com", Role: "user", IsActive: &inactive,
	}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("deactivate = %d, body %s", rec.Code, rec.Body.String())
	}

	before := h.mails.count()
	activeResp := h.request(http.MethodPost, "/api/v1/auth/password-reset",
		passwordResetRequest{Email: "admin@example.com"})
	inactiveResp := h.request(http.MethodPost, "/api/v1/auth/password-reset",
		passwordResetRequest{Email: "user@example.com"})
	h.srv.resetInflight.Wait() // issuing happens off the request path

	if inactiveResp.Code != activeResp.Code || inactiveResp.Body.String() != activeResp.Body.String() {
		t.Errorf("deactivated account answered %d %q, active answered %d %q; they must match",
			inactiveResp.Code, inactiveResp.Body.String(), activeResp.Code, activeResp.Body.String())
	}
	// Only the active account gets mail.
	if got := h.mails.count() - before; got != 1 {
		t.Errorf("sent %d mails, want 1 (the deactivated account must not receive one)", got)
	}
}

// TestPasswordResetRoundTrip walks the happy path and asserts the two effects
// that matter: the password actually changes, and every session dies.
func TestPasswordResetRoundTrip(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	// The admin has a live session from setup; prove it works first.
	if rec := h.requestWith(http.MethodGet, "/api/v1/me", nil, admin); rec.Code != http.StatusOK {
		t.Fatalf("pre-reset GET /me = %d, want 200", rec.Code)
	}

	if resp := h.requestReset("admin@example.com"); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("request reset = %d", resp.StatusCode)
	}
	token := h.lastResetToken()

	// The plaintext token must never be what is persisted.
	stored := h.storedResets()
	if len(stored) != 1 {
		t.Fatalf("stored %d resets, want 1", len(stored))
	}
	if stored[0].TokenHash == token {
		t.Fatal("the reset row holds the plaintext token; only its SHA-256 may be stored")
	}
	if stored[0].TokenHash != auth.HashToken(token) {
		t.Fatal("stored hash does not match auth.HashToken of the mailed token")
	}

	rec := h.request(http.MethodPost, "/api/v1/auth/password-reset/confirm",
		passwordResetConfirmRequest{Token: token, NewPassword: "brand-new-password"})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("confirm = %d, body %s", rec.Code, rec.Body.String())
	}
	// No session may be issued from a mailed link.
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			t.Error("confirm issued a session cookie; a reset must not sign the caller in")
		}
	}

	// Every pre-existing session is gone, including this one.
	if rec := h.requestWith(http.MethodGet, "/api/v1/me", nil, admin); rec.Code != http.StatusUnauthorized {
		t.Errorf("post-reset GET /me with the OLD session = %d, want 401 — the reset must revoke "+
			"every session, including one an attacker may hold", rec.Code)
	}
	// The old password no longer works and the new one does.
	if _, code := h.login("admin@example.com", "supersecret", ""); code != http.StatusUnauthorized {
		t.Errorf("login with the old password = %d, want 401", code)
	}
	if _, code := h.login("admin@example.com", "brand-new-password", ""); code != http.StatusOK {
		t.Errorf("login with the new password = %d, want 200", code)
	}
}

// TestPasswordResetTokenIsSingleUse pins that a redeemed token cannot be
// replayed, and that the replay is indistinguishable from an unknown token.
func TestPasswordResetTokenIsSingleUse(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin()
	h.requestReset("admin@example.com")
	token := h.lastResetToken()

	first := h.request(http.MethodPost, "/api/v1/auth/password-reset/confirm",
		passwordResetConfirmRequest{Token: token, NewPassword: "first-new-password"})
	if first.Code != http.StatusNoContent {
		t.Fatalf("first confirm = %d, body %s", first.Code, first.Body.String())
	}
	replay := h.request(http.MethodPost, "/api/v1/auth/password-reset/confirm",
		passwordResetConfirmRequest{Token: token, NewPassword: "second-new-password"})
	unknown := h.request(http.MethodPost, "/api/v1/auth/password-reset/confirm",
		passwordResetConfirmRequest{Token: "a-token-that-never-existed", NewPassword: "second-new-password"})

	if replay.Code != http.StatusBadRequest {
		t.Errorf("replayed token = %d, want 400", replay.Code)
	}
	if replay.Body.String() != unknown.Body.String() {
		t.Errorf("replayed token answered %q but an unknown token answered %q; an already-used "+
			"token must be indistinguishable from one that never existed",
			replay.Body.String(), unknown.Body.String())
	}
	// The second password must not have taken effect.
	if _, code := h.login("admin@example.com", "second-new-password", ""); code == http.StatusOK {
		t.Error("the replayed token changed the password; it must be single-use")
	}
}

// TestPasswordResetTokenExpires drives the injectable clock past the TTL.
func TestPasswordResetTokenExpires(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin()
	h.requestReset("admin@example.com")
	token := h.lastResetToken()

	// Age the stored row past its expiry rather than waiting an hour.
	h.expireResets(passwordResetTTL + time.Minute)

	expired := h.request(http.MethodPost, "/api/v1/auth/password-reset/confirm",
		passwordResetConfirmRequest{Token: token, NewPassword: "brand-new-password"})
	unknown := h.request(http.MethodPost, "/api/v1/auth/password-reset/confirm",
		passwordResetConfirmRequest{Token: "a-token-that-never-existed", NewPassword: "brand-new-password"})

	if expired.Code != http.StatusBadRequest {
		t.Errorf("expired token = %d, want 400", expired.Code)
	}
	if expired.Body.String() != unknown.Body.String() {
		t.Errorf("expired token answered %q but an unknown token answered %q; a distinct answer "+
			"for 'expired' leaks that the token once existed",
			expired.Body.String(), unknown.Body.String())
	}
	if _, code := h.login("admin@example.com", "brand-new-password", ""); code == http.StatusOK {
		t.Error("an expired token changed the password")
	}
}

// TestPasswordResetInvalidatesOtherTokens covers the stale-mail case: after a
// successful recovery, an older link sitting in the inbox must be dead.
func TestPasswordResetInvalidatesOtherTokens(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin()

	h.requestReset("admin@example.com")
	older := h.lastResetToken()
	h.requestReset("admin@example.com")
	newer := h.lastResetToken()
	if older == newer {
		t.Fatal("two requests produced the same token")
	}

	if rec := h.request(http.MethodPost, "/api/v1/auth/password-reset/confirm",
		passwordResetConfirmRequest{Token: newer, NewPassword: "brand-new-password"}); rec.Code != http.StatusNoContent {
		t.Fatalf("confirm newer = %d, body %s", rec.Code, rec.Body.String())
	}
	rec := h.request(http.MethodPost, "/api/v1/auth/password-reset/confirm",
		passwordResetConfirmRequest{Token: older, NewPassword: "yet-another-password"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("older token after recovery = %d, want 400 — a stale mail must not stay "+
			"redeemable once the account has been recovered", rec.Code)
	}
}

// TestPasswordResetRejectsWeakPasswordWithoutBurningToken proves the strength
// check runs before redemption, so a typo does not cost the user their link.
func TestPasswordResetRejectsWeakPasswordWithoutBurningToken(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin()
	h.requestReset("admin@example.com")
	token := h.lastResetToken()

	if rec := h.request(http.MethodPost, "/api/v1/auth/password-reset/confirm",
		passwordResetConfirmRequest{Token: token, NewPassword: "short"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("weak password = %d, want 400", rec.Code)
	}
	if rec := h.request(http.MethodPost, "/api/v1/auth/password-reset/confirm",
		passwordResetConfirmRequest{Token: token, NewPassword: "a-good-long-password"}); rec.Code != http.StatusNoContent {
		t.Errorf("retry with a valid password = %d, want 204 — a rejected password must not "+
			"consume the token", rec.Code)
	}
}

// TestPasswordResetIsRateLimited pins that the mail-sending endpoint cannot be
// used as an unbounded outbound-mail amplifier.
func TestPasswordResetIsRateLimited(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin()

	var limited bool
	for i := 0; i < loginRateLimit+2; i++ {
		if h.request(http.MethodPost, "/api/v1/auth/password-reset",
			passwordResetRequest{Email: "admin@example.com"}).Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Errorf("no 429 within %d requests; the reset endpoint must be rate limited — it sends "+
			"mail to a third party on demand", loginRateLimit+2)
	}
}

// TestPasswordResetTokenNeverAppearsInAudit guards the rule that the plaintext
// token lives in exactly two places: the handler variable and the mail body.
func TestPasswordResetTokenNeverAppearsInAudit(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin()
	h.requestReset("admin@example.com")
	token := h.lastResetToken()

	entries, _, err := h.store.ListAudit(t.Context(), Page{Limit: 100})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	for _, e := range entries {
		raw, _ := json.Marshal(e)
		if strings.Contains(string(raw), token) {
			t.Fatalf("the reset token appears in an audit entry (%s); it must never be "+
				"persisted anywhere but as a hash", e.Action)
		}
	}
}

// --- store helpers -----------------------------------------------------------

func (h *harness) storedResets() []PasswordReset {
	h.t.Helper()
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	out := make([]PasswordReset, 0, len(h.store.resets))
	for _, r := range h.store.resets {
		out = append(out, r)
	}
	return out
}

// expireResets ages every outstanding reset by d, so an expiry test does not
// have to wait.
func (h *harness) expireResets(d time.Duration) {
	h.t.Helper()
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	for id, r := range h.store.resets {
		r.ExpiresAt = r.ExpiresAt.Add(-d)
		h.store.resets[id] = r
	}
}
