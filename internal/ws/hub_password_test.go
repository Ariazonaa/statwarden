package ws

import (
	"strings"
	"testing"
	"time"
)

// TestStatusPageBroadcastCarriesNoPayload pins that a public status-page
// broadcast leaks no content over the anonymous status-page:{slug} topic. The
// topic is subscribable without authentication and cannot be gated per-viewer by
// the page password, so a password-protected page's incident title and body must
// never ride on it. Subscribers re-fetch through the password-gated HTTP endpoint
// on any envelope, so a content-free poke is enough.
//
// Before the fix PublishStatusPage marshalled the whole incident onto the frame;
// this test fails (the frame carries the secret body) without the payload-free
// change.
func TestStatusPageBroadcastCarriesNoPayload(t *testing.T) {
	h := testHub()
	conn, srv := dialTest(t, h, false) // anonymous client, as a real visitor is
	defer srv.Close()
	defer conn.Close()

	if err := conn.WriteJSON(Message{Type: "subscribe", Topic: "status-page:secret"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if m := readMsg(t, conn); m.Type != "subscribed" {
		t.Fatalf("subscribe ack = %+v", m)
	}
	waitFor(t, func() bool { return h.topicSize("status-page:secret") == 1 })

	// The secret an incident would carry on a password-protected page.
	const secret = "database credentials leaked to attacker"
	h.PublishStatusPage("secret", "incident", map[string]any{
		"incident": map[string]any{"title": "Breach", "body": secret},
	})

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	m := readMsg(t, conn)
	if m.Type != "incident" || m.Topic != "status-page:secret" {
		t.Fatalf("envelope = %+v, want an incident poke on the topic", m)
	}
	if len(m.Data) != 0 && strings.TrimSpace(string(m.Data)) != "null" {
		t.Fatalf("status-page broadcast carried a payload %q; it must be content-free", string(m.Data))
	}
	if strings.Contains(string(m.Data)+m.Message, secret) {
		t.Fatalf("secret incident body leaked over the anonymous topic")
	}
}
