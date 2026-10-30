package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	monreg "github.com/Ariazonaa/statwarden/internal/monitor/registry"
	notifyreg "github.com/Ariazonaa/statwarden/internal/notify/registry"
)

// --- fake identity provider ---------------------------------------------------

// fakeIdP serves a discovery document and a token endpoint, so the callback can
// be driven end to end without a real Keycloak. It hands back whatever claims
// the test asks for, which is how the negative cases are built.
type fakeIdP struct {
	server *httptest.Server
	// claims is what the token endpoint mints an ID token from. Tests mutate it
	// between calls.
	claims map[string]any
	// omitIDToken drops the id_token from the token response.
	omitIDToken bool
	// issuerOverride, when set, is what the DISCOVERY document claims as its
	// issuer, so the issuer pin can be tested.
	issuerOverride string
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	idp := &fakeIdP{}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		issuer := idp.server.URL
		if idp.issuerOverride != "" {
			issuer = idp.issuerOverride
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"issuer":                 issuer,
			"authorization_endpoint": idp.server.URL + "/authorize",
			"token_endpoint":         idp.server.URL + "/token",
			"jwks_uri":               idp.server.URL + "/jwks",
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		if idp.omitIDToken {
			writeJSON(w, http.StatusOK, map[string]string{"access_token": "x"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"id_token": fakeIDToken(idp.claims)})
	})
	idp.server = httptest.NewServer(mux)
	t.Cleanup(idp.server.Close)
	return idp
}

// fakeIDToken builds an unsigned JWT. The server deliberately does not verify
// the signature — the token only ever arrives over the TLS back-channel from the
// pinned issuer — so an unsigned token is exactly what these tests should use.
func fakeIDToken(claims map[string]any) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	raw, _ := json.Marshal(claims)
	return header + "." + base64.RawURLEncoding.EncodeToString(raw) + ".signature"
}

// oidcHarness wires the API to a fake identity provider.
type oidcHarness struct {
	*harness
	idp      *fakeIdP
	provider *OIDCProvider
}

func newOIDCHarness(t *testing.T, tweak func(*OIDCProvider)) *oidcHarness {
	t.Helper()
	idp := newFakeIdP(t)
	store := NewMemStore()
	eng := newFakeEngine()
	mr := monreg.New()
	mr.Register(fakeChecker{})
	nr := notifyreg.New()
	nr.Register(fakeNotifier{})

	p := &OIDCProvider{
		ID:          "sso",
		DisplayName: "Test IdP",
		Issuer:      idp.server.URL,
		ClientID:    "statwarden-client",
		// The fake IdP is plain http on loopback, which is the DEV_MODE escape
		// hatch; a real deployment must use https.
		ClientSecret:        "shh",
		AllowInsecureIssuer: true,
		HTTPClient:          idp.server.Client(),
		DefaultRole:         "viewer",
	}
	if tweak != nil {
		tweak(p)
	}
	// A sane default claim set; individual tests override pieces of it.
	idp.claims = map[string]any{
		"iss": idp.server.URL, "sub": "external-subject-1", "aud": p.ClientID,
		"email": "sso-user@example.com", "email_verified": true, "name": "SSO User",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
	}

	h := New(Options{
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:           store,
		Engine:          eng,
		MonitorRegistry: mr,
		NotifyRegistry:  nr,
		OIDC:            []*OIDCProvider{p},
		PublicBaseURL:   "https://statwarden.example",
	})
	return &oidcHarness{
		harness:  &harness{t: t, handler: h, store: store, engine: eng},
		idp:      idp,
		provider: p,
	}
}

// startLogin performs the /start call and returns the flow cookie plus the
// state the server generated (read out of the authorize redirect).
func (h *oidcHarness) startLogin() (*http.Cookie, string) {
	h.t.Helper()
	rec := h.request(http.MethodGet, "/api/v1/auth/oidc/sso/start", nil)
	if rec.Code != http.StatusFound {
		h.t.Fatalf("start = %d, body %s", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		h.t.Fatalf("parse authorize URL: %v", err)
	}
	state := loc.Query().Get("state")
	if state == "" {
		h.t.Fatal("the authorize URL carries no state")
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == oidcCookie && c.Value != "" {
			return c, state
		}
	}
	h.t.Fatal("no flow cookie was set")
	return nil, ""
}

// callback drives the redirect back from the identity provider.
func (h *oidcHarness) callback(cookie *http.Cookie, state string, sess *session) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/auth/oidc/sso/callback?code=auth-code&state="+url.QueryEscape(state), nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if sess != nil {
		for _, c := range sess.cookies {
			req.AddCookie(c)
		}
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// nonceFrom decodes the nonce the server put in the flow cookie, so a test can
// make the identity provider echo the right one.
func nonceFrom(t *testing.T, cookie *http.Cookie) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	if err != nil {
		t.Fatalf("decode flow cookie: %v", err)
	}
	var f oidcFlow
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse flow cookie: %v", err)
	}
	return f.Nonce
}

// login runs a full flow with correct state and nonce.
func (h *oidcHarness) login(sess *session) *httptest.ResponseRecorder {
	h.t.Helper()
	cookie, state := h.startLogin()
	h.idp.claims["nonce"] = nonceFrom(h.t, cookie)
	return h.callback(cookie, state, sess)
}

func errorCodeOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect %q: %v", rec.Header().Get("Location"), err)
	}
	return loc.Query().Get("error")
}

func hasSessionCookie(rec *httptest.ResponseRecorder) bool {
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			return true
		}
	}
	return false
}

// --- S2: no auto-linking by e-mail --------------------------------------------

// TestOIDCRefusesToLinkByEmailWhenAutoProvisionIsOff is THE test for S2.
//
// A local account exists at sso-user@example.com. The identity provider asserts
// that exact address for a subject nobody has linked. The tempting behaviour —
// match the address and sign the caller into that account — is precisely the
// vulnerability: it would let a hostile or compromised provider take over any
// account by asserting its address. With auto-provisioning off (the default),
// nothing may happen at all.
func TestOIDCRefusesToLinkByEmailWhenAutoProvisionIsOff(t *testing.T) {
	h := newOIDCHarness(t, func(p *OIDCProvider) { p.AutoProvision = false })
	admin := h.setupAdmin()
	// A local account owning the address the IdP will assert.
	h.createUser(admin, "sso-user@example.com", "admin")
	usersBefore := h.userCount()

	rec := h.login(nil)

	if hasSessionCookie(rec) {
		t.Fatal("the callback issued a session for an unlinked subject; an account must never " +
			"be resolved from an IdP-asserted e-mail address")
	}
	if got := errorCodeOf(t, rec); got != oidcErrNoAccount {
		t.Errorf("error code = %q, want %q", got, oidcErrNoAccount)
	}
	if got := h.userCount(); got != usersBefore {
		t.Errorf("user count went from %d to %d; auto-provisioning is off, so nothing may be "+
			"created", usersBefore, got)
	}
	// Crucially, no identity was attached to the existing account either.
	ids, err := h.store.ListIdentitiesByUser(t.Context(), 2)
	if err != nil {
		t.Fatalf("list identities: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("the assertion was linked to the account owning the address (%+v); that is the "+
			"account-takeover path S2 forbids", ids)
	}
}

// TestOIDCAutoProvisionRefusesTakenAddress covers the same attack with
// auto-provisioning ON. Creating the account fails on the unique index, and the
// handler must refuse rather than fall back to attaching the identity to the
// account that already owns the address.
func TestOIDCAutoProvisionRefusesTakenAddress(t *testing.T) {
	h := newOIDCHarness(t, func(p *OIDCProvider) { p.AutoProvision = true })
	admin := h.setupAdmin()
	h.createUser(admin, "sso-user@example.com", "admin")
	usersBefore := h.userCount()

	rec := h.login(nil)

	if hasSessionCookie(rec) {
		t.Fatal("the callback signed the caller into the pre-existing account that merely shared " +
			"the asserted address")
	}
	if got := errorCodeOf(t, rec); got != oidcErrEmailTaken {
		t.Errorf("error code = %q, want %q", got, oidcErrEmailTaken)
	}
	if got := h.userCount(); got != usersBefore {
		t.Errorf("user count changed from %d to %d", usersBefore, got)
	}
	ids, _ := h.store.ListIdentitiesByUser(t.Context(), 2)
	if len(ids) != 0 {
		t.Errorf("an identity was linked to the existing account: %+v", ids)
	}
}

// TestOIDCAutoProvisionCreatesLeastPrivilegedAccount covers the opt-in path.
func TestOIDCAutoProvisionCreatesLeastPrivilegedAccount(t *testing.T) {
	h := newOIDCHarness(t, func(p *OIDCProvider) { p.AutoProvision = true })
	h.setupAdmin()

	rec := h.login(nil)
	if !hasSessionCookie(rec) {
		t.Fatalf("no session issued, error=%q", errorCodeOf(t, rec))
	}
	u, err := h.store.GetCredentialsByEmail(t.Context(), "sso-user@example.com")
	if err != nil {
		t.Fatalf("the provisioned account does not exist: %v", err)
	}
	if string(u.Role) != "viewer" {
		t.Errorf("provisioned role = %q, want viewer — a provisioned account gets the least "+
			"privileged role, not the writer role", u.Role)
	}
	// No local password: the empty hash is not a weak password, it is a value
	// auth.VerifyPassword rejects outright.
	if u.PasswordHash != "" {
		t.Errorf("provisioned account has a password hash %q, want empty", u.PasswordHash)
	}
	if _, code := h.harness.login("sso-user@example.com", "", ""); code == http.StatusOK {
		t.Error("a password login succeeded against a provisioned account with an empty hash")
	}
}

// TestOIDCAutoProvisionRequiresVerifiedEmail pins that an address the provider
// has not itself confirmed cannot become a local account.
func TestOIDCAutoProvisionRequiresVerifiedEmail(t *testing.T) {
	h := newOIDCHarness(t, func(p *OIDCProvider) { p.AutoProvision = true })
	h.setupAdmin()
	h.idp.claims["email_verified"] = false
	before := h.userCount()

	rec := h.login(nil)
	if hasSessionCookie(rec) {
		t.Error("an unverified address was provisioned into an account")
	}
	if got := h.userCount(); got != before {
		t.Errorf("user count changed from %d to %d", before, got)
	}
}

// TestOIDCAutoProvisionHonoursDomainAllowlist pins the provisioning filter.
func TestOIDCAutoProvisionHonoursDomainAllowlist(t *testing.T) {
	h := newOIDCHarness(t, func(p *OIDCProvider) {
		p.AutoProvision = true
		p.AllowedEmailDomains = []string{"allowed.example"}
	})
	h.setupAdmin()
	before := h.userCount()

	rec := h.login(nil) // claims say sso-user@example.com
	if hasSessionCookie(rec) {
		t.Error("an address outside the allowlist was provisioned")
	}
	if got := h.userCount(); got != before {
		t.Errorf("user count changed from %d to %d", before, got)
	}
}

// TestOIDCLoginUsesIssuerSubjectOnly pins the join key. Once an identity is
// linked, the flow resolves it by (issuer, subject) — and a CHANGED e-mail claim
// must not affect which account is signed in.
func TestOIDCLoginUsesIssuerSubjectOnly(t *testing.T) {
	h := newOIDCHarness(t, nil)
	admin := h.setupAdmin()
	target := h.createUser(admin, "target@example.com", "user")

	if _, err := h.store.CreateIdentity(t.Context(), ExternalIdentity{
		UserID: target.userID, Provider: "sso", Issuer: h.idp.server.URL, Subject: "external-subject-1",
	}); err != nil {
		t.Fatalf("seed identity: %v", err)
	}

	// The provider now asserts somebody else's address entirely. The subject is
	// what decides, so the session must still belong to `target`.
	h.idp.claims["email"] = "admin@example.com"
	rec := h.login(nil)
	if !hasSessionCookie(rec) {
		t.Fatalf("no session issued, error=%q", errorCodeOf(t, rec))
	}
	if got := h.sessionOwner(rec); got != target.userID {
		t.Errorf("signed in as user %d, want %d — the account must be resolved by "+
			"(issuer, subject), never by the asserted address", got, target.userID)
	}
}

// TestOIDCRejectsDeactivatedAccount pins the IsActive check on the callback.
func TestOIDCRejectsDeactivatedAccount(t *testing.T) {
	h := newOIDCHarness(t, nil)
	admin := h.setupAdmin()
	user := h.createUser(admin, "user@example.com", "user")
	if _, err := h.store.CreateIdentity(t.Context(), ExternalIdentity{
		UserID: user.userID, Provider: "sso", Issuer: h.idp.server.URL, Subject: "external-subject-1",
	}); err != nil {
		t.Fatalf("seed identity: %v", err)
	}
	inactive := false
	if rec := h.requestWith(http.MethodPut, "/api/v1/users/"+itoaTest(user.userID), updateUserRequest{
		Email: "user@example.com", Role: "user", IsActive: &inactive,
	}, admin); rec.Code != http.StatusOK {
		t.Fatalf("deactivate = %d, body %s", rec.Code, rec.Body.String())
	}

	rec := h.login(nil)
	if hasSessionCookie(rec) {
		t.Error("a deactivated account received a session")
	}
	if got := errorCodeOf(t, rec); got != oidcErrInactive {
		t.Errorf("error code = %q, want %q", got, oidcErrInactive)
	}
}

// --- flow integrity -----------------------------------------------------------

// TestOIDCCallbackRequiresFlowCookie is the login-CSRF defence. Without the
// state check an attacker can complete a flow in a victim's browser and silently
// sign them into the ATTACKER's account.
func TestOIDCCallbackRequiresFlowCookie(t *testing.T) {
	h := newOIDCHarness(t, nil)
	h.setupAdmin()

	rec := h.callback(nil, "some-state", nil)
	if hasSessionCookie(rec) {
		t.Fatal("a callback with no flow cookie issued a session")
	}
	if got := errorCodeOf(t, rec); got != oidcErrFailed {
		t.Errorf("error code = %q, want %q", got, oidcErrFailed)
	}
}

// TestOIDCCallbackRejectsMismatchedState covers the same defence with a cookie
// present but a state that does not match it.
func TestOIDCCallbackRejectsMismatchedState(t *testing.T) {
	h := newOIDCHarness(t, nil)
	h.setupAdmin()
	cookie, _ := h.startLogin()

	rec := h.callback(cookie, "a-state-the-server-never-issued", nil)
	if hasSessionCookie(rec) {
		t.Fatal("a callback with a forged state issued a session")
	}
	if got := errorCodeOf(t, rec); got != oidcErrFailed {
		t.Errorf("error code = %q, want %q", got, oidcErrFailed)
	}
}

// TestOIDCRejectsBadClaims walks the ID-token validation checklist. Each case
// must fail closed, with the same coarse code, and issue no session.
func TestOIDCRejectsBadClaims(t *testing.T) {
	tests := []struct {
		name  string
		mutth func(*oidcHarness, *http.Cookie)
	}{
		{"nonce mismatch", func(h *oidcHarness, _ *http.Cookie) {
			h.idp.claims["nonce"] = "a-nonce-from-another-flow"
		}},
		{"missing nonce", func(h *oidcHarness, _ *http.Cookie) {
			delete(h.idp.claims, "nonce")
		}},
		{"issuer mismatch", func(h *oidcHarness, _ *http.Cookie) {
			h.idp.claims["iss"] = "https://evil.example"
		}},
		{"audience is another client", func(h *oidcHarness, _ *http.Cookie) {
			h.idp.claims["aud"] = "some-other-client"
		}},
		{"multiple audiences without azp", func(h *oidcHarness, _ *http.Cookie) {
			h.idp.claims["aud"] = []string{"statwarden-client", "another-client"}
		}},
		{"expired", func(h *oidcHarness, _ *http.Cookie) {
			h.idp.claims["exp"] = time.Now().Add(-time.Hour).Unix()
		}},
		{"empty subject", func(h *oidcHarness, _ *http.Cookie) {
			h.idp.claims["sub"] = ""
		}},
		{"no id_token in the token response", func(h *oidcHarness, _ *http.Cookie) {
			h.idp.omitIDToken = true
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newOIDCHarness(t, func(p *OIDCProvider) { p.AutoProvision = true })
			h.setupAdmin()
			before := h.userCount()

			cookie, state := h.startLogin()
			h.idp.claims["nonce"] = nonceFrom(t, cookie)
			tc.mutth(h, cookie)
			rec := h.callback(cookie, state, nil)

			if hasSessionCookie(rec) {
				t.Fatalf("%s: a session was issued despite a rejected ID token", tc.name)
			}
			if got := errorCodeOf(t, rec); got != oidcErrFailed {
				t.Errorf("error code = %q, want %q — failures must be coarse and uniform so they "+
					"do not say which check rejected the token", got, oidcErrFailed)
			}
			if got := h.userCount(); got != before {
				t.Errorf("user count changed from %d to %d", before, got)
			}
		})
	}
}

// TestOIDCPinsDiscoveryIssuer pins the discovery document to the configured
// issuer. Without it a redirected or misconfigured provider could hand back
// somebody else's endpoints and every later check would target the wrong
// authority.
func TestOIDCPinsDiscoveryIssuer(t *testing.T) {
	h := newOIDCHarness(t, nil)
	h.setupAdmin()
	h.idp.issuerOverride = "https://someone-else.example"

	rec := h.request(http.MethodGet, "/api/v1/auth/oidc/sso/start", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("start = %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/login?error=") {
		t.Errorf("start redirected to %q; a discovery document whose issuer does not match the "+
			"configured one must be rejected", loc)
	}
}

// TestOIDCAuthorizeURLCarriesPKCEAndNonce pins the request parameters. PKCE must
// be S256 — never "plain", which offers no protection against an intercepted
// authorization code — and the redirect URI must be derived from the configured
// base URL rather than from any request header.
func TestOIDCAuthorizeURLCarriesPKCEAndNonce(t *testing.T) {
	h := newOIDCHarness(t, nil)
	h.setupAdmin()

	rec := h.request(http.MethodGet, "/api/v1/auth/oidc/sso/start", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("start = %d, body %s", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	q := loc.Query()
	if got := q.Get("response_type"); got != "code" {
		t.Errorf("response_type = %q, want \"code\" — no implicit or hybrid flow", got)
	}
	if got := q.Get("code_challenge_method"); got != "S256" {
		t.Errorf("code_challenge_method = %q, want S256", got)
	}
	if q.Get("code_challenge") == "" {
		t.Error("no code_challenge")
	}
	if q.Get("nonce") == "" {
		t.Error("no nonce; the nonce is the replay defence and is independent of state")
	}
	want := "https://statwarden.example/api/v1/auth/oidc/sso/callback"
	if got := q.Get("redirect_uri"); got != want {
		t.Errorf("redirect_uri = %q, want %q — it must come from PUBLIC_BASE_URL, never from a "+
			"request header", got, want)
	}
}

// TestOIDCRedirectURIIgnoresHostHeader makes the previous point directly: a
// forged Host must not move the redirect URI.
func TestOIDCRedirectURIIgnoresHostHeader(t *testing.T) {
	h := newOIDCHarness(t, nil)
	h.setupAdmin()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/sso/start", nil)
	req.Host = "evil.example"
	req.Header.Set("X-Forwarded-Host", "evil.example")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	redirect := loc.Query().Get("redirect_uri")
	if strings.Contains(redirect, "evil.example") {
		t.Fatalf("redirect_uri = %q followed the Host header; the authorization code is a "+
			"credential and must only ever be sent to the configured base URL", redirect)
	}
}

// TestOIDCFlowCookieIsHardened pins the transient-state cookie's properties.
func TestOIDCFlowCookieIsHardened(t *testing.T) {
	h := newOIDCHarness(t, nil)
	h.setupAdmin()
	cookie, _ := h.startLogin()

	if !cookie.HttpOnly {
		t.Error("the flow cookie is not HttpOnly; script must not be able to read the PKCE verifier")
	}
	// Lax is required, not merely chosen: the callback arrives as a top-level GET
	// navigation from the identity provider, which Strict would strip.
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", cookie.SameSite)
	}
	if cookie.Path != oidcCookiePath {
		t.Errorf("Path = %q, want %q", cookie.Path, oidcCookiePath)
	}
	if cookie.MaxAge <= 0 || time.Duration(cookie.MaxAge)*time.Second > oidcFlowTTL {
		t.Errorf("Max-Age = %d, want a short positive lifetime (<= %s); the session cookie's "+
			"month-long lifetime must not be reused for a five-minute flow", cookie.MaxAge, oidcFlowTTL)
	}
}

// TestOIDCFlowCookieIsClearedOnEveryOutcome pins that an abandoned or failed
// ceremony cannot be resumed.
func TestOIDCFlowCookieIsClearedOnEveryOutcome(t *testing.T) {
	h := newOIDCHarness(t, func(p *OIDCProvider) { p.AutoProvision = true })
	h.setupAdmin()

	for name, run := range map[string]func() *httptest.ResponseRecorder{
		"success": func() *httptest.ResponseRecorder { return h.login(nil) },
		"failure": func() *httptest.ResponseRecorder {
			cookie, _ := h.startLogin()
			return h.callback(cookie, "wrong-state", nil)
		},
	} {
		rec := run()
		var cleared bool
		for _, c := range rec.Result().Cookies() {
			if c.Name == oidcCookie && c.MaxAge < 0 {
				cleared = true
			}
		}
		if !cleared {
			t.Errorf("%s: the flow cookie was not cleared", name)
		}
	}
}

// --- linking ------------------------------------------------------------------

// TestOIDCLinkAttachesToCallerNotToAssertedEmail pins that link mode binds the
// identity to the AUTHENTICATED caller, whatever address the provider asserts.
func TestOIDCLinkAttachesToCallerNotToAssertedEmail(t *testing.T) {
	h := newOIDCHarness(t, nil)
	admin := h.setupAdmin()
	victim := h.createUser(admin, "victim@example.com", "admin")

	// The caller is the admin; the provider asserts the VICTIM's address.
	h.idp.claims["email"] = "victim@example.com"

	rec := h.requestWith(http.MethodPost, "/api/v1/me/oidc/sso/link-start", stepUp{Password: "supersecret"}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("link-start = %d, body %s", rec.Code, rec.Body.String())
	}
	cookie := flowCookieFrom(t, rec)
	state, nonce := flowStateNonce(t, cookie)
	h.idp.claims["nonce"] = nonce
	h.callback(cookie, state, admin)

	adminIDs, _ := h.store.ListIdentitiesByUser(t.Context(), admin.userID)
	victimIDs, _ := h.store.ListIdentitiesByUser(t.Context(), victim.userID)
	if len(adminIDs) != 1 {
		t.Errorf("the caller has %d identities, want 1", len(adminIDs))
	}
	if len(victimIDs) != 0 {
		t.Errorf("the identity was attached to the account owning the ASSERTED address (%+v); "+
			"link mode must bind to the caller", victimIDs)
	}
}

// TestOIDCLinkStartRequiresAuthAndCSRF pins that linking is a protected
// mutation. A GET redirect here would be a one-click link-injection, since a
// top-level navigation cannot carry the CSRF header.
func TestOIDCLinkStartRequiresAuthAndCSRF(t *testing.T) {
	h := newOIDCHarness(t, nil)
	admin := h.setupAdmin()

	if rec := h.request(http.MethodPost, "/api/v1/me/oidc/sso/link-start", stepUp{Password: "supersecret"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous link-start = %d, want 401", rec.Code)
	}
	stripped := &session{cookies: admin.cookies}
	if rec := h.requestWith(http.MethodPost, "/api/v1/me/oidc/sso/link-start", stepUp{Password: "supersecret"}, stripped); rec.Code != http.StatusForbidden {
		t.Errorf("link-start without CSRF = %d, want 403", rec.Code)
	}
}

// TestOIDCUnlinkIsScopedToCaller is the IDOR guard on unlinking.
func TestOIDCUnlinkIsScopedToCaller(t *testing.T) {
	h := newOIDCHarness(t, nil)
	admin := h.setupAdmin()
	victim := h.createUser(admin, "victim@example.com", "user")

	id, err := h.store.CreateIdentity(t.Context(), ExternalIdentity{
		UserID: victim.userID, Provider: "sso", Issuer: h.idp.server.URL, Subject: "victim-subject",
	})
	if err != nil {
		t.Fatalf("seed identity: %v", err)
	}

	rec := h.requestWith(http.MethodDelete,
		"/api/v1/me/oidc/identities/"+itoaTest(id.ID), nil, admin)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unlinking another user's identity = %d, want 404", rec.Code)
	}
	remaining, _ := h.store.ListIdentitiesByUser(t.Context(), victim.userID)
	if len(remaining) != 1 {
		t.Errorf("the victim's identity was removed by another user")
	}
}

// TestOIDCUnlinkRefusesLastIdentityWithoutPassword pins the lockout guard.
// Removing the only way a passwordless account can authenticate is an
// irrecoverable self-lockout the user cannot undo.
func TestOIDCUnlinkRefusesLastIdentityWithoutPassword(t *testing.T) {
	h := newOIDCHarness(t, func(p *OIDCProvider) { p.AutoProvision = true })
	h.setupAdmin()

	rec := h.login(nil)
	if !hasSessionCookie(rec) {
		t.Fatalf("provisioning failed: %q", errorCodeOf(t, rec))
	}
	sess := sessionFromRecorder(t, rec)
	// The redirect body carries no user id, so resolve it from the store.
	creds, err := h.store.GetCredentialsByEmail(t.Context(), "sso-user@example.com")
	if err != nil {
		t.Fatalf("provisioned user: %v", err)
	}
	sess.userID = creds.ID
	ids, _ := h.store.ListIdentitiesByUser(t.Context(), creds.ID)
	if len(ids) != 1 {
		t.Fatalf("provisioned user has %d identities, want 1", len(ids))
	}

	del := h.requestWith(http.MethodDelete,
		"/api/v1/me/oidc/identities/"+itoaTest(ids[0].ID), nil, sess)
	if del.Code != http.StatusConflict {
		t.Errorf("unlinking the last identity of a passwordless account = %d, want 409 — it "+
			"would leave the account with no way to authenticate at all, body %s",
			del.Code, del.Body.String())
	}
	if remaining, _ := h.store.ListIdentitiesByUser(t.Context(), creds.ID); len(remaining) != 1 {
		t.Error("the identity was removed despite the refusal")
	}
}

// --- surface ------------------------------------------------------------------

// TestOIDCProvidersListLeaksNoSecrets pins that the public provider list carries
// only what the login page needs to render a button.
func TestOIDCProvidersListLeaksNoSecrets(t *testing.T) {
	h := newOIDCHarness(t, nil)

	rec := h.request(http.MethodGet, "/api/v1/auth/oidc/providers", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("providers = %d, body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, secret := range []string{"shh", "statwarden-client", "client_secret", "clientSecret"} {
		if strings.Contains(body, secret) {
			t.Errorf("the provider list contains %q: %s", secret, body)
		}
	}
	var out []map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("listed %d providers, want 1", len(out))
	}
	for k := range out[0] {
		if k != "id" && k != "display_name" {
			t.Errorf("unexpected field %q in the public provider list", k)
		}
	}
}

// TestOIDCRoutesAbsentWhenUnconfigured pins that an instance with no SSO
// configuration has no SSO surface at all, rather than endpoints reporting 503.
func TestOIDCRoutesAbsentWhenUnconfigured(t *testing.T) {
	h := newHarness(t) // no OIDC option
	admin := h.setupAdmin()

	// The flow and identity endpoints could only ever report an error without a
	// configured provider, so they are not mounted at all.
	for _, tc := range []struct{ method, target string }{
		{http.MethodGet, "/api/v1/auth/oidc/sso/start"},
		{http.MethodGet, "/api/v1/me/oidc/identities"},
	} {
		rec := h.requestWith(tc.method, tc.target, nil, admin)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404 when SSO is not configured", tc.method, tc.target, rec.Code)
		}
	}
}

// TestOIDCProviderDiscoveryAlwaysAnswers pins that provider discovery stays
// mounted with SSO off. Its whole job is to answer "is single sign-on available
// here", and an empty list is the honest answer; a 404 cannot be told apart from
// a broken deployment. The login screen asks on every load, so returning 404
// logged a console error for every instance that does not use SSO.
func TestOIDCProviderDiscoveryAlwaysAnswers(t *testing.T) {
	h := newHarness(t) // no OIDC option

	rec := h.request(http.MethodGet, "/api/v1/auth/oidc/providers", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("providers = %d, want 200 even with SSO off", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("providers body = %s, want an empty array", got)
	}
}

// TestOIDCHandlersNeverResolveByEmail is a source-level guard for the S2 rule.
// The e-mail address on an assertion is display data; matching on it is the
// account-takeover path. A reviewer should be able to grep the OIDC handlers for
// the address lookup and find nothing, and this test keeps that true.
func TestOIDCHandlersNeverResolveByEmail(t *testing.T) {
	for _, file := range []string{"oidc.go", "oidc_handlers.go"} {
		raw, err := readSourceFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		// Match a CALL, not the identifier: these files discuss the rule in prose,
		// and a guard that fired on its own explanation would be useless.
		if strings.Contains(raw, "store.GetCredentialsByEmail(") {
			t.Errorf("%s calls GetCredentialsByEmail. Resolving a local account from an "+
				"IdP-asserted address lets a hostile or compromised provider take over any "+
				"account. (issuer, subject) is the only permitted join key.", file)
		}
	}
}

// --- helpers ------------------------------------------------------------------

func (h *oidcHarness) userCount() int {
	h.t.Helper()
	n, err := h.store.CountUsers(h.t.Context())
	if err != nil {
		h.t.Fatalf("count users: %v", err)
	}
	return n
}

// sessionOwner resolves which user the session cookie in rec belongs to.
func (h *oidcHarness) sessionOwner(rec *httptest.ResponseRecorder) int64 {
	h.t.Helper()
	sess := sessionFromRecorder(h.t, rec)
	me := h.requestWith(http.MethodGet, "/api/v1/me", nil, sess)
	if me.Code != http.StatusOK {
		h.t.Fatalf("GET /me = %d, body %s", me.Code, me.Body.String())
	}
	var u User
	mustJSON(h.t, me, &u)
	return u.ID
}

func flowCookieFrom(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == oidcCookie && c.Value != "" {
			return c
		}
	}
	t.Fatal("no flow cookie was set")
	return nil
}

func flowStateNonce(t *testing.T, cookie *http.Cookie) (state, nonce string) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	if err != nil {
		t.Fatalf("decode flow cookie: %v", err)
	}
	var f oidcFlow
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse flow cookie: %v", err)
	}
	return f.State, f.Nonce
}

func itoaTest(n int64) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}

// readSourceFile reads a file from this package's directory.
func readSourceFile(name string) (string, error) {
	raw, err := os.ReadFile(filepath.FromSlash(name))
	return string(raw), err
}

// --- flow-cookie integrity ----------------------------------------------------

// forgeFlowCookie rewrites the flow cookie. It stands in for an attacker
// editing it in their own browser or HTTP client: the cookie is HttpOnly, which
// keeps SCRIPT out, but its own holder can rewrite it freely.
func forgeFlowCookie(t *testing.T, c *http.Cookie, mutate func(*oidcFlow)) *http.Cookie {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		t.Fatalf("decode flow cookie: %v", err)
	}
	var f oidcFlow
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse flow cookie: %v", err)
	}
	mutate(&f)
	out, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal flow cookie: %v", err)
	}
	return &http.Cookie{Name: oidcCookie, Value: base64.RawURLEncoding.EncodeToString(out)}
}

// TestOIDCCallbackRejectsForgedLinkUserID is the account-takeover regression.
//
// The flow cookie is JSON + base64 and carries no signature, so whoever holds it
// can rewrite it. The exploit: start an ordinary login, add the victim's user id
// to the cookie as "u", complete the flow with your OWN identity-provider
// account. The callback then linked the attacker's external subject to the
// victim's local account, and an ordinary SSO login afterwards signed the
// attacker in as the victim.
func TestOIDCCallbackRejectsForgedLinkUserID(t *testing.T) {
	h := newOIDCHarness(t, nil)
	victim := h.setupAdmin() // the account worth stealing

	cookie, state := h.startLogin()
	h.idp.claims["nonce"] = nonceFrom(t, cookie)
	victimID := victim.userID
	forged := forgeFlowCookie(t, cookie, func(f *oidcFlow) { f.LinkUserID = &victimID })

	// The attacker sends no session: they hold none for the victim.
	rec := h.callback(forged, state, nil)

	ids, err := h.store.ListIdentitiesByUser(context.Background(), victimID)
	if err != nil {
		t.Fatalf("ListIdentitiesByUser: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("a forged flow cookie linked an external identity to user %d: %+v", victimID, ids)
	}
	if hasSessionCookie(rec) {
		t.Fatal("the forged link flow issued a session")
	}
}

// The same forgery carried out by someone who IS signed in, but as a different
// account: a caller may only ever link an identity to their own user.
func TestOIDCCallbackRejectsLinkToAnotherUser(t *testing.T) {
	h := newOIDCHarness(t, nil)
	admin := h.setupAdmin()
	rec := h.requestWith(http.MethodPost, "/api/v1/users", createUserRequest{
		Email: "attacker@example.com", Username: "attacker",
		Password: "supersecret", Role: "user",
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create attacker = %d, body %s", rec.Code, rec.Body.String())
	}
	attacker, code := h.harness.login("attacker@example.com", "supersecret", "")
	if attacker == nil {
		t.Fatalf("attacker login = %d", code)
	}

	cookie, state := h.startLogin()
	h.idp.claims["nonce"] = nonceFrom(t, cookie)
	victimID := admin.userID
	forged := forgeFlowCookie(t, cookie, func(f *oidcFlow) { f.LinkUserID = &victimID })

	h.callback(forged, state, attacker) // signed in, but as the attacker

	ids, err := h.store.ListIdentitiesByUser(context.Background(), victimID)
	if err != nil {
		t.Fatalf("ListIdentitiesByUser: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("a signed-in attacker linked an identity to user %d: %+v", victimID, ids)
	}
}

// The legitimate link flow must still work: the caller holds a session for the
// very user the flow names.
func TestOIDCLinkSucceedsForTheAuthenticatedUser(t *testing.T) {
	h := newOIDCHarness(t, nil)
	admin := h.setupAdmin()

	rec := h.requestWith(http.MethodPost, "/api/v1/me/oidc/sso/link-start", stepUp{Password: "supersecret"}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("link-start = %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		AuthorizeURL string `json:"authorize_url"`
	}
	mustJSON(t, rec, &body)
	loc, err := url.Parse(body.AuthorizeURL)
	if err != nil {
		t.Fatalf("parse authorize URL: %v", err)
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == oidcCookie && c.Value != "" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("link-start set no flow cookie")
	}
	h.idp.claims["nonce"] = nonceFrom(t, cookie)

	if got := h.callback(cookie, loc.Query().Get("state"), admin); got.Code != http.StatusFound {
		t.Fatalf("callback = %d, body %s", got.Code, got.Body.String())
	}
	ids, err := h.store.ListIdentitiesByUser(context.Background(), admin.userID)
	if err != nil {
		t.Fatalf("ListIdentitiesByUser: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("linked identities = %d, want 1", len(ids))
	}
}

// TestSignedFlowCookieRejectsTampering pins the SECOND, independent gate on the
// flow cookie: an HMAC keyed on SESSION_SECRET.
//
// The callback also checks the link user id against the caller's live session,
// so a forged id already fails there. This closes the same door from the other
// side — and it is what finally gives SESSION_SECRET a job: it was mandatory
// configuration that nothing outside the config parser ever read, so rotating
// it had no effect on anything at all.
func TestSignedFlowCookieRejectsTampering(t *testing.T) {
	h := newOIDCHarness(t, nil)
	h.handler = New(Options{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:         h.store,
		Engine:        h.engine,
		OIDC:          []*OIDCProvider{h.provider},
		PublicBaseURL: "https://statwarden.example",
		SessionSecret: "a-real-session-secret",
	})

	cookie, state := h.startLogin()
	if !strings.Contains(cookie.Value, ".") {
		t.Fatalf("flow cookie carries no signature: %q", cookie.Value)
	}

	// Flip one byte of the payload; the signature no longer matches.
	tampered := &http.Cookie{Name: oidcCookie, Value: "x" + cookie.Value[1:]}
	rec := h.callback(tampered, state, nil)
	if hasSessionCookie(rec) {
		t.Fatal("a tampered flow cookie completed the flow")
	}
	if code := errorCodeOf(t, rec); code != oidcErrFailed {
		t.Fatalf("error code = %q, want %q", code, oidcErrFailed)
	}

	// Stripping the signature entirely is refused too.
	unsigned := &http.Cookie{Name: oidcCookie, Value: strings.SplitN(cookie.Value, ".", 2)[0]}
	if rec := h.callback(unsigned, state, nil); hasSessionCookie(rec) {
		t.Fatal("an unsigned flow cookie completed the flow while a secret is configured")
	}
}

// Without a configured secret (DEV_MODE) the cookie is unsigned and still
// accepted, so local development without SESSION_SECRET keeps working.
func TestUnsignedFlowCookieStillWorksWithoutSecret(t *testing.T) {
	h := newOIDCHarness(t, nil) // no SessionSecret
	cookie, _ := h.startLogin()
	if strings.Contains(cookie.Value, ".") {
		t.Fatalf("cookie was signed without a configured secret: %q", cookie.Value)
	}
	if rec := h.login(nil); !hasSessionCookie(rec) && errorCodeOf(t, rec) == oidcErrFailed {
		t.Fatal("the unsigned flow was rejected even though no secret is configured")
	}
}

// Concurrent SSO starts share one *OIDCProvider, so the start path must hold no
// unsynchronised mutable state.
//
// scopeList used to memoise its result into p.scopesOK with no lock — p.mu guards
// only the discovery cache — and the only caller is authorizeURL, reached from
// startOIDCFlow on a request goroutine. Two people signing in at the same moment
// raced on the field. Every writer stored the same value, so the scope list was
// never wrong; the defect was the torn read, which surfaces as `go test -race`
// output or an implausible panic rather than as misbehaviour.
//
// This test is the seam that makes the race REACHABLE by the race detector. It
// passes either way without -race, which is precisely why it has to exist: the
// CI race job is the only thing that can see the difference, and it can only see
// it if something drives two starts at once.
func TestConcurrentOIDCStartsShareNoMutableState(t *testing.T) {
	h := newOIDCHarness(t, nil)

	const starts = 16
	var wg sync.WaitGroup
	codes := make([]int, starts)
	scopes := make([]string, starts)
	for i := range starts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := h.request(http.MethodGet, "/api/v1/auth/oidc/sso/start", nil)
			codes[i] = rec.Code
			if loc, err := url.Parse(rec.Header().Get("Location")); err == nil {
				scopes[i] = loc.Query().Get("scope")
			}
		}()
	}
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusFound {
			t.Fatalf("start %d = %d, want 302", i, code)
		}
	}
	// Every flow must have asked for the same scopes; a differing one would mean
	// the list was assembled from partially shared state.
	for i, s := range scopes {
		if s == "" {
			t.Fatalf("start %d produced no scope parameter", i)
		}
		if s != scopes[0] {
			t.Fatalf("start %d requested scope %q, start 0 requested %q — the scope "+
				"list is not built independently per flow", i, s, scopes[0])
		}
	}
	if !strings.Contains(scopes[0], "openid") {
		t.Fatalf("scope = %q, must always include openid", scopes[0])
	}
}
