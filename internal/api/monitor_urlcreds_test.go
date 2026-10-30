package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestMonitorURLPasswordIsMaskedOnRead covers the read half of the url-credential
// finding.
//
// The write half lives in httpcheck's validator, which now refuses userinfo
// outright — but that only stops NEW rows. A monitor configured before the rule
// existed still holds "https://user:pass@host" in its config, and url is not a
// secret field (it is the monitor's identity on screen, so it cannot be blanked
// the way proxy_url is). It leaves by two doors: config.url and the derived
// view.url. Both are covered here.
//
// The harness registers a checker that validates nothing, which is precisely how
// a legacy row gets written in a test.
func TestMonitorURLPasswordIsMaskedOnRead(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	create := monitorRequest{
		Name: "legacy", Type: "fake", Interval: 60,
		Config: json.RawMessage(`{"url":"https://ops:hunter2@api.example.test/health?k=v"}`),
	}
	rec := h.requestWith(http.MethodPost, "/api/v1/monitors", create, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, body %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID int64 `json:"id"`
	}
	mustJSON(t, rec, &created)

	assertMasked := func(t *testing.T, what string, body string) {
		t.Helper()
		if strings.Contains(body, "hunter2") {
			t.Fatalf("%s handed out the password in cleartext:\n%s", what, body)
		}
		if !strings.Contains(body, monitorSecretPlaceholder) {
			t.Fatalf("%s did not mark the password as redacted:\n%s", what, body)
		}
		// Masking the secret must not cost the reader the target's identity —
		// that is the whole reason url is not simply blanked.
		for _, keep := range []string{"api.example.test", "/health", "ops"} {
			if !strings.Contains(body, keep) {
				t.Fatalf("%s lost %q from the target:\n%s", what, keep, body)
			}
		}
	}

	get := h.requestWith(http.MethodGet, "/api/v1/monitors/"+itoa64(created.ID), nil, admin)
	if get.Code != http.StatusOK {
		t.Fatalf("get = %d, body %s", get.Code, get.Body.String())
	}
	assertMasked(t, "GET /monitors/{id}", get.Body.String())

	list := h.requestWith(http.MethodGet, "/api/v1/monitors", nil, admin)
	if list.Code != http.StatusOK {
		t.Fatalf("list = %d, body %s", list.Code, list.Body.String())
	}
	assertMasked(t, "GET /monitors", list.Body.String())

	// The derived view.url is the second door, and it is populated from the
	// plaintext config before the config itself is redacted.
	var view struct {
		URL    string          `json:"url"`
		Config json.RawMessage `json:"config"`
	}
	mustJSON(t, get, &view)
	if strings.Contains(view.URL, "hunter2") {
		t.Fatalf("view.url leaked the password: %q", view.URL)
	}
	if !strings.Contains(view.URL, monitorSecretPlaceholder) {
		t.Fatalf("view.url was not masked: %q", view.URL)
	}
}

// TestMonitorURLWithoutCredentialsIsUntouched pins that the masking is inert on
// the overwhelmingly common case. A config that never carried a credential must
// come back byte-for-byte, or every monitor's config would be rewritten on every
// read.
func TestMonitorURLWithoutCredentialsIsUntouched(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	const plain = `{"url":"https://api.example.test/health","method":"GET"}`
	create := monitorRequest{Name: "plain", Type: "fake", Interval: 60, Config: json.RawMessage(plain)}
	rec := h.requestWith(http.MethodPost, "/api/v1/monitors", create, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, body %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID int64 `json:"id"`
	}
	mustJSON(t, rec, &created)

	get := h.requestWith(http.MethodGet, "/api/v1/monitors/"+itoa64(created.ID), nil, admin)
	var view struct {
		URL    string          `json:"url"`
		Config json.RawMessage `json:"config"`
	}
	mustJSON(t, get, &view)

	if view.URL != "https://api.example.test/health" {
		t.Fatalf("view.url = %q, want it unchanged", view.URL)
	}
	if strings.Contains(string(view.Config), monitorSecretPlaceholder) {
		t.Fatalf("a credential-free config was redacted anyway: %s", view.Config)
	}
}

// TestMaskURLPassword pins the helper directly, including the shapes the read
// path must not mangle.
func TestMaskURLPassword(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"password masked", "https://ops:hunter2@x.test/p", "https://ops:" + monitorSecretPlaceholder + "@x.test/p"},
		{"no userinfo", "https://x.test/p?a=1", "https://x.test/p?a=1"},
		{"username only", "https://ops@x.test/p", "https://ops@x.test/p"},
		{"empty password is still a password", "https://ops:@x.test/p", "https://ops:" + monitorSecretPlaceholder + "@x.test/p"},
		{"not a url", "://nonsense", "://nonsense"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := maskURLPassword(tc.in); got != tc.want {
				t.Fatalf("maskURLPassword(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
