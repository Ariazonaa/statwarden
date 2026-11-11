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

	// Exactly the two public status routes the SPA has — the page and its
	// availability archive — stay framable, with or without a trailing slash.
	for _, target := range []string{"/status/acme", "/status/acme/history", "/status/acme/", "/status/acme/history/"} {
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

// TestOnlyRealStatusRoutesAreFramable pins that the relaxed policy covers the
// public status routes and nothing else under /status/. The whole subtree used
// to be framable, and any path the SPA does not know renders its error page —
// whose button navigates into the dashboard inside the frame, client-side,
// without a new response and therefore without a new policy. A hostile embed
// could then frame the logged-in dashboard of whoever visited it.
func TestOnlyRealStatusRoutesAreFramable(t *testing.T) {
	h := newTestHandler(t)
	for _, target := range []string{
		"/status/",
		"/status",
		"/status/acme/settings",
		"/status/acme/history/extra",
		"/status/acme//history",
		"/status/Not_A_Slug!",
		"/status/" + strings.Repeat("a", 65),
	} {
		rec := do(t, h, http.MethodGet, target)
		if got := rec.Header().Get("Content-Security-Policy"); got != wantBaseCSP {
			t.Errorf("GET %s Content-Security-Policy = %q, want %q", target, got, wantBaseCSP)
		}
	}
}
