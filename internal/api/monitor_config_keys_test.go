package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/domain"
)

// hostChecker is a checker whose target is "host", the shape the socket-family
// types (redis, postgres, mqtt, …) share. It decodes the way they do: plain
// encoding/json into a struct, which matches field names case-insensitively
// and under Unicode simple folding.
type hostChecker struct{}

func (hostChecker) Type() string      { return "hosty" }
func (hostChecker) Secrets() []string { return []string{"token"} }
func (hostChecker) Check(context.Context, json.RawMessage) (domain.CheckResult, error) {
	return domain.CheckResult{Status: domain.StatusUp}, nil
}
func (hostChecker) Validate(cfg json.RawMessage) error {
	var c struct {
		Host  string `json:"host"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		return err
	}
	if c.Host == "" {
		return errors.New("host is required")
	}
	return nil
}

// TestAFoldedTwinOfATargetKeyCannotCarryAMaskedCredentialAway
//
// The guard against "masked credential + new target" compared config keys byte
// for byte, while the checkers decode with encoding/json, which also accepts
// "hoſt" (U+017F) for the field "host". Sent next to an unchanged "host", the
// twin passed the comparison, sorted after the original when the config was
// re-serialised and so won the decode: the stored password went to a host the
// caller chose.
func TestAFoldedTwinOfATargetKeyCannotCarryAMaskedCredentialAway(t *testing.T) {
	h := newHarness(t)
	h.monReg.Register(hostChecker{})
	admin := h.setupAdmin()

	rec := h.requestWith(http.MethodPost, "/api/v1/monitors", monitorRequest{
		Name: "db", Type: "hosty", Interval: 60,
		Config: json.RawMessage(`{"host":"db.internal","token":"s3cr3t"}`),
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, body %s", rec.Code, rec.Body.String())
	}
	var created monitorView
	mustJSON(t, rec, &created)

	rec = h.requestWith(http.MethodPut, monitorPath(created.ID, ""), monitorRequest{
		Name: "db", Type: "hosty", Interval: 60,
		Config: json.RawMessage(`{"host":"db.internal","hoſt":"attacker.test","token":"` +
			monitorSecretPlaceholder + `"}`),
	}, admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("folded twin of host with a masked token = %d, want 400; body %s",
			rec.Code, rec.Body.String())
	}

	stored, err := h.store.GetMonitor(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("GetMonitor: %v", err)
	}
	if strings.Contains(string(stored.Config), "attacker.test") {
		t.Fatalf("the folded key was stored despite the refusal: %s", stored.Config)
	}
}

// A secret written under a differently-cased key is still read by the checker
// as that secret, but redaction looks the field up by its exact name and would
// hand it out unmasked on every read.
func TestASecretUnderAFoldedKeyIsRefusedRatherThanStoredUnmasked(t *testing.T) {
	h := newHarness(t)
	h.monReg.Register(hostChecker{})
	admin := h.setupAdmin()

	for _, key := range []string{"Token", "toKen"} {
		rec := h.requestWith(http.MethodPost, "/api/v1/monitors", monitorRequest{
			Name: "db", Type: "hosty", Interval: 60,
			Config: json.RawMessage(`{"host":"db.internal","` + key + `":"s3cr3t"}`),
		}, admin)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("config key %q = %d, want 400; body %s", key, rec.Code, rec.Body.String())
		}
	}
}

// Two keys that differ only by case name the same field to the decoder, so
// which one takes effect depends on serialisation order. That holds for any
// field, including a checker-specific target the shared list does not know.
func TestConfigKeysThatFoldTogetherAreRefused(t *testing.T) {
	h := newHarness(t)
	h.monReg.Register(hostChecker{})
	admin := h.setupAdmin()

	rec := h.requestWith(http.MethodPost, "/api/v1/monitors", monitorRequest{
		Name: "db", Type: "hosty", Interval: 60,
		Config: json.RawMessage(`{"host":"db.internal","note":"a","Note":"b"}`),
	}, admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("keys differing only by case = %d, want 400; body %s", rec.Code, rec.Body.String())
	}
}

// The canonical spelling keeps working, including keys no list mentions.
func TestCanonicalConfigKeysAreAccepted(t *testing.T) {
	h := newHarness(t)
	h.monReg.Register(hostChecker{})
	admin := h.setupAdmin()

	rec := h.requestWith(http.MethodPost, "/api/v1/monitors", monitorRequest{
		Name: "db", Type: "hosty", Interval: 60,
		Config: json.RawMessage(`{"host":"db.internal","token":"s3cr3t","note":"a"}`),
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("canonical keys = %d, want 201; body %s", rec.Code, rec.Body.String())
	}
}
