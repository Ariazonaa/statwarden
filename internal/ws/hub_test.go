package ws

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/Ariazonaa/statwarden/internal/domain"
)

func testHub() *Hub {
	return NewHub(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// dialTest starts an httptest server whose handler upgrades with the given
// authentication flag and returns a connected client.
// dialTest opens a connection whose session, if any, stays valid forever.
func dialTest(t *testing.T, h *Hub, authenticated bool) (*websocket.Conn, *httptest.Server) {
	t.Helper()
	var auth func() bool
	if authenticated {
		auth = func() bool { return true }
	}
	return dialTestAuth(t, h, auth)
}

// dialTestAuth opens a connection with an explicit session re-check, so a test
// can revoke the session mid-connection.
func dialTestAuth(t *testing.T, h *Hub, auth func() bool) (*websocket.Conn, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeWS(w, r, auth)
	}))
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		srv.Close()
		t.Fatalf("dial: %v", err)
	}
	return conn, srv
}

func readMsg(t *testing.T, conn *websocket.Conn) Message {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m Message
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal %q: %v", raw, err)
	}
	return m
}

func TestSubscribeAndBroadcast(t *testing.T) {
	h := testHub()
	conn, srv := dialTest(t, h, true)
	defer srv.Close()
	defer conn.Close()

	// Subscribe to a monitor topic.
	if err := conn.WriteJSON(Message{Type: "subscribe", Topic: "monitor:7"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if m := readMsg(t, conn); m.Type != "subscribed" || m.Topic != "monitor:7" {
		t.Fatalf("subscribe ack = %+v", m)
	}

	// Wait until the hub has registered the subscription, then broadcast.
	waitFor(t, func() bool { return h.topicSize("monitor:7") == 1 })
	h.PublishStateChange(domain.StateChange{MonitorID: 7, From: domain.StatusUp, To: domain.StatusDown, At: time.Now()})

	m := readMsg(t, conn)
	if m.Type != "state_change" || m.Topic != "monitor:7" {
		t.Fatalf("broadcast = %+v", m)
	}
	var sc stateChangeView
	if err := json.Unmarshal(m.Data, &sc); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	if sc.To != "down" {
		t.Fatalf("state = %q, want down", sc.To)
	}
}

func TestHeartbeatBroadcastToDashboard(t *testing.T) {
	h := testHub()
	conn, srv := dialTest(t, h, true)
	defer srv.Close()
	defer conn.Close()

	_ = conn.WriteJSON(Message{Type: "subscribe", Topic: TopicDashboard})
	readMsg(t, conn) // ack
	waitFor(t, func() bool { return h.topicSize(TopicDashboard) == 1 })

	lat := 42 * time.Millisecond
	h.PublishHeartbeat(domain.Heartbeat{MonitorID: 3, Status: domain.StatusUp, CheckedAt: time.Now(), Latency: &lat})
	m := readMsg(t, conn)
	if m.Type != "heartbeat" || m.Topic != TopicDashboard {
		t.Fatalf("heartbeat = %+v", m)
	}
}

func TestUnauthenticatedLimitedToPublicTopics(t *testing.T) {
	h := testHub()
	conn, srv := dialTest(t, h, false)
	defer srv.Close()
	defer conn.Close()

	// A private topic is refused.
	_ = conn.WriteJSON(Message{Type: "subscribe", Topic: "monitor:1"})
	if m := readMsg(t, conn); m.Type != "error" {
		t.Fatalf("private subscribe = %+v, want error", m)
	}
	// A public status-page topic is allowed.
	_ = conn.WriteJSON(Message{Type: "subscribe", Topic: "status-page:acme"})
	if m := readMsg(t, conn); m.Type != "subscribed" {
		t.Fatalf("public subscribe = %+v, want subscribed", m)
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	h := testHub()
	conn, srv := dialTest(t, h, true)
	defer srv.Close()
	defer conn.Close()

	_ = conn.WriteJSON(Message{Type: "subscribe", Topic: "monitor:9"})
	readMsg(t, conn)
	waitFor(t, func() bool { return h.topicSize("monitor:9") == 1 })
	_ = conn.WriteJSON(Message{Type: "unsubscribe", Topic: "monitor:9"})
	readMsg(t, conn) // unsubscribed ack
	waitFor(t, func() bool { return h.topicSize("monitor:9") == 0 })

	// Broadcasting now reaches nobody; a ping still round-trips, proving the
	// connection is alive but got no state_change in between.
	h.PublishStateChange(domain.StateChange{MonitorID: 9, To: domain.StatusDown, At: time.Now()})
	_ = conn.WriteJSON(Message{Type: "ping"})
	if m := readMsg(t, conn); m.Type != "pong" {
		t.Fatalf("after unsubscribe expected pong, got %+v", m)
	}
}

func TestInvalidTopicRejected(t *testing.T) {
	h := testHub()
	conn, srv := dialTest(t, h, true)
	defer srv.Close()
	defer conn.Close()
	_ = conn.WriteJSON(Message{Type: "subscribe", Topic: "bogus:1"})
	if m := readMsg(t, conn); m.Type != "error" {
		t.Fatalf("invalid topic = %+v, want error", m)
	}
}

// waitFor polls cond until true or a timeout.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

// topicSize exposes the subscriber count for a topic (test-only).
func (h *Hub) topicSize(topic string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.topics[topic])
}

// --- session lifetime on an open socket ---------------------------------------

// TestWebSocketDropsRevokedSession pins that authentication is re-proved for
// the LIFE of the connection.
//
// It used to be resolved once, at the handshake, and reduced to a boolean on
// the client. A logout, a session expiry or an admin deactivating the account
// therefore left an already-open socket reading private monitor and dashboard
// events until the browser happened to disconnect — which for a dashboard left
// open in a tab can be days.
func TestWebSocketDropsRevokedSession(t *testing.T) {
	h := testHub()
	h.authRecheck = 20 * time.Millisecond
	var valid atomic.Bool
	valid.Store(true)

	conn, srv := dialTestAuth(t, h, func() bool { return valid.Load() })
	defer srv.Close()
	defer conn.Close()

	// While the session is live the PRIVATE dashboard topic is allowed.
	if err := conn.WriteJSON(Message{Type: "subscribe", Topic: TopicDashboard}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if m := readMsg(t, conn); m.Type != "subscribed" {
		t.Fatalf("subscribe ack = %+v", m)
	}
	waitFor(t, func() bool { return h.topicSize(TopicDashboard) == 1 })

	// Revoke it. The hub must drop the subscription and forget the client
	// without the browser doing anything.
	valid.Store(false)
	waitFor(t, func() bool { return h.topicSize(TopicDashboard) == 0 })
	waitFor(t, func() bool { return h.clientCount() == 0 })
}

// An anonymous connection has no session to revoke and must not be torn down by
// the re-check: public status-page viewers stay connected indefinitely.
func TestAnonymousWebSocketIsNotRevoked(t *testing.T) {
	h := testHub()
	h.authRecheck = 20 * time.Millisecond
	conn, srv := dialTestAuth(t, h, nil)
	defer srv.Close()
	defer conn.Close()

	if err := conn.WriteJSON(Message{Type: "subscribe", Topic: "status-page:acme"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if m := readMsg(t, conn); m.Type != "subscribed" {
		t.Fatalf("subscribe ack = %+v", m)
	}
	// Well past several re-check intervals, the connection is still live.
	time.Sleep(100 * time.Millisecond)
	if h.clientCount() != 1 {
		t.Fatalf("anonymous client count = %d, want 1", h.clientCount())
	}
	_ = conn.WriteJSON(Message{Type: "ping"})
	if m := readMsg(t, conn); m.Type != "pong" {
		t.Fatalf("expected pong on a live anonymous connection, got %+v", m)
	}
}
