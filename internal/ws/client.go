package ws

import (
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/Ariazonaa/statwarden/internal/safego"
)

// Connection tuning.
const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 4 << 10 // client control messages are tiny
	// authRecheckPeriod is how often an authenticated connection re-proves its
	// session. It bounds how long a logged-out, expired or deactivated account
	// can keep reading private topics on a socket that is already open.
	authRecheckPeriod = 30 * time.Second
	// msgBurst and msgRefillPerSec budget inbound client messages.
	//
	// maxSubscriptions bounds what a connection can RETAIN; this bounds how fast
	// it can ask. Without it a socket can spin subscribe/unsubscribe as fast as
	// the network allows, which costs a JSON decode and the hub write lock per
	// message and is reachable anonymously (/ws sits outside every HTTP
	// rate-limiter group).
	//
	// The burst must clear what a legitimate client does in one go: on reconnect
	// the SPA re-subscribes every topic it holds, at most maxSubscriptions of
	// them. 128 leaves that twice over, and 10/s sustained is far above a UI that
	// only subscribes when a view mounts.
	msgBurst        = 128
	msgRefillPerSec = 10
)

// msgBudget is a token bucket over inbound messages. readPump is the only
// goroutine that touches one, so it needs no lock and no timer: tokens are
// refilled from elapsed time on each call.
type msgBudget struct {
	tokens float64
	last   time.Time
}

// allow charges one message against the budget and reports whether it fits.
func (b *msgBudget) allow(now time.Time) bool {
	if b.last.IsZero() {
		b.tokens = msgBurst
	} else {
		b.tokens = min(b.tokens+now.Sub(b.last).Seconds()*msgRefillPerSec, msgBurst)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1 << 10,
	WriteBufferSize: 1 << 10,
	// Permissive ON PURPOSE, because the check happens earlier and elsewhere.
	//
	// This used to read "same-origin is enforced by the SPA being served from
	// the same origin; a stricter check can be layered in with the trusted-proxy
	// work". Both halves were wrong. A WebSocket handshake is NOT subject to the
	// same-origin policy — that is exactly what makes cross-site WebSocket
	// hijacking possible — so being served from one origin enforces nothing. And
	// the stricter check is not pending: it is api.wsOriginAllowed, which
	// compares scheme, host AND port against the configured PUBLIC_BASE_URL plus
	// ALLOWED_ORIGINS, and which handleWS applies BEFORE adopting any identity.
	//
	// What is left here is a real constraint: whoever calls Hub.ServeWS is
	// responsible for the origin. Today that is one caller. A second one inherits
	// no protection from this line.
	CheckOrigin: func(*http.Request) bool { return true },
}

// client is one WebSocket connection.
//
// Channel lifecycle: c.send is written by many goroutines (readPump via
// trySend, and the hub via broadcast) and drained by the single writePump.
// It is deliberately NEVER closed — closing it while a concurrent trySend or
// broadcast is mid-send is a "send on closed channel" panic that no amount of
// sync.Once around the close can prevent, because the send side takes no lock.
// Instead the single owner of teardown is c.done: close() closes it exactly
// once, senders select on it to stop, and writePump watches it to emit the
// close frame and exit. This gives the connection one shutdown signal with one
// owner instead of a closed channel racing live senders.
type client struct {
	hub  *Hub
	conn *websocket.Conn
	send chan []byte
	// authenticated is atomic because authPump clears it from its own goroutine
	// while hub.subscribe reads it.
	authenticated atomic.Bool
	// auth re-proves the connection's session; nil for an anonymous client.
	auth func() bool

	subs      map[string]struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// ServeWS upgrades an HTTP request to a WebSocket connection and runs its
// pumps.
//
// auth is nil for an anonymous connection, which is limited to public topics.
// A non-nil auth means the caller resolved a valid session at the handshake AND
// can re-resolve it later: the session was previously checked exactly once, so
// a logout, an expiry or an admin revoking the account left an already-open
// socket reading private monitor and dashboard events until the browser
// happened to disconnect. It is re-checked every authRecheckPeriod for as long
// as the connection lives.
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request, auth func() bool) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.log.Debug("websocket upgrade failed", "error", err)
		return
	}
	c := &client{
		hub:  h,
		conn: conn,
		send: make(chan []byte, clientSendBuffer),
		auth: auth,
		subs: map[string]struct{}{},
		done: make(chan struct{}),
	}
	c.authenticated.Store(auth != nil)
	h.register(c)
	safego.Go(c.hub.log, "ws writePump", c.writePump)
	safego.Go(c.hub.log, "ws authPump", c.authPump)
	safego.Go(c.hub.log, "ws readPump", c.readPump)
}

// authPump re-proves the session for as long as the connection is open and
// tears it down the moment it stops being valid. An anonymous connection has
// nothing to revoke, so it runs no timer at all.
func (c *client) authPump() {
	if c.auth == nil {
		return
	}
	ticker := time.NewTicker(c.hub.authRecheck)
	// Fail closed. c.auth reaches the session store, so it can panic; without the
	// close here that panic would end this goroutine and leave the socket open and
	// still marked authenticated, never re-checked again — silently restoring the
	// exact revocation hole this pump exists to close. Dropping the connection is
	// also what the pump does when it cannot prove the session for any other
	// reason, so it is the same policy rather than a new one.
	defer func() {
		ticker.Stop()
		c.close()
	}()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			if c.auth() {
				continue
			}
			// Logged out, expired, or the account was deactivated. Drop the
			// whole connection rather than silently demoting it: every topic
			// this client holds was granted on the strength of that session, and
			// a client that wants public topics can reconnect anonymously.
			c.authenticated.Store(false)
			c.hub.log.Info("websocket session no longer valid, closing connection")
			c.trySend(errorMessage("session expired"))
			c.close()
			return
		}
	}
}

// close tears down the connection exactly once. Signalling c.done (never
// c.send) is the single teardown owner: it stops senders and writePump without
// closing the channel they are writing to.
func (c *client) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		c.hub.unregister(c)
		_ = c.conn.Close()
	})
}

// readPump processes inbound client messages until the connection closes.
func (c *client) readPump() {
	defer c.close()
	c.conn.SetReadLimit(maxMessageSize)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})
	var budget msgBudget
	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		// Charged before the decode, so a flood costs the read and nothing else.
		// A client over budget is dropped rather than throttled: the ceiling is
		// well above any legitimate use, so reaching it is abuse, and the hub
		// already drops clients it will not serve (see broadcast's slow reader).
		if !budget.allow(time.Now()) {
			c.trySend(errorMessage("message rate limit exceeded"))
			return
		}
		var msg Message
		if err := json.Unmarshal(raw, &msg); err != nil {
			c.trySend(errorMessage("malformed message"))
			continue
		}
		c.handle(msg)
	}
}

func (c *client) handle(msg Message) {
	switch msg.Type {
	case "subscribe":
		if reason := c.hub.subscribe(c, msg.Topic); reason != "" {
			c.trySend(errorMessage(reason))
			return
		}
		c.trySend(mustMarshal(Message{Type: "subscribed", Topic: msg.Topic}))
	case "unsubscribe":
		c.hub.unsubscribe(c, msg.Topic)
		c.trySend(mustMarshal(Message{Type: "unsubscribed", Topic: msg.Topic}))
	case "ping":
		c.trySend(mustMarshal(Message{Type: "pong"}))
	default:
		c.trySend(errorMessage("unknown message type"))
	}
}

// trySend enqueues payload, dropping the client if its buffer is full. The
// c.done arm makes the send a no-op once the connection is being torn down, so
// a late sender neither blocks nor writes to a channel with no reader; because
// c.send is never closed, none of these arms can panic.
func (c *client) trySend(payload []byte) {
	select {
	case <-c.done:
	case c.send <- payload:
	default:
		c.close()
	}
}

// writePump drains the send channel to the socket and keeps it alive with
// periodic pings.
func (c *client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.close()
	}()
	for {
		select {
		case <-c.done:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			_ = c.conn.WriteMessage(websocket.CloseMessage, nil)
			return
		case payload := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func errorMessage(reason string) []byte {
	return mustMarshal(Message{Type: "error", Message: reason})
}

func mustMarshal(m Message) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		return []byte(`{"type":"error","message":"encoding error"}`)
	}
	return b
}
