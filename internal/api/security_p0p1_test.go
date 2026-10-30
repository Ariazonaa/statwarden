package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/auth"
)

// --- Item 1: API-key scope escalation -------------------------------------

// TestAPIKeyCannotEscalateScope pins that a read-only API key cannot mint a
// key more privileged than itself. Without the cap in handleCreateAPIKey the
// self-service mint route (which requires no scope of its own) lets a read key
// hand itself write or admin scope and walk out of its restrictions.
func TestAPIKeyCannotEscalateScope(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	// Mint a read-only key from the admin session.
	rec := h.requestWith(http.MethodPost, "/api/v1/me/api-keys",
		createAPIKeyRequest{Name: "ro", Scopes: []string{ScopeRead}, stepUp: stepUp{Password: "supersecret"}}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create read key = %d, body %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Secret string `json:"secret"`
	}
	mustJSON(t, rec, &created)

	// The read-only key must not be able to mint a write key…
	rec = h.requestKey(http.MethodPost, "/api/v1/me/api-keys", created.Secret,
		createAPIKeyRequest{Name: "esc-write", Scopes: []string{ScopeWrite}, stepUp: stepUp{Password: "supersecret"}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("read key minting write scope = %d, want 403; body %s", rec.Code, rec.Body.String())
	}
	// …nor an admin key.
	rec = h.requestKey(http.MethodPost, "/api/v1/me/api-keys", created.Secret,
		createAPIKeyRequest{Name: "esc-admin", Scopes: []string{ScopeAdmin}, stepUp: stepUp{Password: "supersecret"}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("read key minting admin scope = %d, want 403; body %s", rec.Code, rec.Body.String())
	}
	// …and no longer a same-scope key either. This assertion used to be the
	// opposite, on the reasoning that equal privilege is not an escalation. It
	// is still a PERSISTENCE primitive: a stolen key that can mint a sibling
	// outlives the revocation of the original, so key management is now
	// session-only (see requireSession).
	rec = h.requestKey(http.MethodPost, "/api/v1/me/api-keys", created.Secret,
		createAPIKeyRequest{Name: "ro2", Scopes: []string{ScopeRead}, stepUp: stepUp{Password: "supersecret"}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("read key minting a sibling key = %d, want 403; body %s", rec.Code, rec.Body.String())
	}
}

// The scope cap inside handleCreateAPIKey stays as defence in depth even though
// no API key can reach that route any more, so it is asserted directly.
func TestGrantsScopeCapsToOwnRights(t *testing.T) {
	readKey := &principal{kind: principalAPIKey, scopes: []string{ScopeRead}}
	if readKey.grantsScope(ScopeWrite) || readKey.grantsScope(ScopeAdmin) {
		t.Error("a read-only key must not grant write or admin")
	}
	if !readKey.grantsScope(ScopeRead) {
		t.Error("a read-only key may grant read")
	}
	writeKey := &principal{kind: principalAPIKey, scopes: []string{ScopeRead, ScopeWrite}}
	if writeKey.grantsScope(ScopeAdmin) {
		t.Error("a write key must not grant admin")
	}
	if !writeKey.grantsScope(ScopeWrite) {
		t.Error("a write key may grant write")
	}
	viewer := &principal{kind: principalSession, role: auth.RoleViewer}
	if viewer.grantsScope(ScopeWrite) || viewer.grantsScope(ScopeAdmin) {
		t.Error("a viewer session must not grant write or admin")
	}
	if adminSess := (&principal{kind: principalSession, role: auth.RoleAdmin}); !adminSess.grantsScope(ScopeAdmin) {
		t.Error("an admin session may grant admin")
	}
}

// TestAdminSessionCanStillMintPrivilegedKey guards against the cap being too
// aggressive: an admin cookie session must keep the ability to mint an
// admin-scoped key (the regression the previous behaviour relied on).
func TestAdminSessionCanStillMintPrivilegedKey(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	rec := h.requestWith(http.MethodPost, "/api/v1/me/api-keys",
		createAPIKeyRequest{Name: "ci", Scopes: []string{ScopeRead, ScopeWrite, ScopeAdmin}, stepUp: stepUp{Password: "supersecret"}}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin session minting admin key = %d, want 201; body %s", rec.Code, rec.Body.String())
	}
}

// --- Item 2: badge ID enumeration -----------------------------------------

// TestBadgeRequiresOpaqueToken pins that the public badge endpoints are keyed
// by an unguessable, revocable token — a monitor cannot be reached by walking
// sequential ids, and a revoked token stops resolving.
func TestBadgeRequiresOpaqueToken(t *testing.T) {
	h := newExtHarness(t, nil, nil)
	admin := h.setupAdmin()
	id := h.createMonitorFake(admin)

	// The monitor's sequential id is not a valid badge address.
	rec := h.request(http.MethodGet, "/api/v1/badge/"+itoa64(id)+"/status.svg", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("badge by raw id = %d, want 404 (no enumeration); body %s", rec.Code, rec.Body.String())
	}

	// The opaque token resolves.
	token := h.mintBadge(admin, id)
	rec = h.request(http.MethodGet, "/api/v1/badge/"+token+"/status.svg", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("badge by token = %d, want 200; body %s", rec.Code, rec.Body.String())
	}

	// Revoking the token stops the badge from resolving.
	rec = h.requestWith(http.MethodDelete, "/api/v1/monitors/"+itoa64(id)+"/badge", nil, admin)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke badge = %d, want 204; body %s", rec.Code, rec.Body.String())
	}
	rec = h.request(http.MethodGet, "/api/v1/badge/"+token+"/status.svg", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("badge by revoked token = %d, want 404; body %s", rec.Code, rec.Body.String())
	}
}

// --- Item 3: /metrics closed by default -----------------------------------

// TestMetricsClosedWithoutToken pins that a wired /metrics handler is NOT served
// when no scrape token is configured: an empty token must close the endpoint,
// not open it to anyone.
func TestMetricsClosedWithoutToken(t *testing.T) {
	body := "statwarden_metric 1"
	metrics := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// No token: closed (404), and the metric body must not leak.
	closed := New(Options{Logger: log, Store: NewMemStore(), Metrics: metrics, MetricsToken: ""})
	rec := httptest.NewRecorder()
	closed.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("/metrics with empty token = %d, want 404", rec.Code)
	}
	if got := rec.Body.String(); strings.Contains(got, body) {
		t.Fatalf("/metrics leaked the metrics body with no token configured: %q", got)
	}

	// With a token, the wired handler serves (it enforces the token itself).
	open := New(Options{Logger: log, Store: NewMemStore(), Metrics: metrics, MetricsToken: "s3cret"})
	rec = httptest.NewRecorder()
	open.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != body {
		t.Fatalf("/metrics with token = %d body %q, want 200 %q", rec.Code, rec.Body.String(), body)
	}
}

// --- Item 4: WebSocket origin -------------------------------------------

// recordingWSHub records whether the upgrade was reached, standing in for the
// real hub so the origin gate can be exercised without a real socket upgrade.
type recordingWSHub struct{ served bool }

func (h *recordingWSHub) ServeWS(w http.ResponseWriter, _ *http.Request, _ func() bool) {
	h.served = true
	w.WriteHeader(http.StatusSwitchingProtocols)
}

// TestWebSocketOriginValidated pins that a cross-origin handshake is rejected
// before the hub adopts any identity, while a same-origin request and a
// non-browser request with no Origin are allowed through.
func TestWebSocketOriginValidated(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	newServer := func() (http.Handler, *recordingWSHub) {
		hub := &recordingWSHub{}
		h := New(Options{
			Logger:        log,
			Store:         NewMemStore(),
			Hub:           hub,
			PublicBaseURL: "https://statwarden.example",
		})
		return h, hub
	}

	// Cross-origin: rejected, hub never reached.
	h, hub := newServer()
	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin /ws = %d, want 403", rec.Code)
	}
	if hub.served {
		t.Fatal("cross-origin /ws reached the hub; identity would have been adopted")
	}

	// Same origin as the configured public base URL: allowed.
	h, hub = newServer()
	req = httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Header.Set("Origin", "https://statwarden.example")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !hub.served {
		t.Fatalf("same-origin /ws did not reach the hub (code %d)", rec.Code)
	}

	// No Origin (non-browser client): allowed.
	h, hub = newServer()
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ws", nil))
	if !hub.served {
		t.Fatalf("originless /ws did not reach the hub (code %d)", rec.Code)
	}

	// A multi-hostname deployment lists extra origins; one of them is allowed,
	// while an origin on neither the base URL nor the allow-list stays rejected.
	allowServer := func() (http.Handler, *recordingWSHub) {
		hub := &recordingWSHub{}
		h := New(Options{
			Logger:         log,
			Store:          NewMemStore(),
			Hub:            hub,
			PublicBaseURL:  "https://statwarden.example",
			AllowedOrigins: []string{"https://nas.example"},
		})
		return h, hub
	}
	h, hub = allowServer()
	req = httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Header.Set("Origin", "https://nas.example")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !hub.served {
		t.Fatalf("allowed extra origin /ws did not reach the hub (code %d)", rec.Code)
	}

	h, hub = allowServer()
	req = httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || hub.served {
		t.Fatalf("origin off the allow-list was not rejected (code %d, served %v)", rec.Code, hub.served)
	}
}

// --- Item 5: push POST-only ----------------------------------------------

// TestPushRejectsGet pins that the public push heartbeat endpoint accepts POST
// only: a GET form could be tripped by a prefetcher or crawler and record
// spurious beats.
func TestPushRejectsGet(t *testing.T) {
	h := newHarness(t)
	rec := h.request(http.MethodGet, "/api/v1/push/sometoken", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /push = %d, want 405", rec.Code)
	}
	// POST is routed (reaches the handler; 503 here because no recorder is wired,
	// which is decidedly not a 405 method rejection).
	rec = h.request(http.MethodPost, "/api/v1/push/sometoken", nil)
	if rec.Code == http.StatusMethodNotAllowed {
		t.Fatalf("POST /push = 405, want the method to be accepted")
	}
}

// --- read-only API keys must not manage account credentials -------------------

// mintKey creates an API key with the given scopes and returns its secret.
func mintKey(t *testing.T, h *harness, sess *session, scopes ...string) string {
	t.Helper()
	rec := h.requestWith(http.MethodPost, "/api/v1/me/api-keys",
		createAPIKeyRequest{Name: "k", Scopes: scopes, stepUp: stepUp{Password: "supersecret"}}, sess)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create api key = %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Secret string `json:"secret"`
	}
	mustJSON(t, rec, &body)
	if body.Secret == "" {
		t.Fatal("no secret returned")
	}
	return body.Secret
}

// TestReadOnlyKeyCannotTakeOverTheAccount is the account-takeover regression.
//
// The /me/* mutations sit outside requireRole — a viewer owns their own profile
// — so their only gate was requireAuth, which a READ-scoped key satisfies. API
// keys are also exempt from CSRF (there is no cookie to double-submit). A
// stolen read-only key could therefore change the account's e-mail address,
// trigger a password reset to it, disable 2FA and mint further keys: full
// takeover from a credential whose documented contract is "read gates GET
// endpoints".
func TestReadOnlyKeyCannotTakeOverTheAccount(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	readKey := mintKey(t, h, admin, ScopeRead)

	for _, tc := range []struct {
		name, method, path string
		body               any
	}{
		{"change email", http.MethodPut, "/api/v1/me/email", changeEmailRequest{Email: "attacker@evil.test"}},
		{"change password", http.MethodPut, "/api/v1/me/password", changePasswordRequest{CurrentPassword: "x", NewPassword: "yyyyyyyyyyyy"}},
		{"disable 2FA", http.MethodPost, "/api/v1/me/totp/disable", stepUp{Password: "supersecret"}},
		{"enroll 2FA", http.MethodPost, "/api/v1/me/totp/enroll", nil},
		{"regenerate backup codes", http.MethodPost, "/api/v1/me/totp/backup-codes", nil},
		{"mint another key", http.MethodPost, "/api/v1/me/api-keys", createAPIKeyRequest{Name: "persist", Scopes: []string{ScopeRead}}},
		{"revoke a session", http.MethodDelete, "/api/v1/me/sessions/others", nil},
		{"upload an avatar", http.MethodPost, "/api/v1/me/avatar", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.requestKey(tc.method, tc.path, readKey, tc.body)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s %s with a read-only key = %d, want 403; body %s",
					tc.method, tc.path, rec.Code, rec.Body.String())
			}
		})
	}

	// The address must be untouched after all of that.
	u, err := h.store.GetUserByID(context.Background(), admin.userID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if u.Email == "attacker@evil.test" {
		t.Fatal("a read-only key changed the account e-mail address")
	}

	// Reading stays allowed — that IS the scope's purpose.
	if rec := h.requestKey(http.MethodGet, "/api/v1/me", readKey, nil); rec.Code != http.StatusOK {
		t.Fatalf("GET /me with a read key = %d, want 200", rec.Code)
	}
}

// A WRITE-scoped key still cannot manage credentials: an API key is for
// automation, and rotating its owner's second factor is not automation. It may
// mutate ordinary resources, which is what the scope is for.
func TestWriteKeyCannotManageCredentialsButCanMutateResources(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	writeKey := mintKey(t, h, admin, ScopeRead, ScopeWrite)

	if rec := h.requestKey(http.MethodPut, "/api/v1/me/email", writeKey,
		changeEmailRequest{Email: "attacker@evil.test"}); rec.Code != http.StatusForbidden {
		t.Fatalf("write key changed the e-mail: %d, body %s", rec.Code, rec.Body.String())
	}
	rec := h.requestKey(http.MethodPost, "/api/v1/tags", writeKey, tagRequest{Name: "ops", Color: "#0ea5e9"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("write key creating a tag = %d, want 201; body %s", rec.Code, rec.Body.String())
	}
}

// --- readiness reflects whether anything is actually being checked ------------

// TestSchedulingReadiness pins that a process which checks NOTHING does not
// report itself ready.
//
// A reachable database used to be the whole readiness test, so a wrong or
// rotated ENCRYPTION_KEY — which makes every monitor config fail to decrypt and
// therefore fail its checker validation — produced a "healthy" instance that
// was silently monitoring nothing at all.
//
// Only TOTAL failure is fatal: one monitor with a broken config is a content
// problem, not a readiness problem, and must not take the instance out of
// service.
func TestSchedulingReadiness(t *testing.T) {
	tests := []struct {
		name                   string
		scheduled, unscheduled int
		wantReady              bool
	}{
		{"everything scheduled", 40, 0, true},
		{"partial failure stays in service", 40, 2, true},
		{"nothing loaded at all", 0, 0, true},
		{"nothing could be scheduled", 0, 12, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eng := newFakeEngine()
			eng.scheduled, eng.unscheduled = tt.scheduled, tt.unscheduled
			unscheduled, ok := schedulingReady(eng)
			if ok != tt.wantReady {
				t.Fatalf("ready = %v, want %v", ok, tt.wantReady)
			}
			if unscheduled != tt.unscheduled {
				t.Fatalf("unscheduled = %d, want %d", unscheduled, tt.unscheduled)
			}
		})
	}
	// A nil engine (DEV_MODE without a database) is never a readiness failure.
	if _, ok := schedulingReady(nil); !ok {
		t.Fatal("a nil engine must not fail readiness")
	}
}
