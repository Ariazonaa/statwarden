package ws

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// TestAnonymousStatusPageSubscriptionIsGated pins that an anonymous client may
// only follow a status page it could open over HTTP. Any well-formed slug used
// to be accepted, so a socket could follow a draft or a password-protected
// page and learn when, and what kind of, changes happened on it.
func TestAnonymousStatusPageSubscriptionIsGated(t *testing.T) {
	visible := map[string]bool{"open": true}
	canSee := func(slug string) bool { return visible[slug] }

	h := testHub()
	anon := newTestClient(h, false)
	anon.canSeePage = canSee
	if reason := h.subscribe(anon, "status-page:open"); reason != "" {
		t.Fatalf("anonymous subscribe to a visible page refused: %s", reason)
	}
	for _, slug := range []string{"draft", "protected", "nonexistent"} {
		if reason := h.subscribe(anon, "status-page:"+slug); reason == "" {
			t.Errorf("anonymous subscribe to %q was accepted", slug)
		}
		if h.topicSize("status-page:"+slug) != 0 {
			t.Errorf("refused topic %q still allocated a subscriber map", slug)
		}
	}

	// No check supplied fails closed: the hub cannot tell a draft from a page.
	blind := newTestClient(h, false)
	blind.canSeePage = nil
	if reason := h.subscribe(blind, "status-page:open"); reason == "" {
		t.Error("anonymous subscribe without a page check was accepted")
	}

	// A signed-in client sees drafts in the editor already, so it is not gated.
	authed := newTestClient(h, true)
	authed.canSeePage = canSee
	if reason := h.subscribe(authed, "status-page:draft"); reason != "" {
		t.Errorf("authenticated subscribe to a draft refused: %s", reason)
	}
}

// TestAnonymousConnectionsPerPeerAreCapped pins the per-address ceiling on
// anonymous sockets. /ws sits outside every HTTP rate limiter, and each
// connection costs goroutines and buffers, so one address could open sockets
// until the process ran out of memory.
func TestAnonymousConnectionsPerPeerAreCapped(t *testing.T) {
	h := testHub()
	h.maxAnonPerPeer = 2
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := func() bool { return true }
		if r.URL.Query().Get("anon") == "1" {
			auth = nil
		}
		h.ServeWS(w, r, r.URL.Query().Get("peer"), auth, allowAllPages)
	}))
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")
	dial := func(query string) (*websocket.Conn, int) {
		conn, resp, err := websocket.DefaultDialer.Dial(base+"/?"+query, nil)
		if err != nil {
			if resp == nil {
				t.Fatalf("dial %s: %v", query, err)
			}
			return nil, resp.StatusCode
		}
		return conn, http.StatusSwitchingProtocols
	}

	var open []*websocket.Conn
	defer func() {
		for _, c := range open {
			_ = c.Close()
		}
	}()
	for range 2 {
		c, code := dial("anon=1&peer=a")
		if c == nil {
			t.Fatalf("anonymous connection within the cap refused with %d", code)
		}
		open = append(open, c)
	}
	if c, code := dial("anon=1&peer=a"); c != nil || code != http.StatusTooManyRequests {
		t.Fatalf("third anonymous connection from one peer = %d, want 429", code)
	}
	// Another address, and a signed-in client from the same one, are unaffected.
	if c, code := dial("anon=1&peer=b"); c == nil {
		t.Fatalf("anonymous connection from another peer refused with %d", code)
	} else {
		open = append(open, c)
	}
	if c, code := dial("peer=a"); c == nil {
		t.Fatalf("authenticated connection refused with %d", code)
	} else {
		open = append(open, c)
	}

	// Closing one frees its slot.
	_ = open[0].Close()
	waitFor(t, func() bool {
		h.mu.RLock()
		defer h.mu.RUnlock()
		return h.anonByPeer["a"] < 2
	})
	if c, code := dial("anon=1&peer=a"); c == nil {
		t.Fatalf("anonymous connection after one closed refused with %d", code)
	} else {
		open = append(open, c)
	}
}
