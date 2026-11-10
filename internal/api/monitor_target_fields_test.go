package api

import (
	"encoding/json"
	"testing"
)

// A database check reports what its query returned, so the query decides what
// the stored credential is used to READ. Changing it while keeping a masked
// password is the same move as changing the host: someone who may edit the
// monitor but not read its password gets the server to run their statement with
// it and reports the result back through the check message.
func TestAMaskedCredentialCannotFollowAChangedQuery(t *testing.T) {
	h := newHarness(t)
	h.monReg.Register(secretChecker{})

	stored := json.RawMessage(`{"url":"postgres://db.example.test/app","token":"s3cr3t","query":"SELECT 1"}`)
	write := json.RawMessage(`{"url":"postgres://db.example.test/app","token":"` + monitorSecretPlaceholder +
		`","query":"SELECT password_hash FROM users"}`)
	if _, ok := h.srv.keepRedactedSecrets("secretty", write, stored); ok {
		t.Fatal("a masked credential was carried over to a different query")
	}

	// The same query with the mask is an ordinary edit and keeps the secret.
	same := json.RawMessage(`{"url":"postgres://db.example.test/app","token":"` + monitorSecretPlaceholder +
		`","query":"SELECT 1"}`)
	got, ok := h.srv.keepRedactedSecrets("secretty", same, stored)
	if !ok {
		t.Fatal("an unchanged query refused the masked credential")
	}
	var m map[string]string
	if err := json.Unmarshal(got, &m); err != nil || m["token"] != "s3cr3t" {
		t.Fatalf("restored config = %s, want the stored token back", got)
	}
}
