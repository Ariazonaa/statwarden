package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/notify/secrets"
)

func TestWebhookSecretValidation(t *testing.T) {
	cases := []struct {
		value string
		ok    bool
		why   string
	}{
		{"", true, "empty switches signing off, and is the default"},
		{"whsec-0123456789abcdef", true, "the ordinary generated secret"},
		{strings.Repeat("a1", 128), true, "256 characters is the ceiling, inclusive"},
		{"short", false, "a placeholder, not a secret"},
		{strings.Repeat("a1", 129), false, "past the ceiling"},
		{"has a space in the middle!!", false, "spaces are a copy-paste accident waiting to happen"},
		{"tab\tinside-0123456789", false, "control characters never belong in a key"},
	}
	for _, c := range cases {
		err := CheckWebhookSecret(c.value)
		if c.ok && err != nil {
			t.Errorf("CheckWebhookSecret(%q) = %v, want accepted: %s", c.value, err, c.why)
		}
		if !c.ok && err == nil {
			t.Errorf("CheckWebhookSecret(%q) was accepted; %s", c.value, c.why)
		}
	}
}

// A signing secret is write-only: the list reports whether it is set and
// nothing more, and so does the response to the write itself.
func TestWebhookSecretsAreNeverReadBack(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	const secret = "whsec-0123456789abcdef"

	for _, key := range []string{SettingAuditWebhookSecret, SettingOutboundWebhookSecret} {
		rec := h.requestWith(http.MethodPut, "/api/v1/settings/"+key, secret, admin)
		if rec.Code != http.StatusOK {
			t.Fatalf("set %s = %d, body %s", key, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("the write response for %s echoes the secret: %s", key, rec.Body.String())
		}
	}

	rec := h.requestWith(http.MethodGet, "/api/v1/settings", nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("the settings list leaks a signing secret: %s", rec.Body.String())
	}
	var rows []Setting
	mustJSON(t, rec, &rows)
	seen := 0
	for _, r := range rows {
		if isSecretSetting(r.Key) {
			seen++
			if string(r.Value) != "true" {
				t.Errorf("%s reads back as %s, want true (set)", r.Key, r.Value)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("secret rows in the list = %d, want 2", seen)
	}

	// Clearing it reads back as "not set".
	h.requestWith(http.MethodPut, "/api/v1/settings/"+SettingAuditWebhookSecret, "", admin)
	rec = h.requestWith(http.MethodGet, "/api/v1/settings", nil, admin)
	mustJSON(t, rec, &rows)
	for _, r := range rows {
		if r.Key == SettingAuditWebhookSecret && string(r.Value) != "false" {
			t.Errorf("a cleared secret reads back as %s, want false", r.Value)
		}
	}
}

func TestSavingAWeakWebhookSecretIsRejected(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	rec := h.requestWith(http.MethodPut, "/api/v1/settings/"+SettingOutboundWebhookSecret, "short", admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("saving a 5-character secret = %d, want 400", rec.Code)
	}
}

// With a secrets box configured the row holds the sealed envelope, never the
// plaintext, and the box opens it back to exactly what was written.
func TestWebhookSecretIsSealedAtRest(t *testing.T) {
	box, err := secrets.New("test-key-for-webhook-secrets")
	if err != nil {
		t.Fatal(err)
	}
	h := newHarnessWithBox(t, box)
	admin := h.setupAdmin()
	const secret = "whsec-0123456789abcdef"

	rec := h.requestWith(http.MethodPut, "/api/v1/settings/"+SettingOutboundWebhookSecret, secret, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("set = %d, body %s", rec.Code, rec.Body.String())
	}
	st, err := h.store.GetSetting(context.Background(), SettingOutboundWebhookSecret)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(st.Value), secret) {
		t.Fatalf("the secret is stored in plaintext: %s", st.Value)
	}
	plain, err := box.DecryptConfig(st.Value)
	if err != nil {
		t.Fatalf("the stored value does not open: %v", err)
	}
	var got string
	if err := json.Unmarshal(plain, &got); err != nil || got != secret {
		t.Fatalf("opened value = %s (%v), want %q", plain, err, secret)
	}
}
