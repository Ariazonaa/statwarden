package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// rootOn requests "/" as a browser addressing the given host would.
func (h *harness) rootOn(host string) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = host
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// TestCustomDomainRootLandsOnThePage pins that a visitor typing a page's custom
// domain reaches that page. The root of every host served the dashboard SPA,
// which sent an anonymous visitor to the admin login instead of the status
// page the domain was set up for.
func TestCustomDomainRootLandsOnThePage(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	pub, no := true, false
	h.createPage(admin, statusPageRequest{
		Slug: "acme", Title: "Acme", Published: &pub, CustomDomain: "Status.Example.Test",
	})
	h.createPage(admin, statusPageRequest{
		Slug: "hidden", Title: "Hidden", Published: &no, CustomDomain: "draft.example.test",
	})

	for _, host := range []string{"status.example.test", "STATUS.example.test:8443"} {
		rec := h.rootOn(host)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/status/acme" {
			t.Errorf("GET / on %s = %d Location %q, want 302 to /status/acme",
				host, rec.Code, rec.Header().Get("Location"))
		}
	}

	// The instance's own host, an unknown host and a draft's domain keep the SPA.
	for _, host := range []string{"statwarden.example", "unknown.example.test", "draft.example.test"} {
		if rec := h.rootOn(host); rec.Code == http.StatusFound {
			t.Errorf("GET / on %s redirected to %q, want the SPA", host, rec.Header().Get("Location"))
		}
	}

	// Only the root is redirected; other paths on the domain are left alone.
	req := httptest.NewRequest(http.MethodGet, "/status/acme", nil)
	req.Host = "status.example.test"
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusFound {
		t.Errorf("GET /status/acme on the custom domain redirected to %q", rec.Header().Get("Location"))
	}
}

// TestCustomDomainIsValidatedOnWrite pins that custom_domain must be a bare
// hostname. It is matched against the Host header, so a URL, a port or a path
// can never match and silently left the page unreachable under its domain; and
// the instance's own hostname would hand the dashboard's root to one page.
func TestCustomDomainIsValidatedOnWrite(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	for _, bad := range []string{
		"https://status.example.test",
		"status.example.test/path",
		"status.example.test:443",
		"-status.example.test",
		"status..example.test",
		"status.example.test.",
		"stat us.example.test",
		"203.0.113.5",
		"statwarden.example", // the instance's own PUBLIC_BASE_URL host
	} {
		rec := h.requestWith(http.MethodPost, "/api/v1/status-pages",
			statusPageRequest{Slug: "bad", Title: "Bad", CustomDomain: bad}, admin)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("custom_domain %q = %d, want 400", bad, rec.Code)
		}
	}

	page := h.createPage(admin, statusPageRequest{Slug: "good", Title: "Good", CustomDomain: " Status.Example.Test "})
	if page.CustomDomain != "status.example.test" {
		t.Errorf("custom_domain = %q, want it trimmed and lower-cased", page.CustomDomain)
	}
	rec := h.requestWith(http.MethodPut, "/api/v1/status-pages/"+itoa64(page.ID),
		statusPageRequest{Slug: "good", Title: "Good", CustomDomain: "status.example.test:80"}, admin)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("update with a port = %d, want 400", rec.Code)
	}
	// Single-label intranet names are hostnames too.
	h.createPage(admin, statusPageRequest{Slug: "intranet", Title: "Intranet", CustomDomain: "status"})
}
