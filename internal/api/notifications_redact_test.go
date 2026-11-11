package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/notify/secrets"
)

// A channel written before at-rest encryption existed — or restored from an
// old backup — is stored as plaintext JSON. redactConfig only masked the
// sealed envelope and passed everything else through, so exactly those
// channels showed their bot tokens and SMTP passwords to every viewer.
func TestAPlaintextLegacyConfigIsMaskedLikeASealedOne(t *testing.T) {
	box, err := secrets.New("legacy-config-key")
	if err != nil {
		t.Fatal(err)
	}
	h := newHarnessWithBox(t, box)
	admin := h.setupAdmin()
	viewer := h.createUser(admin, "viewer@example.test", "viewer")

	const secret = "legacy-plaintext-token"
	legacy, err := h.store.CreateNotification(context.Background(), Notification{
		Name: "old", Type: "fake", Active: true,
		Config: json.RawMessage(`{"token":"` + secret + `"}`),
	})
	if err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	for _, target := range []string{
		"/api/v1/notifications",
		fmt.Sprintf("/api/v1/notifications/%d", legacy.ID),
	} {
		rec := h.requestWith(http.MethodGet, target, nil, viewer)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", target, rec.Code)
		}
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("GET %s shows the plaintext config to a viewer: %s", target, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"$encrypted":true`) {
			t.Errorf("GET %s does not show the redaction marker: %s", target, rec.Body.String())
		}
	}

	// Masked is not lost: a rename that leaves the config off keeps it, and the
	// write seals it on the way back in.
	rec := h.requestWith(http.MethodPut, fmt.Sprintf("/api/v1/notifications/%d", legacy.ID),
		map[string]any{"name": "renamed", "type": "fake"}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename = %d, body %s", rec.Code, rec.Body.String())
	}
	stored, err := h.store.GetNotification(context.Background(), legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !secrets.IsEncrypted(stored.Config) {
		t.Fatalf("the rename left the config unsealed: %s", stored.Config)
	}
	plain, err := box.DecryptConfig(stored.Config)
	if err != nil || !strings.Contains(string(plain), secret) {
		t.Fatalf("the rename lost the config: %s (%v)", plain, err)
	}
}
