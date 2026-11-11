package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestWebSocketStatusPageAccess pins the page check the API hands the hub for
// anonymous sockets: a client may follow exactly the status pages it could
// open over HTTP. Drafts, unknown slugs and password-protected pages were all
// subscribable, so a stranger could watch a protected page for changes.
//
// A visitor who has unlocked a protected page keeps live updates: the unlock
// also sets a page-named cookie scoped to /ws, because the main unlock cookie
// is scoped to the public API path and never reaches the handshake.
func TestWebSocketStatusPageAccess(t *testing.T) {
	hub := &recordingWSHub{}
	h := newHarnessTweaked(t, func(o *Options) { o.Hub = hub })
	admin := h.setupAdmin()
	pub, no := true, false
	h.createPage(admin, statusPageRequest{Slug: "open", Title: "Open", Published: &pub})
	h.createPage(admin, statusPageRequest{Slug: "draft", Title: "Draft", Published: &no})
	h.createPage(admin, statusPageRequest{Slug: "vault", Title: "Vault", Published: &pub, Password: "hunter2"})
	h.createPage(admin, statusPageRequest{Slug: "other", Title: "Other", Published: &pub, Password: "different"})

	handshake := func(cookies ...*http.Cookie) {
		t.Helper()
		*hub = recordingWSHub{}
		req := httptest.NewRequest(http.MethodGet, "/ws", nil)
		req.RemoteAddr = "198.51.100.20:5000"
		for _, c := range cookies {
			req.AddCookie(c)
		}
		h.handler.ServeHTTP(httptest.NewRecorder(), req)
		if !hub.served || hub.canSeePage == nil {
			t.Fatal("handshake did not reach the hub with a page check")
		}
	}

	handshake()
	if hub.auth != nil {
		t.Fatal("a cookieless handshake was treated as signed in")
	}
	if hub.peer != "198.51.100.20" {
		t.Errorf("peer = %q, want the resolved client address", hub.peer)
	}
	for slug, want := range map[string]bool{"open": true, "draft": false, "vault": false, "missing": false} {
		if got := hub.canSeePage(slug); got != want {
			t.Errorf("anonymous canSeePage(%q) = %v, want %v", slug, got, want)
		}
	}

	// Unlocking the page over HTTP lets the socket follow it, and only it.
	rec := h.pubReq(http.MethodGet, "/api/v1/public/status/vault", "hunter2", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("unlock = %d", rec.Code)
	}
	var wsCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == pageUnlockWSCookiePrefix+"vault" {
			wsCookie = c
		}
	}
	if wsCookie == nil {
		t.Fatalf("unlock set no WebSocket cookie; cookies: %v", rec.Result().Cookies())
	}
	if wsCookie.Path != "/ws" || !wsCookie.HttpOnly {
		t.Errorf("WebSocket unlock cookie Path=%q HttpOnly=%v, want /ws and true", wsCookie.Path, wsCookie.HttpOnly)
	}
	handshake(wsCookie)
	if !hub.canSeePage("vault") {
		t.Error("an unlocked visitor cannot follow the page they unlocked")
	}
	if hub.canSeePage("other") {
		t.Error("the unlock for one page opened another")
	}

	// A cookie for one page renamed onto another does not verify: the token
	// is bound to the page it was minted for.
	handshake(&http.Cookie{Name: pageUnlockWSCookiePrefix + "other", Value: wsCookie.Value})
	if hub.canSeePage("other") {
		t.Error("a token minted for vault unlocked other")
	}
}
