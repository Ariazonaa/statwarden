package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	monreg "github.com/Ariazonaa/statwarden/internal/monitor/registry"
	notifyreg "github.com/Ariazonaa/statwarden/internal/notify/registry"

	"github.com/Ariazonaa/statwarden/internal/auth"
)

// newTTLHarness builds a server with an explicit Options.SessionTTL, standing in
// for SESSION_TTL_DAYS from the environment.
func newTTLHarness(t *testing.T, ttl time.Duration) *harness {
	t.Helper()
	store := NewMemStore()
	eng := newFakeEngine()
	mr := monreg.New()
	mr.Register(fakeChecker{})
	nr := notifyreg.New()
	nr.Register(fakeNotifier{})
	h := New(Options{
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:           store,
		Engine:          eng,
		MonitorRegistry: mr,
		NotifyRegistry:  nr,
		SessionTTL:      ttl,
		PublicBaseURL:   "https://statwarden.example",
	})
	return &harness{t: t, handler: h, store: store, engine: eng}
}

// TestEffectiveSessionTTLPrecedence pins the resolution order: the settings row
// wins when it is present and valid, and the Options value is the fallback for
// every other case.
//
// The clamp matters as much as the precedence. The settings table is writable
// directly in psql and a restored database never passed the write-side
// validator, so a garbage row must never be able to produce a TTL of zero or
// less — that would mint already-expired sessions and lock every user out,
// including the admin who would have to undo it.
func TestEffectiveSessionTTLPrecedence(t *testing.T) {
	const optionsTTL = 30 * 24 * time.Hour

	tests := []struct {
		name  string
		row   string // "" = no row at all
		want  time.Duration
		bound bool // when true, only assert the safety floor
	}{
		{name: "no row falls back to the configured value", row: "", want: optionsTTL},
		{name: "a valid row wins", row: `7`, want: 7 * 24 * time.Hour},
		{name: "one day is accepted", row: `1`, want: 24 * time.Hour},
		{name: "the maximum is accepted", row: `365`, want: 365 * 24 * time.Hour},
		// Each of these would be a lockout or an unbounded cookie if honoured.
		{name: "zero falls back", row: `0`, want: optionsTTL},
		{name: "negative falls back", row: `-1`, want: optionsTTL},
		{name: "over the ceiling falls back", row: `10000`, want: optionsTTL},
		{name: "a string falls back", row: `"forever"`, want: optionsTTL},
		{name: "null falls back", row: `null`, want: optionsTTL},
		{name: "an object falls back", row: `{"days":7}`, want: optionsTTL},
		// A fraction must not truncate to zero days.
		{name: "a fraction falls back", row: `0.5`, want: optionsTTL},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newTTLHarness(t, optionsTTL)
			if tc.row != "" {
				if _, err := h.store.SetSetting(t.Context(), settingSessionTTLDays, json.RawMessage(tc.row)); err != nil {
					t.Fatalf("seed setting: %v", err)
				}
			}
			// effectiveSessionTTL is exercised directly over the same store the
			// handler uses; the end-to-end effect is covered by the next test.
			srv := &server{
				logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
				store:      h.store,
				sessionTTL: optionsTTL,
			}
			got := srv.effectiveSessionTTL(t.Context())
			if got != tc.want {
				t.Errorf("effectiveSessionTTL = %s, want %s", got, tc.want)
			}
			if got <= 0 {
				t.Fatalf("effectiveSessionTTL = %s; a non-positive TTL mints already-expired "+
					"sessions and locks everybody out", got)
			}
			if got < minSessionTTL {
				t.Errorf("effectiveSessionTTL = %s, below the floor %s", got, minSessionTTL)
			}
			_ = tc.bound
		})
	}
}

// TestSessionTTLAppliesToIssuedSession proves the resolved value actually
// reaches both the stored session and the cookie, rather than only the helper.
func TestSessionTTLAppliesToIssuedSession(t *testing.T) {
	h := newTTLHarness(t, 30*24*time.Hour)
	h.setupAdmin()
	if _, err := h.store.SetSetting(t.Context(), settingSessionTTLDays, json.RawMessage(`2`)); err != nil {
		t.Fatalf("seed setting: %v", err)
	}

	before := time.Now()
	sess, code := h.login("admin@example.com", "supersecret", "")
	if code != http.StatusOK {
		t.Fatalf("login = %d", code)
	}

	// Resolve THIS session by its cookie rather than scanning the user's
	// sessions: the setup call already left a 30-day session behind, and picking
	// the wrong row would make this test pass or fail for the wrong reason.
	var token string
	for _, c := range sess.cookies {
		if c.Name == sessionCookie {
			token = c.Value
		}
	}
	if token == "" {
		t.Fatal("login set no session cookie")
	}
	stored, err := h.store.GetSession(t.Context(), auth.HashToken(token))
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	wantMin := before.Add(2 * 24 * time.Hour).Add(-time.Minute)
	wantMax := before.Add(2 * 24 * time.Hour).Add(time.Minute)
	if stored.ExpiresAt.Before(wantMin) || stored.ExpiresAt.After(wantMax) {
		t.Errorf("session expires at %s, want ~%s — the settings row must override the "+
			"configured default", stored.ExpiresAt, before.Add(2*24*time.Hour))
	}

	// And the cookie Max-Age follows it, so the browser does not hold a cookie
	// for a month past the server-side expiry.
	var maxAge int
	for _, c := range sess.cookies {
		if c.Name == sessionCookie {
			maxAge = c.MaxAge
		}
	}
	if want := int((2 * 24 * time.Hour).Seconds()); maxAge != want {
		t.Errorf("session cookie Max-Age = %d, want %d", maxAge, want)
	}
}

// TestSetSessionTTLSettingValidates pins the write-side check. Without it an
// admin could store 0 through the settings UI and lock the whole instance out;
// the read-side clamp would save them, but they would get no error and no clue.
func TestSetSessionTTLSettingValidates(t *testing.T) {
	h := newTTLHarness(t, 30*24*time.Hour)
	admin := h.setupAdmin()
	target := "/api/v1/settings/" + settingSessionTTLDays

	for _, bad := range []string{`0`, `-1`, `10000`, `"forever"`, `null`, `0.5`} {
		rec := h.requestWith(http.MethodPut, target, json.RawMessage(bad), admin)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %s = %d for %s, want 400", target, rec.Code, bad)
		}
		// The rejected value must not have been stored.
		if _, err := h.store.GetSetting(t.Context(), settingSessionTTLDays); err == nil {
			t.Errorf("the rejected value %s was written to the store anyway", bad)
		}
	}

	if rec := h.requestWith(http.MethodPut, target, json.RawMessage(`14`), admin); rec.Code != http.StatusOK {
		t.Fatalf("PUT a valid value = %d, body %s", rec.Code, rec.Body.String())
	}
	got, err := h.store.GetSetting(t.Context(), settingSessionTTLDays)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got.Value) != `14` {
		t.Errorf("stored value = %s, want 14", got.Value)
	}
}

// TestSessionTTLSettingIsAdminOnly pins that the knob stays behind the admin
// role and CSRF. It changes the authentication window for every user.
func TestSessionTTLSettingIsAdminOnly(t *testing.T) {
	h := newTTLHarness(t, 30*24*time.Hour)
	admin := h.setupAdmin()
	user := h.createUser(admin, "user@example.com", "user")
	target := "/api/v1/settings/" + settingSessionTTLDays

	if rec := h.requestWith(http.MethodPut, target, json.RawMessage(`7`), user); rec.Code != http.StatusForbidden {
		t.Errorf("non-admin PUT = %d, want 403", rec.Code)
	}
	stripped := &session{cookies: admin.cookies}
	if rec := h.requestWith(http.MethodPut, target, json.RawMessage(`7`), stripped); rec.Code != http.StatusForbidden {
		t.Errorf("admin PUT without CSRF = %d, want 403", rec.Code)
	}
}

// TestSessionTTLSurvivesStoreFailure pins the last safety property: the resolver
// runs on the login path and inside the sliding-expiry touch, so it must fall
// back rather than fail. A nil store stands in for the degenerate case.
func TestSessionTTLSurvivesStoreFailure(t *testing.T) {
	s := &server{
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		sessionTTL: 12 * 24 * time.Hour,
	}
	if got := s.effectiveSessionTTL(t.Context()); got != 12*24*time.Hour {
		t.Errorf("effectiveSessionTTL with no store = %s, want the configured value", got)
	}
	// A zero configured TTL must not propagate either.
	s.sessionTTL = 0
	if got := s.effectiveSessionTTL(t.Context()); got != defaultSessionTTL {
		t.Errorf("effectiveSessionTTL with a zero configured value = %s, want %s",
			got, defaultSessionTTL)
	}
}
