package ws

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestValidTopicSlugShape pins that the status-page family — the only one an
// anonymous client may subscribe to — is checked for slug shape and not merely
// for being non-empty. An arbitrary suffix used to be accepted, and each
// distinct one allocates a subscriber map in the hub.
func TestValidTopicSlugShape(t *testing.T) {
	cases := []struct {
		topic string
		want  bool
	}{
		{"status-page:acme", true},
		{"status-page:a", true},
		{"status-page:my_page-2", true},
		{"status-page:" + strings.Repeat("a", 64), true},

		{"status-page:", false},
		{"status-page:" + strings.Repeat("a", 65), false},
		{"status-page:Acme", false}, // uppercase is rejected, not folded
		{"status-page:a/b", false},  // would widen the unlock cookie's Path
		{"status-page:a b", false},
		{"status-page:../etc", false},

		// The other families are unchanged.
		{TopicDashboard, true},
		{"monitor:7", true},
		{"monitor:x", false},
		{"bogus:1", false},
	}
	for _, c := range cases {
		if got := validTopic(c.topic); got != c.want {
			t.Errorf("validTopic(%q) = %v, want %v", c.topic, got, c.want)
		}
	}
}

// newTestClient builds a client for the subscription-bookkeeping tests.
// hub.subscribe touches neither the connection nor the send channel, so a bare
// value exercises the real path without a socket.
func newTestClient(h *Hub, authenticated bool) *client {
	c := &client{hub: h, subs: map[string]struct{}{}}
	c.authenticated.Store(authenticated)
	return c
}

// TestSubscriptionCap pins the per-client ceiling. Without it one socket could
// hold unlimited topics, growing both c.subs and Hub.topics until the process
// died.
func TestSubscriptionCap(t *testing.T) {
	h := testHub()
	c := newTestClient(h, true)

	for i := range maxSubscriptions {
		if reason := h.subscribe(c, "monitor:"+strconv.Itoa(i)); reason != "" {
			t.Fatalf("subscribe %d rejected: %s", i, reason)
		}
	}
	if reason := h.subscribe(c, "monitor:9999"); reason == "" {
		t.Fatalf("subscribe past the cap was accepted, want refusal")
	}
	if got := len(c.subs); got != maxSubscriptions {
		t.Fatalf("held topics = %d, want %d", got, maxSubscriptions)
	}
	if h.topicSize("monitor:9999") != 0 {
		t.Fatalf("refused topic still allocated a subscriber map")
	}

	// A topic already held costs nothing, so a client at the ceiling can still
	// re-affirm what it has — the cap guards growth, not repetition.
	if reason := h.subscribe(c, "monitor:0"); reason != "" {
		t.Fatalf("re-subscribe to a held topic rejected: %s", reason)
	}

	// Releasing one frees exactly one slot.
	h.unsubscribe(c, "monitor:0")
	if reason := h.subscribe(c, "monitor:9999"); reason != "" {
		t.Fatalf("subscribe after unsubscribe rejected: %s", reason)
	}
}

// TestAnonymousTopicGrowthIsBounded is the finding's actual attack: an
// unauthenticated socket looping subscribe with a fresh status-page slug each
// time. Both bounds are in play — the slug shape caps each name's length, the
// per-client cap caps how many the hub will allocate.
func TestAnonymousTopicGrowthIsBounded(t *testing.T) {
	h := testHub()
	c := newTestClient(h, false)

	refused := 0
	for i := range maxSubscriptions * 4 {
		if reason := h.subscribe(c, "status-page:page-"+strconv.Itoa(i)); reason != "" {
			refused++
		}
	}
	if refused == 0 {
		t.Fatal("anonymous client minted every topic it asked for")
	}
	h.mu.RLock()
	topics := len(h.topics)
	h.mu.RUnlock()
	if topics > maxSubscriptions {
		t.Fatalf("hub allocated %d topics for one anonymous client, cap is %d", topics, maxSubscriptions)
	}

	// An over-long slug is refused before it can allocate anything at all.
	if reason := h.subscribe(c, "status-page:"+strings.Repeat("x", 5000)); reason == "" {
		t.Fatal("over-long status-page slug was accepted")
	}
}

// TestMsgBudget drives the inbound token bucket on an explicit clock, so it
// pins the refill arithmetic rather than a sleep.
func TestMsgBudget(t *testing.T) {
	var b msgBudget
	now := time.Unix(0, 0)

	for i := range msgBurst {
		if !b.allow(now) {
			t.Fatalf("message %d of the burst was refused", i)
		}
	}
	if b.allow(now) {
		t.Fatal("message past the burst was allowed")
	}

	// Half a second refills msgRefillPerSec/2 messages, and no more.
	now = now.Add(500 * time.Millisecond)
	for i := range msgRefillPerSec / 2 {
		if !b.allow(now) {
			t.Fatalf("refilled message %d was refused", i)
		}
	}
	if b.allow(now) {
		t.Fatal("refill exceeded the elapsed budget")
	}

	// Idling does not bank more than one burst.
	now = now.Add(time.Hour)
	for i := range msgBurst {
		if !b.allow(now) {
			t.Fatalf("message %d after a long idle was refused", i)
		}
	}
	if b.allow(now) {
		t.Fatal("a long idle banked more than one burst")
	}
}

// TestFloodedClientIsDropped covers the readPump wiring end to end: a client
// that outruns the budget is disconnected rather than served.
func TestFloodedClientIsDropped(t *testing.T) {
	h := testHub()
	conn, srv := dialTest(t, h, true)
	defer srv.Close()
	defer conn.Close()

	// Comfortably past msgBurst, and faster than msgRefillPerSec can refill.
	for range msgBurst + 50 {
		if err := conn.WriteJSON(Message{Type: "ping"}); err != nil {
			break // the server has already closed the socket
		}
	}
	waitFor(t, func() bool {
		h.mu.RLock()
		defer h.mu.RUnlock()
		return len(h.clients) == 0
	})
}
