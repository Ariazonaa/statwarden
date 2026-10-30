package ws

import (
	"strings"
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
)

// TestStateChangePokesAffectedStatusPages pins that a monitor transition pokes
// the public status pages that display the monitor, so their anonymous viewers
// refresh live. Before the fix PublishStateChange only touched the private
// monitor:{id} and dashboard:all topics, which the public page never listens
// on, so a state change never reached status-page:{slug} and the badge stayed
// stale until a manual reload.
//
// The poke must stay content-free for the same reason PublishStatusPage is: the
// topic is anonymous and password-ungated.
func TestStateChangePokesAffectedStatusPages(t *testing.T) {
	h := testHub()
	h.SetPageResolver(func(id int64) []string {
		if id == 7 {
			return []string{"acme"}
		}
		return nil
	})

	conn, srv := dialTest(t, h, false) // anonymous, like a real public viewer
	defer srv.Close()
	defer conn.Close()

	if err := conn.WriteJSON(Message{Type: "subscribe", Topic: "status-page:acme"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if m := readMsg(t, conn); m.Type != "subscribed" {
		t.Fatalf("subscribe ack = %+v", m)
	}
	waitFor(t, func() bool { return h.topicSize("status-page:acme") == 1 })

	h.PublishStateChange(domain.StateChange{
		MonitorID: 7, MonitorName: "API", From: domain.StatusUp, To: domain.StatusDown, At: time.Now(),
	})

	m := readMsg(t, conn)
	if m.Topic != "status-page:acme" {
		t.Fatalf("poke topic = %q, want status-page:acme", m.Topic)
	}
	// Content-free: no monitor name / status may leak onto the anonymous topic.
	if len(m.Data) != 0 && strings.TrimSpace(string(m.Data)) != "null" {
		t.Fatalf("status-page poke carried a payload %q; it must be content-free", string(m.Data))
	}
	if strings.Contains(string(m.Data)+m.Message, "API") {
		t.Fatalf("monitor detail leaked over the anonymous status-page topic")
	}
}

// TestStateChangeWithoutResolverSkipsStatusPages guards the nil-resolver path:
// an unconfigured hub must not attempt any status-page poke, and a page a
// monitor is not on must not be poked.
func TestStateChangeNoResolverNoStatusPagePoke(t *testing.T) {
	h := testHub() // no resolver set
	conn, srv := dialTest(t, h, false)
	defer srv.Close()
	defer conn.Close()

	_ = conn.WriteJSON(Message{Type: "subscribe", Topic: "status-page:acme"})
	readMsg(t, conn) // ack
	waitFor(t, func() bool { return h.topicSize("status-page:acme") == 1 })

	h.PublishStateChange(domain.StateChange{MonitorID: 7, To: domain.StatusDown, At: time.Now()})

	// Nothing should arrive on the status-page topic; a ping still round-trips.
	_ = conn.WriteJSON(Message{Type: "ping"})
	if m := readMsg(t, conn); m.Type != "pong" {
		t.Fatalf("expected pong (no status-page poke), got %+v", m)
	}
}
