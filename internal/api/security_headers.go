package api

import "net/http"

// baseCSP is the response Content-Security-Policy for the dashboard and API.
//
// It cannot set a script-src: the SPA relies on an inline pre-paint theme
// script in app.html and on SvelteKit's inline hydration bootstrap, and this
// build has no nonce/hash injection seam, so 'unsafe-inline' would be the only
// way to keep them working — which would defeat the point. The directives here
// are therefore the ones that harden without touching inline scripts and that
// close the non-script escalation paths a stored payload (e.g. a hostile
// monitor link_url) would otherwise reach for:
//
//   - object-src 'none'   — no <object>/<embed>/<applet> plugin execution.
//   - base-uri 'self'     — an injected <base> cannot rewrite every relative
//     URL (script/style/action) to an attacker origin.
//   - form-action 'self'  — an injected <form> cannot exfiltrate to a remote
//     endpoint.
//   - frame-ancestors 'none' — the existing clickjacking guard; the public
//     status pages relax only this one via
//     framableStatusPage.
//
// There is deliberately no default-src: it is the fallback for script-src, so
// setting it would block the inline scripts just as surely as a bare
// script-src would. The primary defence against a "javascript:" link_url stays
// server- and client-side validation; this policy is defence in depth.
// cspShared holds every directive that applies to EVERY response. It is split
// out so the framable variant below can relax frame-ancestors alone instead of
// replacing the whole policy — which is what it used to do, silently dropping
// object-src, base-uri and form-action on exactly the pages that render
// operator-supplied text. Adding a directive here now reaches both policies.
const cspShared = "object-src 'none'; base-uri 'self'; form-action 'self'"

const baseCSP = cspShared + "; frame-ancestors 'none'"

// framableCSP is baseCSP with only the clickjacking guard lifted.
const framableCSP = cspShared + "; frame-ancestors *"

// securityHeaders applies the baseline response hardening to every request. The
// public status-page route overrides the frame-ancestors directive via
// framableStatusPage.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", baseCSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}

// framableStatusPage keeps the public status pages iframe-able by anyone, as
// they were before any frame-ancestors header existed. Operators already embed
// /status/<slug> in intranet portals and wikis, so tightening it would be a
// breaking change; the dashboard and the API keep the global 'none' policy. It
// runs after securityHeaders and replaces the header (Set, not Add) -- adding a
// second policy would make browsers intersect the two and still deny framing.
// X-Frame-Options is deliberately not used: it has no "any origin" form beyond
// omitting it entirely.
//
// It replaces the header with framableCSP rather than with a bare
// "frame-ancestors *". Writing only that directive dropped object-src,
// base-uri and form-action from the response — on the one route that renders
// operator-supplied titles, descriptions and footer text to anonymous
// visitors, i.e. precisely where those directives earn their keep.
func framableStatusPage(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", framableCSP)
		next.ServeHTTP(w, r)
	})
}
