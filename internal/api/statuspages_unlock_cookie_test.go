package api

import (
	"net/http"
	"testing"
)

// TestPageUnlockCookieWorksInCrossSiteFrame pins the attributes that let the
// unlock cookie survive inside an iframe on another site, which the status
// pages explicitly support. A SameSite=Lax cookie is neither stored nor sent
// from a cross-site frame, so an embedded protected page asked for the
// password on every request. SameSite=None requires Secure, and Partitioned
// keys the cookie to the embedding site, so one embedder's unlock does not
// open the page inside another's frame.
func TestPageUnlockCookieWorksInCrossSiteFrame(t *testing.T) {
	h := newHarnessTweaked(t, func(o *Options) { o.SecureCookies = true })
	admin := h.setupAdmin()
	pub := true
	h.createPage(admin, statusPageRequest{Slug: "framed", Title: "Framed", Published: &pub, Password: "hunter2"})

	rec := h.pubReq(http.MethodGet, "/api/v1/public/status/framed", "hunter2", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("unlock = %d, body %s", rec.Code, rec.Body.String())
	}
	c := unlockCookie(t, rec)
	if c.SameSite != http.SameSiteNoneMode || !c.Secure || !c.Partitioned || !c.HttpOnly {
		t.Errorf("unlock cookie SameSite=%v Secure=%v Partitioned=%v HttpOnly=%v; want None/true/true/true",
			c.SameSite, c.Secure, c.Partitioned, c.HttpOnly)
	}
}

// TestPageUnlockCookieOverPlainHTTPStaysLax pins the other half: without
// secure cookies a browser rejects SameSite=None outright, which would break
// the unlock even for a visitor on the page itself.
func TestPageUnlockCookieOverPlainHTTPStaysLax(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	pub := true
	h.createPage(admin, statusPageRequest{Slug: "plain", Title: "Plain", Published: &pub, Password: "hunter2"})

	rec := h.pubReq(http.MethodGet, "/api/v1/public/status/plain", "hunter2", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("unlock = %d, body %s", rec.Code, rec.Body.String())
	}
	c := unlockCookie(t, rec)
	if c.SameSite != http.SameSiteLaxMode || c.Secure || c.Partitioned {
		t.Errorf("unlock cookie SameSite=%v Secure=%v Partitioned=%v; want Lax/false/false",
			c.SameSite, c.Secure, c.Partitioned)
	}
}
