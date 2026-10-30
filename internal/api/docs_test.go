package api

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// TestAPIDocsServed asserts the reference page is served as HTML.
func TestAPIDocsServed(t *testing.T) {
	h := newHarness(t)
	rec := h.request(http.MethodGet, "/api/v1/docs", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/docs = %d, body %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "/api/v1/openapi.json") {
		t.Error("the page does not reference /api/v1/openapi.json; it would render nothing")
	}
}

// TestAPIDocsIsPublic pins the same decision the spec route makes: the
// documentation needs no credential, because an integrator reads it before any
// account exists.
func TestAPIDocsIsPublic(t *testing.T) {
	h := newHarness(t)
	if rec := h.request(http.MethodGet, "/api/v1/docs", nil); rec.Code != http.StatusOK {
		t.Fatalf("anonymous GET /api/v1/docs = %d, want 200", rec.Code)
	}
}

// TestAPIDocsHasNoExternalAssets is the guard on the property that makes this
// page work air-gapped. Statwarden ships as one binary with everything embedded; a
// CDN <script>, a Google Fonts <link> or a remote <img> added later would break
// offline installs in a way nothing else in the suite would notice, because the
// page still renders fine on a developer machine with internet access.
func TestAPIDocsHasNoExternalAssets(t *testing.T) {
	h := newHarness(t)
	body := h.request(http.MethodGet, "/api/v1/docs", nil).Body.String()

	// Any absolute URL in a fetchable position. The spec URL the page fetches is
	// relative, so there should be none at all.
	for _, pat := range []string{"http://", "https://", "//cdn", "//fonts"} {
		if strings.Contains(body, pat) {
			t.Errorf("the documentation page references an external asset (%q). "+
				"Everything must be inline or embedded: this binary is expected to "+
				"run air-gapped.", pat)
		}
	}
	for _, tag := range []string{"<link ", "<img ", "@import"} {
		if strings.Contains(body, tag) {
			t.Errorf("the page contains %q; external resource loads are not allowed", tag)
		}
	}
}

// TestAPIDocsCSP asserts the page ships its own strict policy with a nonce on
// both inline blocks. The global CSP is currently permissive enough that the
// page would work without this, which is exactly why it is worth pinning: when
// the baseline policy is tightened, this route must already be correct rather
// than silently going blank.
func TestAPIDocsCSP(t *testing.T) {
	h := newHarness(t)
	rec := h.request(http.MethodGet, "/api/v1/docs", nil)

	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("CSP = %q, want a default-src 'none' base", csp)
	}
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Errorf("CSP = %q, must not relax to unsafe-inline/unsafe-eval; the inline "+
			"blocks carry a nonce instead", csp)
	}
	if !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP = %q, lost the baseline frame-ancestors 'none'", csp)
	}

	nonce := regexp.MustCompile(`'nonce-([A-Za-z0-9+/]+)'`).FindStringSubmatch(csp)
	if nonce == nil {
		t.Fatalf("CSP = %q, carries no nonce", csp)
	}
	body := rec.Body.String()
	if strings.Contains(body, docsNoncePlaceholder) {
		t.Errorf("the %s placeholder survived into the response; the inline blocks "+
			"would be blocked by the policy above", docsNoncePlaceholder)
	}
	if got := strings.Count(body, `nonce="`+nonce[1]+`"`); got != 2 {
		t.Errorf("found %d nonced inline blocks, want 2 (one <style>, one <script>)", got)
	}
}

// TestAPIDocsNonceIsPerResponse guards the point of a nonce. A constant would
// be worth nothing: an injected script could simply carry it.
func TestAPIDocsNonceIsPerResponse(t *testing.T) {
	h := newHarness(t)
	first := h.request(http.MethodGet, "/api/v1/docs", nil).Header().Get("Content-Security-Policy")
	second := h.request(http.MethodGet, "/api/v1/docs", nil).Header().Get("Content-Security-Policy")
	if first == second {
		t.Error("two responses carry the same CSP nonce; it must be freshly generated per response")
	}
}
