package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/importexport"
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

// An MQTT broker URL can carry the broker login inline (the client library
// uses userinfo when present), and "broker" was handed out verbatim on every
// read. It gets the same password-only mask as "url".
//
// The mask must also survive the edit round trip: the form sends back what it
// read, and storing "tcp://ops:__redacted__@…" would replace the real password
// with the placeholder while the save looked successful.
func TestMQTTBrokerPasswordIsMaskedAndKept(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	const stored = `{"broker":"tcp://ops:hunter2@mq.example.test:1883","topic":"health"}`
	rec := h.requestWith(http.MethodPost, "/api/v1/monitors", monitorRequest{
		Name: "mq", Type: "fake", Interval: 60, Config: json.RawMessage(stored),
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, body %s", rec.Code, rec.Body.String())
	}
	var created monitorView
	mustJSON(t, rec, &created)

	get := h.requestWith(http.MethodGet, "/api/v1/monitors/"+itoa64(created.ID), nil, admin)
	var view monitorView
	mustJSON(t, get, &view)
	body := string(view.Config)
	if strings.Contains(body, "hunter2") {
		t.Fatalf("the broker password is handed out on read: %s", body)
	}
	if !strings.Contains(body, "ops:"+monitorSecretPlaceholder+"@mq.example.test") {
		t.Fatalf("the broker lost its identity or was not marked redacted: %s", body)
	}

	rec = h.requestWith(http.MethodPut, "/api/v1/monitors/"+itoa64(created.ID), monitorRequest{
		Name: "mq renamed", Type: "fake", Interval: 60, Config: view.Config,
	}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("save of the read config = %d, body %s", rec.Code, rec.Body.String())
	}
	m, err := h.store.GetMonitor(t.Context(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(m.Config), "ops:hunter2@mq.example.test") {
		t.Fatalf("the round trip replaced the stored broker password: %s", m.Config)
	}
}

// A masked broker is a placeholder, not a password: create refuses it like a
// placeholder secret, and an import drops it with a warning instead of storing
// the mask as the login.
func TestMaskedBrokerPasswordIsNeverStored(t *testing.T) {
	h := newImportHarness(t, nil)
	admin := h.setupAdmin()
	masked := `{"broker":"tcp://ops:` + monitorSecretPlaceholder + `@mq.example.test:1883"}`

	rec := h.requestWith(http.MethodPost, "/api/v1/monitors", monitorRequest{
		Name: "mq", Type: "fake", Interval: 60, Config: json.RawMessage(masked),
	}, admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create with a masked broker = %d, want 400; body %s", rec.Code, rec.Body.String())
	}

	rec = h.requestWith(http.MethodPost, "/api/v1/import", importexport.Bundle{
		Version: importexport.BundleVersion,
		Monitors: []importexport.MonitorSpec{{
			Name: "mq", Type: "fake", IntervalSeconds: 60, Active: true, Config: json.RawMessage(masked),
		}},
	}, admin)
	var res importResult
	mustJSON(t, rec, &res)
	if res.Monitors != 1 {
		t.Fatalf("import = %+v, want the monitor created", res)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "broker") {
		t.Fatalf("the dropped broker password is not reported: %v", res.Warnings)
	}
	mons, _, _ := h.store.ListMonitors(t.Context(), MonitorFilter{}, Page{Limit: 10})
	if len(mons) != 1 || strings.Contains(string(mons[0].Config), monitorSecretPlaceholder) {
		t.Fatalf("the mask was stored as the broker password: %+v", mons)
	}
	if !strings.Contains(string(mons[0].Config), "ops@mq.example.test") {
		t.Fatalf("the broker lost more than its password: %s", mons[0].Config)
	}
}
