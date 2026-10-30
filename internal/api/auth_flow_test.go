package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/auth"
)

func TestSetupFlow(t *testing.T) {
	h := newHarness(t)

	// Before setup: needs_setup true.
	rec := h.request(http.MethodGet, "/api/v1/setup", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("setup status = %d", rec.Code)
	}
	var status map[string]bool
	mustJSON(t, rec, &status)
	if !status["needs_setup"] {
		t.Fatal("expected needs_setup=true before setup")
	}

	admin := h.setupAdmin()
	if len(admin.cookies) == 0 {
		t.Fatal("setup did not set cookies")
	}

	// After setup: needs_setup false, and a second setup is rejected.
	rec = h.request(http.MethodGet, "/api/v1/setup", nil)
	mustJSON(t, rec, &status)
	if status["needs_setup"] {
		t.Fatal("expected needs_setup=false after setup")
	}
	rec = h.request(http.MethodPost, "/api/v1/setup", setupRequest{Email: "x@y.z", Password: "supersecret"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("second setup = %d, want 409", rec.Code)
	}
}

func TestLoginSuccessAndFailure(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin()

	if _, code := h.login("admin@example.com", "wrongpass", ""); code != http.StatusUnauthorized {
		t.Fatalf("bad password login = %d, want 401", code)
	}
	if _, code := h.login("nobody@example.com", "supersecret", ""); code != http.StatusUnauthorized {
		t.Fatalf("unknown user login = %d, want 401", code)
	}
	sess, code := h.login("admin@example.com", "supersecret", "")
	if code != http.StatusOK {
		t.Fatalf("valid login = %d, want 200", code)
	}
	// The session authenticates /me.
	rec := h.requestWith(http.MethodGet, "/api/v1/me", nil, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("/me = %d, want 200", rec.Code)
	}
}

func TestUnauthenticatedRejected(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin()
	rec := h.request(http.MethodGet, "/api/v1/me", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous /me = %d, want 401", rec.Code)
	}
}

func TestAccountLockout(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin()
	// lockoutThreshold consecutive failures lock the account.
	for i := 0; i < lockoutThreshold; i++ {
		if _, code := h.login("admin@example.com", "wrongpass", ""); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i, code)
		}
	}
	// Now even the correct password is refused with 429.
	if _, code := h.login("admin@example.com", "supersecret", ""); code != http.StatusTooManyRequests {
		t.Fatalf("post-lockout login = %d, want 429", code)
	}
}

func TestLoginRateLimit(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin()
	got429 := false
	for i := 0; i < loginRateLimit+5; i++ {
		// Distinct unknown users so lockout never triggers first.
		_, code := h.login("nobody@example.com", "supersecret", "")
		if code == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Fatal("expected a 429 after exceeding the login rate limit")
	}
}

func TestTwoFactorLogin(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	// Enroll TOTP.
	rec := h.requestWith(http.MethodPost, "/api/v1/me/totp/enroll", stepUp{Password: "supersecret"}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("enroll = %d, body %s", rec.Code, rec.Body.String())
	}
	var enroll map[string]string
	mustJSON(t, rec, &enroll)
	secret := enroll["secret"]

	// Verify with a valid code activates 2FA and returns backup codes.
	code, _ := auth.TOTPCode(secret, time.Now())
	rec = h.requestWith(http.MethodPost, "/api/v1/me/totp/verify", totpVerifyRequest{Code: code}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("verify = %d, body %s", rec.Code, rec.Body.String())
	}
	var verify struct {
		BackupCodes []string `json:"backup_codes"`
	}
	mustJSON(t, rec, &verify)
	if len(verify.BackupCodes) == 0 {
		t.Fatal("no backup codes returned")
	}

	// Login without the second factor now fails.
	if _, c := h.login("admin@example.com", "supersecret", ""); c != http.StatusUnauthorized {
		t.Fatalf("login without 2FA = %d, want 401", c)
	}
	// Login with a fresh TOTP code succeeds.
	fresh, _ := auth.TOTPCode(secret, time.Now())
	if _, c := h.login("admin@example.com", "supersecret", fresh); c != http.StatusOK {
		t.Fatalf("login with 2FA = %d, want 200", c)
	}
	// A backup code also works and is single-use.
	rec = h.request(http.MethodPost, "/api/v1/auth/login", loginRequest{
		Email: "admin@example.com", Password: "supersecret", BackupCode: verify.BackupCodes[0],
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("backup-code login = %d, want 200", rec.Code)
	}
	rec = h.request(http.MethodPost, "/api/v1/auth/login", loginRequest{
		Email: "admin@example.com", Password: "supersecret", BackupCode: verify.BackupCodes[0],
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("reused backup code = %d, want 401", rec.Code)
	}
}

func TestCSRFEnforced(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	body := monitorRequest{Name: "m", Type: "fake", Interval: 60}

	// With CSRF header (from the session) → allowed.
	rec := h.requestWith(http.MethodPost, "/api/v1/monitors", body, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("with CSRF = %d, want 201, body %s", rec.Code, rec.Body.String())
	}

	// Same cookies but no CSRF header → 403.
	noCSRF := &session{cookies: admin.cookies}
	rec = h.requestWith(http.MethodPost, "/api/v1/monitors", body, noCSRF)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("without CSRF = %d, want 403", rec.Code)
	}
}

func TestRBACViewerDenied(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	viewer := h.createUser(admin, "viewer@example.com", "viewer")

	// Viewer can read monitors.
	rec := h.requestWith(http.MethodGet, "/api/v1/monitors", nil, viewer)
	if rec.Code != http.StatusOK {
		t.Fatalf("viewer list monitors = %d, want 200", rec.Code)
	}
	// Viewer cannot create.
	rec = h.requestWith(http.MethodPost, "/api/v1/monitors",
		monitorRequest{Name: "m", Type: "fake", Interval: 60}, viewer)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer create monitor = %d, want 403", rec.Code)
	}
	// Viewer cannot reach admin-only user list.
	rec = h.requestWith(http.MethodGet, "/api/v1/users", nil, viewer)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer list users = %d, want 403", rec.Code)
	}
}

func TestAPIKeyAuth(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	// Create a write-scoped key.
	rec := h.requestWith(http.MethodPost, "/api/v1/me/api-keys",
		createAPIKeyRequest{Name: "ci", Scopes: []string{ScopeRead, ScopeWrite}, stepUp: stepUp{Password: "supersecret"}}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create key = %d, body %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Secret string `json:"secret"`
	}
	mustJSON(t, rec, &created)
	if created.Secret == "" {
		t.Fatal("no secret returned")
	}

	// The key authenticates a read without CSRF (no cookie).
	rec = h.requestKey(http.MethodGet, "/api/v1/monitors", created.Secret, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("api-key list = %d, want 200", rec.Code)
	}
	// And a write, since it has the write scope, bypassing CSRF.
	rec = h.requestKey(http.MethodPost, "/api/v1/monitors", created.Secret,
		monitorRequest{Name: "via-key", Type: "fake", Interval: 60})
	if rec.Code != http.StatusCreated {
		t.Fatalf("api-key create = %d, want 201, body %s", rec.Code, rec.Body.String())
	}

	// A bogus token is rejected.
	rec = h.requestKey(http.MethodGet, "/api/v1/monitors", "statwarden_bogus", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bogus key = %d, want 401", rec.Code)
	}
}

func TestAPIKeyReadScopeCannotWrite(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	rec := h.requestWith(http.MethodPost, "/api/v1/me/api-keys",
		createAPIKeyRequest{Name: "ro", Scopes: []string{ScopeRead}, stepUp: stepUp{Password: "supersecret"}}, admin)
	var created struct {
		Secret string `json:"secret"`
	}
	mustJSON(t, rec, &created)

	rec = h.requestKey(http.MethodPost, "/api/v1/monitors", created.Secret,
		monitorRequest{Name: "x", Type: "fake", Interval: 60})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("read-only key write = %d, want 403", rec.Code)
	}
}

func TestLogoutClearsSession(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	rec := h.requestWith(http.MethodPost, "/api/v1/auth/logout", nil, admin)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout = %d, want 204", rec.Code)
	}
	// The session is gone server-side; reusing the cookie fails.
	rec = h.requestWith(http.MethodGet, "/api/v1/me", nil, admin)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("post-logout /me = %d, want 401", rec.Code)
	}
}

// sessionViews decodes the session list for a caller.
func sessionViews(t *testing.T, h *harness, sess *session) []struct {
	ID        string `json:"id"`
	Current   bool   `json:"current"`
	UserAgent string `json:"user_agent"`
} {
	t.Helper()
	rec := h.requestWith(http.MethodGet, "/api/v1/me/sessions", nil, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("list sessions = %d, body %s", rec.Code, rec.Body.String())
	}
	var out []struct {
		ID        string `json:"id"`
		Current   bool   `json:"current"`
		UserAgent string `json:"user_agent"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode sessions: %v", err)
	}
	return out
}

// TestSessionListExposesFingerprintNotHash pins the security property the
// fingerprint exists for: the list must address each session uniquely without
// ever handing out the stored hash, which is what the session cookie has to
// reproduce.
func TestSessionListExposesFingerprintNotHash(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	if _, code := h.login("admin@example.com", "supersecret", ""); code != http.StatusOK {
		t.Fatalf("second login = %d", code)
	}

	views := sessionViews(t, h, admin)
	if len(views) < 2 {
		t.Fatalf("want at least 2 sessions, got %d", len(views))
	}

	seen := map[string]bool{}
	for _, v := range views {
		if v.ID == "" {
			t.Error("session id is empty; the list cannot be keyed or addressed")
		}
		if len(v.ID) != auth.SessionFingerprintLen {
			t.Errorf("id %q is not a fingerprint (len %d, want %d)", v.ID, len(v.ID), auth.SessionFingerprintLen)
		}
		if seen[v.ID] {
			t.Errorf("duplicate session id %q — a keyed list would throw", v.ID)
		}
		seen[v.ID] = true
	}

	// No response body may contain a raw stored hash (64 hex chars).
	if body := h.requestWith(http.MethodGet, "/api/v1/me/sessions", nil, admin).Body.String(); len(body) > 0 {
		for _, tok := range strings.Fields(strings.NewReplacer(`"`, " ", ",", " ", ":", " ").Replace(body)) {
			if len(tok) == 64 && strings.Trim(tok, "0123456789abcdef") == "" {
				t.Errorf("response contains what looks like a raw session hash: %s", tok)
			}
		}
	}
}

// TestRevokeSessionByFingerprint covers the happy path and the two ways it must
// refuse: another user's session, and an unknown selector.
func TestRevokeSessionByFingerprint(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	if _, code := h.login("admin@example.com", "supersecret", ""); code != http.StatusOK {
		t.Fatalf("second login = %d", code)
	}

	views := sessionViews(t, h, admin)
	var target string
	for _, v := range views {
		if !v.Current {
			target = v.ID
			break
		}
	}
	if target == "" {
		t.Fatal("no non-current session to revoke")
	}

	rec := h.requestWith(http.MethodDelete, "/api/v1/me/sessions/"+target, nil, admin)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d, body %s", rec.Code, rec.Body.String())
	}
	for _, v := range sessionViews(t, h, admin) {
		if v.ID == target {
			t.Error("session still present after revoke")
		}
	}

	// Unknown selector.
	if rec := h.requestWith(http.MethodDelete, "/api/v1/me/sessions/deadbeefdeadbeef", nil, admin); rec.Code != http.StatusNotFound {
		t.Errorf("unknown fingerprint = %d, want 404", rec.Code)
	}

	// IDOR: another user's fingerprint must not resolve, even though it is a
	// perfectly valid fingerprint of a real session.
	other := h.createUser(admin, "other@example.com", "user")
	otherViews := sessionViews(t, h, other)
	if len(otherViews) == 0 {
		t.Fatal("other user has no session")
	}
	victim := otherViews[0].ID
	if rec := h.requestWith(http.MethodDelete, "/api/v1/me/sessions/"+victim, nil, admin); rec.Code != http.StatusNotFound {
		t.Errorf("cross-user revoke = %d, want 404", rec.Code)
	}
	if len(sessionViews(t, h, other)) != len(otherViews) {
		t.Error("another user's session was deleted")
	}

	// "others" still works.
	if rec := h.requestWith(http.MethodDelete, "/api/v1/me/sessions/others", nil, admin); rec.Code != http.StatusNoContent {
		t.Errorf("revoke others = %d, want 204", rec.Code)
	}
}

// The setup endpoints carry a rate-limit budget like every other public route.
//
// They sat outside every limiter: unauthenticated, reachable before anybody has
// logged in, and the POST decodes and validates a body. handleSetup does answer
// 409 once a user exists, so this is a flood guard rather than an authorization
// fix — but "cheap to reject" is not the same as "free", and it was the only
// public endpoint pair with no budget at all.
func TestSetupIsRateLimited(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin() // from here on every /setup call is a 409

	got429 := false
	for i := 0; i < publicRateLimit+5; i++ {
		rec := h.request(http.MethodPost, "/api/v1/setup", setupRequest{
			Email: "someone@example.com", Password: "supersecret", DisplayName: "X",
		})
		if rec.Code == http.StatusTooManyRequests {
			got429 = true
			break
		}
		if rec.Code != http.StatusConflict {
			t.Fatalf("POST /setup after setup = %d, want 409 or 429", rec.Code)
		}
	}
	if !got429 {
		t.Fatalf("no 429 after %d POSTs to /setup — the endpoint has no rate limit",
			publicRateLimit+5)
	}
}
