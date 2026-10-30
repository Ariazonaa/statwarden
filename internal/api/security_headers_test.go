package api

import (
	"net/http"
	"strings"
	"testing"
)

// wantBaseCSP is the hardened default policy: the clickjacking guard plus the
// non-script escalation guards that back the link_url stored-XSS defence.
const wantBaseCSP = "object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'"

// wantFramableCSP is the public status page policy: the SAME hardening, with
// only the clickjacking guard lifted.
const wantFramableCSP = "object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors *"

func TestFrameAncestorsDefaultDeny(t *testing.T) {
	rec := do(t, newTestHandler(t), http.MethodGet, "/")
	if got := rec.Header().Get("Content-Security-Policy"); got != wantBaseCSP {
		t.Errorf("Content-Security-Policy = %q, want %q", got, wantBaseCSP)
	}
	// The clickjacking directive must survive inside the fuller policy.
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Errorf("Content-Security-Policy = %q, missing frame-ancestors 'none'", rec.Header().Get("Content-Security-Policy"))
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got, want := rec.Header().Get("Referrer-Policy"), "strict-origin-when-cross-origin"; got != want {
		t.Errorf("Referrer-Policy = %q, want %q", got, want)
	}
}

// TestPublicStatusPagesStayFramable pins both halves of the framing contract:
// the public status pages keep the pre-existing "anyone may iframe this"
// behaviour, and everything else -- dashboard, API, health -- does not.
func TestPublicStatusPagesStayFramable(t *testing.T) {
	h := newTestHandler(t)

	// The nested path pins that "/status/*" covers the whole SPA subtree, not
	// just the bare page: the framable middleware is on /status/*, so any
	// client-routed subpath under a status page stays framable inside an embed.
	for _, target := range []string{"/status/acme", "/status/acme/history"} {
		rec := do(t, h, http.MethodGet, target)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", target, rec.Code)
		}
		// Only frame-ancestors is relaxed. Replacing the whole policy with a
		// bare "frame-ancestors *" dropped object-src, base-uri and form-action
		// from the one route that renders operator-supplied text to anonymous
		// visitors — where those directives matter most.
		got := rec.Header().Get("Content-Security-Policy")
		if got != wantFramableCSP {
			t.Errorf("GET %s Content-Security-Policy = %q, want %q", target, got, wantFramableCSP)
		}
		for _, directive := range []string{"object-src 'none'", "base-uri 'self'", "form-action 'self'"} {
			if !strings.Contains(got, directive) {
				t.Errorf("GET %s lost %q when frame-ancestors was relaxed: %q", target, directive, got)
			}
		}
	}

	for _, target := range []string{"/", "/dashboard", "/api/v1/version", "/healthz"} {
		rec := do(t, h, http.MethodGet, target)
		if got := rec.Header().Get("Content-Security-Policy"); got != wantBaseCSP {
			t.Errorf("GET %s Content-Security-Policy = %q, want %q", target, got, wantBaseCSP)
		}
	}
}
