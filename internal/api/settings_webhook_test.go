package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The two webhook URL settings are the only pair in settingValidators that both
// gates a value AND hands it to something that makes a network request with it.
// Neither the validator nor its read-path clamp had a test.
func TestWebhookURLValidation(t *testing.T) {
	cases := []struct {
		value string
		ok    bool
		why   string
	}{
		{"", true, "empty is the disabled state, and the default one"},
		{"https://collector.example/ingest", true, "the ordinary case"},
		{"http://collector:8080/ingest", true, "plain http to a container on the same network"},
		{"https://user:pass@collector.example/x", true, "credentials in the URL are the operator's business"},
		{"file:///etc/passwd", false, "net/http cannot send this, so saving it is a silent no-op"},
		{"ftp://example.com", false, "not a scheme net/http speaks"},
		{"collector.example/ingest", false, "a bare hostname parses as a PATH, and would never be reached"},
		{"https://", false, "no host to send to"},
		{"://nonsense", false, "not a URL at all"},
	}
	for _, c := range cases {
		err := CheckWebhookURL(c.value)
		if c.ok && err != nil {
			t.Errorf("CheckWebhookURL(%q) = %v, want accepted: %s", c.value, err, c.why)
		}
		if !c.ok && err == nil {
			t.Errorf("CheckWebhookURL(%q) was accepted; %s", c.value, c.why)
		}
	}
}

// The write path rejects a non-string outright rather than coercing it.
func TestWebhookURLValidatorRejectsANonString(t *testing.T) {
	if err := validateWebhookURL(json.RawMessage(`42`)); err == nil {
		t.Error("a number was accepted as a webhook URL")
	}
	if err := validateWebhookURL(json.RawMessage(`""`)); err != nil {
		t.Errorf("an empty string was rejected: %v", err)
	}
}

// End to end: the settings endpoint says WHY, rather than storing a value that
// can never work and going quiet.
func TestSavingABrokenWebhookURLIsRejected(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	for _, key := range []string{SettingAuditWebhookURL, SettingOutboundWebhookURL} {
		// The body IS the value: the endpoint takes a bare JSON document, not an
		// envelope.
		rec := h.requestWith(http.MethodPut, "/api/v1/settings/"+key, "not-a-url", admin)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %s with a bare hostname = %d, want 400: body %s", key, rec.Code, rec.Body.String())
		}
		if !strings.Contains(strings.ToLower(rec.Body.String()), "http") {
			t.Errorf("the error for %s does not say what a valid value looks like: %s", key, rec.Body.String())
		}
		rec = h.requestWith(http.MethodPut, "/api/v1/settings/"+key,
			"https://collector.example/ingest", admin)
		if rec.Code != http.StatusOK {
			t.Errorf("PUT %s with a good URL = %d, body %s", key, rec.Code, rec.Body.String())
		}
	}
}

// The heartbeat-retention setting has to be READABLE, not just writable.
//
// It was validated on write, stored, mapped by the client and rendered as a
// pick-list whose help text promises an irreversible delete — and nothing read
// it back. retention.Options.DaysFunc, the hook built for exactly this, was set
// by no caller, so an admin's choice did nothing in either direction: raising it
// did not preserve history, lowering it did not reclaim disk.
func TestRuntimeRetentionDaysReadsWhatTheSettingsScreenWrote(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	ctx := context.Background()

	// Absent: no override, so the pruner keeps its env default.
	if got, err := RuntimeRetentionDays(ctx, h.store); err != nil || got != 0 {
		t.Errorf("with no row, RuntimeRetentionDays = %d, want 0 (no override)", got)
	}

	rec := h.requestWith(http.MethodPut, "/api/v1/settings/retention_days", 45, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT retention_days = %d, body %s", rec.Code, rec.Body.String())
	}
	if got, err := RuntimeRetentionDays(ctx, h.store); err != nil || got != 45 {
		t.Errorf("RuntimeRetentionDays = %d after the operator chose 45; the control is inert", got)
	}

	// A value written outside the API (psql) that is out of bounds is refused,
	// not clamped into a destructive one.
	if _, err := h.store.SetSetting(ctx, "retention_days", json.RawMessage(`0`)); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if got, err := RuntimeRetentionDays(ctx, h.store); err != nil || got != 0 {
		t.Errorf("an out-of-bounds stored value yielded %d; it must read as no override", got)
	}
	if _, err := h.store.SetSetting(ctx, "retention_days", json.RawMessage(`"ninety"`)); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if got, err := RuntimeRetentionDays(ctx, h.store); err != nil || got != 0 {
		t.Errorf("a non-numeric stored value yielded %d, want 0", got)
	}
}
