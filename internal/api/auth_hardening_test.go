package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
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

// The re-authentication checks behind a session — step-up, password change,
// address change, TOTP enrolment — had no attempt limit at all. A borrowed
// session could therefore guess the password (or a six-digit code) at request
// speed, which the login form's lockout exists to prevent. They now share one
// failure budget per account, and once it is spent even the right answer waits
// out the cool-off.
func TestReauthenticationChecksAreAttemptLimited(t *testing.T) {
	for _, tc := range []struct {
		name         string
		method, path string
		wrong, right any
	}{
		{
			name: "step-up", method: http.MethodPost, path: "/api/v1/me/api-keys",
			wrong: createAPIKeyRequest{Name: "k", stepUp: stepUp{Password: "guess"}},
			right: createAPIKeyRequest{Name: "k", stepUp: stepUp{Password: "supersecret"}},
		},
		{
			name: "password change", method: http.MethodPut, path: "/api/v1/me/password",
			wrong: changePasswordRequest{CurrentPassword: "guess", NewPassword: "another-password"},
			right: changePasswordRequest{CurrentPassword: "supersecret", NewPassword: "another-password"},
		},
		{
			name: "address change", method: http.MethodPut, path: "/api/v1/me/email",
			wrong: changeEmailRequest{CurrentPassword: "guess", Email: "moved@example.com"},
			right: changeEmailRequest{CurrentPassword: "supersecret", Email: "moved@example.com"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			admin := h.setupAdmin()
			for i := 1; i <= lockoutThreshold; i++ {
				if rec := h.requestWith(tc.method, tc.path, tc.wrong, admin); rec.Code != http.StatusForbidden {
					t.Fatalf("wrong attempt %d = %d, want 403; body %s", i, rec.Code, rec.Body.String())
				}
			}
			rec := h.requestWith(tc.method, tc.path, tc.right, admin)
			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("right answer after %d failures = %d, want 429; body %s",
					lockoutThreshold, rec.Code, rec.Body.String())
			}
		})
	}

	t.Run("TOTP enrolment code", func(t *testing.T) {
		h := newHarness(t)
		admin := h.setupAdmin()
		rec := h.requestWith(http.MethodPost, "/api/v1/me/totp/enroll", stepUp{Password: "supersecret"}, admin)
		if rec.Code != http.StatusOK {
			t.Fatalf("enroll = %d, body %s", rec.Code, rec.Body.String())
		}
		var enroll map[string]string
		mustJSON(t, rec, &enroll)
		code, err := auth.TOTPCode(enroll["secret"], time.Now())
		if err != nil {
			t.Fatalf("TOTPCode: %v", err)
		}
		wrong := "000000"
		if code == wrong {
			wrong = "111111"
		}
		for i := 1; i <= lockoutThreshold; i++ {
			rec := h.requestWith(http.MethodPost, "/api/v1/me/totp/verify", totpVerifyRequest{Code: wrong}, admin)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("wrong code %d = %d, want 400", i, rec.Code)
			}
		}
		rec = h.requestWith(http.MethodPost, "/api/v1/me/totp/verify", totpVerifyRequest{Code: code}, admin)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("right code after %d failures = %d, want 429", lockoutThreshold, rec.Code)
		}
	})
}

// A TOTP code verifies for about ninety seconds (its own step plus one either
// side), and nothing remembered that it had already been used. Someone who saw
// a code typed — over a shoulder, in a phishing proxy, in a screen recording —
// could sign in with it again inside that window. RFC 6238 §5.2 says a verifier
// must not accept the same code twice; each account now remembers the last step
// it accepted.
func TestTOTPCodeCannotBeReplayed(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	secret, _ := enableTOTPDirect(t, h, admin.userID)
	code, err := auth.TOTPCode(secret, time.Now())
	if err != nil {
		t.Fatalf("TOTPCode: %v", err)
	}

	if _, c := h.login("admin@example.com", "supersecret", code); c != http.StatusOK {
		t.Fatalf("first use of the code = %d, want 200", c)
	}
	if _, c := h.login("admin@example.com", "supersecret", code); c != http.StatusUnauthorized {
		t.Fatalf("second use of the same code = %d, want 401", c)
	}

	// Nor can a code already spent on a login be spent again on a step-up.
	rec := h.requestWith(http.MethodPost, "/api/v1/me/api-keys",
		createAPIKeyRequest{Name: "k", stepUp: stepUp{Password: "supersecret", TOTPCode: code}}, admin)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("step-up with the spent code = %d, want 403", rec.Code)
	}

	// The next step's code is a new code and is accepted.
	next, err := auth.TOTPCode(secret, time.Now().Add(30*time.Second))
	if err != nil {
		t.Fatalf("TOTPCode: %v", err)
	}
	if _, c := h.login("admin@example.com", "supersecret", next); c != http.StatusOK {
		t.Fatalf("login with the next step's code = %d, want 200", c)
	}
}

// steppingClock is a server clock a test moves forward one TOTP step at a time,
// for tests that need more authenticator codes than the skew window holds: each
// code is single-use, and only three steps verify at any one moment.
type steppingClock struct {
	mu sync.Mutex
	t  time.Time
}

func newSteppingClock() *steppingClock { return &steppingClock{t: time.Now()} }

func (c *steppingClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// nextCode advances the clock one step and returns the code for it.
func (c *steppingClock) nextCode(t *testing.T, secret string) string {
	t.Helper()
	c.mu.Lock()
	c.t = c.t.Add(30 * time.Second)
	at := c.t
	c.mu.Unlock()
	code, err := auth.TOTPCode(secret, at)
	if err != nil {
		t.Fatalf("TOTPCode: %v", err)
	}
	return code
}

// Login CSRF. The public auth endpoints sit outside the CSRF check — there is no
// session yet to bind a token to — and decodeJSON reads any body as JSON whatever
// it is labelled. A cross-site <form enctype="text/plain"> can be shaped into a
// valid JSON object and is a "simple" request the browser sends without a CORS
// preflight, so any page could sign a visitor into an account of the page's
// choosing. Requiring application/json forces a preflight, which a foreign
// origin does not pass.
func TestPublicAuthEndpointsRequireAJSONContentType(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin()

	body := `{"email":"admin@example.com","password":"supersecret"}`
	for _, path := range []string{
		"/api/v1/auth/login",
		"/api/v1/setup",
		"/api/v1/auth/password-reset",
		"/api/v1/auth/password-reset/confirm",
	} {
		for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x", ""} {
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			if ct != "" {
				req.Header.Set("Content-Type", ct)
			}
			rec := httptest.NewRecorder()
			h.handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnsupportedMediaType {
				t.Errorf("POST %s as %q = %d, want 415", path, ct, rec.Code)
			}
			for _, c := range rec.Result().Cookies() {
				if c.Name == sessionCookie && c.Value != "" {
					t.Errorf("POST %s as %q issued a session", path, ct)
				}
			}
		}
	}

	// A parameterised JSON media type is still JSON.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login as application/json; charset=utf-8 = %d, want 200", rec.Code)
	}
}

// Consuming a backup code was read, compare, write: the codes were read with the
// credentials, the matching one removed in Go, and the rest written back. Two
// requests carrying the same code both read the full list, both found their
// code and both got in — a single-use code used several times. Two requests
// carrying DIFFERENT codes each wrote back a list that still held the other's,
// so one burned code came back to life.
func TestBackupCodeConsumptionIsAtomic(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	_, plain := enableTOTPDirect(t, h, admin.userID)

	login := func(code string) int {
		return h.request(http.MethodPost, "/api/v1/auth/login", loginRequest{
			Email: "admin@example.com", Password: "supersecret", BackupCode: code,
		}).Code
	}

	// The same code, several times at once: exactly one may succeed.
	const racers = 4
	codes := make(chan int, racers)
	var wg sync.WaitGroup
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- login(plain[0])
		}()
	}
	wg.Wait()
	close(codes)
	ok := 0
	for c := range codes {
		if c == http.StatusOK {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("one backup code admitted %d concurrent logins, want exactly 1", ok)
	}

	// Two different codes at once: both may succeed, and neither may survive.
	wg.Add(2)
	for _, c := range plain[1:3] {
		go func() {
			defer wg.Done()
			login(c)
		}()
	}
	wg.Wait()
	for i, c := range plain[1:3] {
		if got := login(c); got != http.StatusUnauthorized {
			t.Errorf("backup code %d after it was used = %d, want 401: a concurrent "+
				"write brought it back", i+1, got)
		}
	}
}
