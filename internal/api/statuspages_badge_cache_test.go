package api

import (
	"net/http"
	"strings"
	"testing"
)

// TestProtectedPageBadgeIsNotPubliclyCacheable pins that the badge of a
// password-protected page is never marked cacheable by shared caches. It is
// only answered to a caller holding the password or the unlock cookie, so a
// "public" response let a CDN or corporate proxy hand the unlocked badge — the
// page's live status — to anybody behind it.
func TestProtectedPageBadgeIsNotPubliclyCacheable(t *testing.T) {
	h := newStatusHarness(t)
	admin := h.setupAdmin()
	pub := true
	h.createPage(admin, statusPageRequest{Slug: "open", Title: "Open", Published: &pub})
	h.createPage(admin, statusPageRequest{Slug: "locked", Title: "Locked", Published: &pub, Password: "hunter2"})

	rec := h.pubReq(http.MethodGet, "/api/v1/public/status/locked/badge.svg", "hunter2", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("protected badge = %d, body %s", rec.Code, rec.Body.String())
	}
	cc := rec.Header().Get("Cache-Control")
	if strings.Contains(cc, "public") || !strings.Contains(cc, "private") {
		t.Errorf("protected badge Cache-Control = %q, want private and never public", cc)
	}

	// The open page's badge keeps the shared-cache header it is embedded with.
	rec = h.pubReq(http.MethodGet, "/api/v1/public/status/open/badge.svg", "", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("open badge = %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "public") {
		t.Errorf("open badge Cache-Control = %q, want public", cc)
	}
}
