package api

import (
	"net/http"
	"testing"
)

// Until the first account exists, POST /api/v1/setup makes whoever calls it an
// admin. An instance is reachable from the moment it starts, so without a
// secret the operator raced every scanner on the network for their own server.
// The token is printed in the startup log, which only the operator can read.
func TestSetupRequiresTheSetupToken(t *testing.T) {
	cases := []struct {
		name  string
		token string
	}{
		{"missing", ""},
		{"wrong", "not-the-token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			rec := h.request(http.MethodPost, "/api/v1/setup", setupRequest{
				Email: "admin@example.com", Password: "supersecret", SetupToken: tc.token,
			})
			if rec.Code != http.StatusForbidden {
				t.Fatalf("setup with a %s token = %d, want 403; body %s", tc.name, rec.Code, rec.Body.String())
			}
			n, err := h.store.CountUsers(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Fatalf("a refused setup still created %d user(s)", n)
			}
		})
	}
}

func TestSetupWithTheRightTokenCreatesTheAdmin(t *testing.T) {
	h := newHarness(t)
	rec := h.request(http.MethodPost, "/api/v1/setup", setupRequest{
		Email: "admin@example.com", Password: "supersecret", SetupToken: testSetupToken,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup with the token = %d, want 201; body %s", rec.Code, rec.Body.String())
	}
}

// No token configured must not mean "no token required": that is the open
// door this exists to close, reached by a wiring mistake instead of a choice.
func TestSetupIsRefusedWhenNoTokenIsConfigured(t *testing.T) {
	h := newHarnessTweaked(t, func(o *Options) { o.SetupToken = "" })
	rec := h.request(http.MethodPost, "/api/v1/setup", setupRequest{
		Email: "admin@example.com", Password: "supersecret",
	})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("setup with no token configured = %d, want 503; body %s", rec.Code, rec.Body.String())
	}
}
