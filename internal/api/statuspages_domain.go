package api

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// normalizeCustomDomain is the form custom_domain is stored and compared in.
// toPage applies the same transformation, so validation sees what is written.
func normalizeCustomDomain(d string) string {
	return strings.ToLower(strings.TrimSpace(d))
}

// validateCustomDomain checks that a page's custom domain is a bare hostname.
//
// The column is matched against the request's Host header (minus any port),
// so anything else — a URL, a port, a path, a trailing dot — can never match
// and left the page silently unreachable under its domain. An IP literal is
// refused because a page reached by address is not a custom domain. The
// instance's own host is refused because the root of a page's domain now
// redirects to that page, which would hand the dashboard's front door to it.
func (s *server) validateCustomDomain(raw string) error {
	d := normalizeCustomDomain(raw)
	if d == "" {
		return nil
	}
	const msg = "custom_domain must be a bare hostname such as status.example.com (no scheme, port or path)"
	if len(d) > 253 || net.ParseIP(d) != nil {
		return errors.New(msg)
	}
	for _, label := range strings.Split(d, ".") {
		if !validHostLabel(label) {
			return errors.New(msg)
		}
	}
	if own := s.ownHost(); own != "" && d == own {
		return errors.New("custom_domain must not be this instance's own hostname")
	}
	return nil
}

// validHostLabel reports whether label is one DNS label: 1–63 letters, digits
// or hyphens, not starting or ending with a hyphen. Input is already
// lower-cased; IDNs are expected in their punycode (xn--) form.
func validHostLabel(label string) bool {
	if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// ownHost is the lower-cased hostname of PUBLIC_BASE_URL, or "" when none is
// configured.
func (s *server) ownHost() string {
	if s.baseURL == "" {
		return ""
	}
	u, err := url.Parse(s.baseURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// requestHostname is the request's Host header without its port, lower-cased.
func requestHostname(r *http.Request) string {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.ToLower(host)
}

// customDomainRoot sends a visitor arriving at the ROOT of a published page's
// custom domain to that page.
//
// The domain was configured so that people could type it, but "/" on every
// host served the dashboard SPA, so an anonymous visitor landed on the admin
// login instead of the status page. Only GET and HEAD of exactly "/" are
// redirected, and never on the instance's own host; every other path on the
// domain is served as before. The redirect is 302, not 301, because the
// mapping is an edit away from changing and a browser would cache a 301 for
// good.
func (s *server) customDomainRoot(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.store == nil || r.URL.Path != "/" || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
			next.ServeHTTP(w, r)
			return
		}
		host := requestHostname(r)
		if host == "" || host == s.ownHost() {
			next.ServeHTTP(w, r)
			return
		}
		p, err := s.store.GetStatusPageByDomain(r.Context(), host)
		if err != nil || !p.Published {
			next.ServeHTTP(w, r)
			return
		}
		http.Redirect(w, r, "/status/"+p.Slug, http.StatusFound)
	})
}
