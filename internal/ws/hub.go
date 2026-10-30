// Package ws implements Statwarden's topic-based WebSocket hub. Clients subscribe
// to topics (monitor:{id}, dashboard:all, status-page:{slug}); the hub is
// wired to the monitor engine's event stream and broadcasts heartbeats and
// state changes to the subscribers of the relevant topics. Session-cookie auth
// is applied at upgrade: unauthenticated connections may only subscribe to
// public status-page topics. Slow clients are dropped rather than blocking the
// hub.
package ws

import (
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"

	"github.com/Ariazonaa/statwarden/internal/safego"
)

// Topic name helpers and prefixes.
const (
	TopicDashboard   = "dashboard:all"
	prefixMonitor    = "monitor:"
	prefixStatusPage = "status-page:"
	clientSendBuffer = 64
	// maxSubscriptions caps how many topics one connection may hold at once.
	//
	// status-page:{slug} is subscribable by ANONYMOUS clients (isPublicTopic),
	// and /ws is registered outside every rate-limiter group, so without a cap a
	// single socket could loop subscribe with a fresh slug each time: c.subs grows
	// per topic and Hub.topics allocates a whole subscriber map per distinct name.
	// The slug shape check in validTopic bounds the length of each name; this
	// bounds how many a client may accumulate. Both are needed — a short slug is
	// still unlimited in quantity, and one long name is still one allocation.
	//
	// 64 is far above what the SPA holds (dashboard:all plus a handful of
	// monitor:/status-page: topics for the mounted views).
	maxSubscriptions = 64
)

// Message is the JSON envelope exchanged in both directions.
//
// Client → server: {"type":"subscribe","topic":"monitor:1"} / "unsubscribe" /
// "ping". Server → client: {"type":"heartbeat"|"state_change","topic":…,
// "data":…}, {"type":"subscribed"|"unsubscribed","topic":…},
// {"type":"error","message":…}, {"type":"pong"}.
type Message struct {
	Type    string          `json:"type"`
	Topic   string          `json:"topic,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
	Message string          `json:"message,omitempty"`
}

// Hub owns the set of connected clients and the topic subscription index.
type Hub struct {
	log *slog.Logger
	// authRecheck is how often an authenticated connection re-proves its
	// session. A field rather than a bare constant so a test can drive the
	// revocation path without waiting out the production interval.
	authRecheck time.Duration

	mu           sync.RWMutex
	clients      map[*client]struct{}
	topics       map[string]map[*client]struct{}
	pageResolver func(monitorID int64) []string
}

// NewHub returns a ready hub. Wire PublishStateChange / PublishHeartbeat to the
// engine and mount ServeWS on the /ws route.
func NewHub(logger *slog.Logger) *Hub {
	if logger == nil {
		logger = slog.Default()
	}
	return &Hub{
		log:         logger,
		authRecheck: authRecheckPeriod,
		clients:     map[*client]struct{}{},
		topics:      map[string]map[*client]struct{}{},
	}
}

// SetPageResolver registers the function PublishStateChange uses to find the
// status-page slugs that display a given monitor, so a monitor transition pokes
// those public pages live. Wire it once at startup (before serving) with a
// store-backed lookup; a nil resolver simply skips the status-page poke. It is
// called on the broadcast path and must be safe for concurrent use.
func (h *Hub) SetPageResolver(fn func(monitorID int64) []string) {
	h.mu.Lock()
	h.pageResolver = fn
	h.mu.Unlock()
}

// isPublicTopic reports whether an unauthenticated client may subscribe to
// topic. Only status-page topics are public.
func isPublicTopic(topic string) bool {
	return strings.HasPrefix(topic, prefixStatusPage)
}

// validTopic reports whether topic is a well-formed subscribable topic.
func validTopic(topic string) bool {
	switch {
	case topic == TopicDashboard:
		return true
	case strings.HasPrefix(topic, prefixMonitor):
		_, err := strconv.ParseInt(strings.TrimPrefix(topic, prefixMonitor), 10, 64)
		return err == nil
	case strings.HasPrefix(topic, prefixStatusPage):
		// Not merely non-empty: this family is the one anonymous clients may
		// subscribe to, so the hub must not trust that the slug on the wire came
		// from a status page that exists. Same rule the API enforces on write.
		return domain.ValidStatusPageSlug(strings.TrimPrefix(topic, prefixStatusPage))
	default:
		return false
	}
}

// register adds c to the hub.
func (h *Hub) register(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[c] = struct{}{}
}

// unregister removes c and all its topic memberships.
func (h *Hub) unregister(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.clients[c]; !ok {
		return
	}
	delete(h.clients, c)
	for topic := range c.subs {
		if set := h.topics[topic]; set != nil {
			delete(set, c)
			if len(set) == 0 {
				delete(h.topics, topic)
			}
		}
	}
	// c.send is intentionally NOT closed here: readPump's trySend and broadcast
	// write to it without holding a lock across the send, so closing it would
	// race them into a "send on closed channel" panic. Teardown is signalled via
	// c.done (see client.close); the send channel is simply abandoned to GC.
}

// subscribe joins c to topic (enforcing the public-topic rule). It returns an
// error string suitable for the client, or "".
func (h *Hub) subscribe(c *client, topic string) string {
	if !validTopic(topic) {
		return "unknown topic"
	}
	if !c.authenticated.Load() && !isPublicTopic(topic) {
		return "authentication required for this topic"
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// c.subs is read and written only under h.mu (here, unsubscribe and
	// unregister), so the cap is checked against the same state it guards.
	// Re-subscribing to a topic already held is idempotent and must not be
	// refused at the boundary — it adds nothing to either map.
	if _, held := c.subs[topic]; !held && len(c.subs) >= maxSubscriptions {
		return "subscription limit reached"
	}
	if h.topics[topic] == nil {
		h.topics[topic] = map[*client]struct{}{}
	}
	h.topics[topic][c] = struct{}{}
	c.subs[topic] = struct{}{}
	return ""
}

// unsubscribe removes c from topic.
func (h *Hub) unsubscribe(c *client, topic string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if set := h.topics[topic]; set != nil {
		delete(set, c)
		if len(set) == 0 {
			delete(h.topics, topic)
		}
	}
	delete(c.subs, topic)
}

// broadcast sends payload to every client subscribed to topic. A client whose
// send buffer is full is dropped (closed) to keep one slow reader from stalling
// the hub.
func (h *Hub) broadcast(topic string, payload []byte) {
	h.mu.RLock()
	var slow []*client
	for c := range h.topics[topic] {
		select {
		case c.send <- payload:
		default:
			slow = append(slow, c)
		}
	}
	h.mu.RUnlock()
	for _, c := range slow {
		h.log.Warn("dropping slow websocket client", "topic", topic)
		c.close()
	}
}

// PublishStateChange broadcasts a monitor transition to its per-monitor topic
// and the global dashboard topic.
func (h *Hub) PublishStateChange(sc domain.StateChange) {
	data, err := json.Marshal(stateChangeView{
		MonitorID:   sc.MonitorID,
		MonitorName: sc.MonitorName,
		From:        string(sc.From),
		To:          string(sc.To),
		At:          sc.At.UnixMilli(),
		Message:     sc.Message,
	})
	if err != nil {
		return
	}
	h.publish("state_change", monitorTopic(sc.MonitorID), data)
	h.publish("state_change", TopicDashboard, data)

	// A transition also changes what every public status page that lists this
	// monitor should show, but those pages listen on status-page:{slug}, not on
	// the private monitor:/dashboard: topics. Poke each affected page so its
	// anonymous viewers refresh. The poke is content-free for the same reason
	// PublishStatusPage is (see its doc): status-page:{slug} is an anonymous,
	// password-ungated topic, so the monitor's name/status must not ride on it —
	// subscribers re-fetch through the password-gated public JSON endpoint.
	h.mu.RLock()
	resolve := h.pageResolver
	h.mu.RUnlock()
	if resolve != nil {
		// The resolver reads the database, while PublishStateChange runs on the
		// engine's synchronous OnEvent path, which must stay fast and
		// non-blocking (see engine.Options.OnEvent). The poke is content-free and
		// means only "something changed", so nothing depends on it landing before
		// this returns — resolve and publish on our own goroutine instead of
		// holding an engine worker for the round trip.
		safego.Go(h.log, "status-page poke", func() {
			for _, slug := range resolve(sc.MonitorID) {
				h.publish("monitor", prefixStatusPage+slug, nil)
			}
		})
	}
}

// PublishHeartbeat broadcasts a heartbeat to its per-monitor topic and the
// dashboard topic.
func (h *Hub) PublishHeartbeat(hb domain.Heartbeat) {
	view := heartbeatView{
		MonitorID: hb.MonitorID,
		Status:    string(hb.Status),
		At:        hb.CheckedAt.UnixMilli(),
		Message:   hb.Message,
		Important: hb.Important,
		Retries:   hb.Retries,
	}
	if hb.Latency != nil {
		ms := int(hb.Latency.Milliseconds())
		view.LatencyMs = &ms
	}
	data, err := json.Marshal(view)
	if err != nil {
		return
	}
	h.publish("heartbeat", monitorTopic(hb.MonitorID), data)
	h.publish("heartbeat", TopicDashboard, data)
}

// PublishStatusPage announces a public status-page update (an incident change,
// a layout reorder, …) to the page's public topic.
//
// The announcement is deliberately payload-FREE. status-page:{slug} is a public
// topic any anonymous client may subscribe to (isPublicTopic), and it carries no
// session, so it cannot be gated per-viewer the way the HTTP channels are by the
// page password. Marshalling the incident body onto it would leak a
// password-protected page's contents to any anonymous socket — the same bypass
// the password fix closes on RSS, subscribe and the badge. Subscribers instead
// treat any envelope on the topic as "something changed" and re-fetch through
// the password-gated JSON endpoint, so a content-free poke conveys the update
// without disclosing it. The payload argument is accepted for interface
// compatibility and intentionally ignored.
func (h *Hub) PublishStatusPage(slug, event string, _ any) {
	h.publish(event, prefixStatusPage+slug, nil)
}

func (h *Hub) publish(msgType, topic string, data json.RawMessage) {
	env, err := json.Marshal(Message{Type: msgType, Topic: topic, Data: data})
	if err != nil {
		return
	}
	h.broadcast(topic, env)
}

func monitorTopic(id int64) string { return prefixMonitor + strconv.FormatInt(id, 10) }

type stateChangeView struct {
	MonitorID   int64  `json:"monitor_id"`
	MonitorName string `json:"monitor_name"`
	From        string `json:"from"`
	To          string `json:"to"`
	At          int64  `json:"at"`
	Message     string `json:"message,omitempty"`
}

type heartbeatView struct {
	MonitorID int64  `json:"monitor_id"`
	Status    string `json:"status"`
	At        int64  `json:"at"`
	LatencyMs *int   `json:"latency_ms,omitempty"`
	Message   string `json:"message,omitempty"`
	Important bool   `json:"important"`
	Retries   int    `json:"retries"`
}
