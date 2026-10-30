package api

import (
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
)

// docsPageRaw is the API reference UI, embedded so the single binary serves its
// own documentation.
//
// The page is deliberately hand-written rather than a vendored Swagger-UI or
// Redoc bundle. Statwarden ships as ONE static binary with its frontend embedded and
// is expected to run air-gapped, so pulling a renderer from a CDN is not an
// option, and vendoring one would add megabytes of third-party JavaScript — plus
// its supply chain and its CVE feed — to a binary whose whole point is that it
// has no runtime dependencies. This file is a few hundred lines and reads the
// same document those tools would.
//
// It contains no description of the API surface: it fetches
// /api/v1/openapi.json at runtime and renders whatever it finds. The reference
// therefore cannot drift from the spec, and openapi_contract_test.go already
// keeps the spec from drifting from the router.
//
//go:embed docs.html
var docsPageRaw string

// docsNoncePlaceholder is substituted with a per-response CSP nonce. It appears
// once on the <style> element and once on the <script> element.
const docsNoncePlaceholder = "__CSP_NONCE__"

// handleAPIDocs serves the interactive API reference.
//
// It sits at /api/v1/docs rather than a top-level /docs for two reasons. The
// document it renders is versioned — an Statwarden that one day serves /api/v2
// serves a second spec and needs a second reference beside it — and the SPA owns
// the top-level path space via its catch-all, so a top-level /docs would collide
// with a future client-side route. Living next to /api/v1/openapi.json also
// means the human-readable and machine-readable forms of the contract are found
// in the same place.
//
// Like the spec itself the page is public: it carries no instance data, and
// requiring a session to read the documentation would defeat the purpose for
// anyone integrating before they have an account. The "try it" console runs in
// the caller's browser with the caller's own cookie, so it grants no access the
// caller did not already have.
func (s *server) handleAPIDocs(w http.ResponseWriter, _ *http.Request) {
	nonce, err := docsNonce()
	if err != nil {
		// Failing closed is right: serving the page without a nonce under the
		// strict policy below would render a blank, silently broken document.
		writeProblem(w, http.StatusInternalServerError, "Could not render documentation")
		return
	}

	// This route sets its OWN Content-Security-Policy, replacing the baseline
	// frame-ancestors-only policy from securityHeaders (Set, not Add — a second
	// policy would be intersected with the first, not replace it).
	//
	// The baseline is loose because the SPA needs an inline pre-paint theme
	// script. This page does not: its two inline blocks carry the nonce, so it
	// can afford a policy that is strict today and, more to the point, stays
	// correct if the global CSP is ever tightened. default-src 'none' also
	// enforces the no-external-assets rule at the browser rather than on trust:
	// a stray CDN reference added here later would simply not load.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; "+
			"script-src 'nonce-"+nonce+"'; "+
			"style-src 'nonce-"+nonce+"'; "+
			"connect-src 'self'; "+
			"img-src 'self' data:; "+
			"form-action 'none'; "+
			"base-uri 'none'; "+
			"frame-ancestors 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// no-store, not no-cache: the body embeds a one-shot nonce, and a cached
	// copy replayed against a later response's policy would be blocked.
	w.Header().Set("Cache-Control", "no-store")

	_, _ = io.WriteString(w, strings.ReplaceAll(docsPageRaw, docsNoncePlaceholder, nonce))
}

// docsNonce returns a fresh base64 CSP nonce. 16 bytes is the amount the CSP
// specification asks for.
func docsNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(b[:]), nil
}
