package siem

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func sampleEvent() Event {
	uid := int64(3)
	return Event{
		ID:         42,
		Action:     "monitor.create",
		EntityType: "monitor",
		UserID:     &uid,
		IP:         "203.0.113.7",
		Details:    json.RawMessage(`{"name":"api"}`),
		At:         time.Unix(1700000000, 0).UTC(),
	}
}

func TestForwarderDelivers(t *testing.T) {
	var (
		mu  sync.Mutex
		got Event
		ct  string
		ua  string
	)
	arrived := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		ct, ua = r.Header.Get("Content-Type"), r.Header.Get("User-Agent")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusNoContent)
		close(arrived)
	}))
	defer srv.Close()

	f := New(Options{URL: func(context.Context) (string, error) { return srv.URL, nil }})
	f.HandleAudit(context.Background(), sampleEvent())

	select {
	case <-arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("the event was never delivered")
	}

	mu.Lock()
	defer mu.Unlock()
	if ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if ua != "Statwarden-Audit/1" {
		t.Errorf("User-Agent = %q; a collector receiving several feeds tells them apart by this", ua)
	}
	// The discriminator and version are stamped by HandleAudit, not the caller,
	// so no call site can forget them.
	if got.Event != "audit" {
		t.Errorf("event = %q, want audit", got.Event)
	}
	if got.Version != eventVersion {
		t.Errorf("version = %d, want %d", got.Version, eventVersion)
	}
	if got.ID != 42 || got.Action != "monitor.create" {
		t.Errorf("payload = %+v", got)
	}
}

// No URL configured is the normal state of an instance nobody set up. It must
// be a silent no-op, not an error and not a panic.
func TestNoURLIsANoop(t *testing.T) {
	f := New(Options{URL: func(context.Context) (string, error) { return "", nil }})
	f.HandleAudit(context.Background(), sampleEvent())
}

// HandleAudit runs inside an HTTP handler that is holding a request open. A
// collector that accepts the connection and then says nothing must not become
// this instance's response time.
func TestHandleAuditDoesNotWaitForTheCollector(t *testing.T) {
	blocked := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-blocked
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	defer close(blocked)

	f := New(Options{
		URL:     func(context.Context) (string, error) { return srv.URL, nil },
		Timeout: 30 * time.Second,
	})

	returned := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		f.HandleAudit(context.Background(), sampleEvent())
		returned <- time.Since(start)
	}()
	select {
	case took := <-returned:
		if took > time.Second {
			t.Fatalf("HandleAudit took %s; it waited for the collector", took)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HandleAudit blocked on an unresponsive collector")
	}
}

// The instance name is a settings read, i.e. a database round trip, on a path
// every mutating request in the product goes through. It is cached.
func TestInstanceNameIsCachedNotReadPerEvent(t *testing.T) {
	var calls int
	now := time.Unix(1700000000, 0)
	f := New(Options{
		URL: func(context.Context) (string, error) { return "", nil },
		Instance: func(context.Context) (string, error) {
			calls++
			return "prod", nil
		},
	})
	f.now = func() time.Time { return now }

	for range 100 {
		f.HandleAudit(context.Background(), sampleEvent())
	}
	if calls != 1 {
		t.Fatalf("the instance provider ran %d times for 100 events; it must be cached", calls)
	}
	if got := f.cachedInstance(context.Background()); got != "prod" {
		t.Fatalf("cached instance = %q", got)
	}

	// Past the TTL it refreshes, so a rename does reach the feed.
	now = now.Add(instanceTTL)
	f.HandleAudit(context.Background(), sampleEvent())
	if calls != 2 {
		t.Fatalf("the provider ran %d times; a rename would never reach the collector", calls)
	}
}

// An Instance the caller already set wins over the cache. That is the seam a
// test — or a future multi-tenant caller — writes through.
func TestAnExplicitInstanceIsKept(t *testing.T) {
	arrived := make(chan Event, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e Event
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &e)
		arrived <- e
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	f := New(Options{
		URL:      func(context.Context) (string, error) { return srv.URL, nil },
		Instance: func(context.Context) (string, error) { return "from-settings", nil },
	})
	e := sampleEvent()
	e.Instance = "explicit"
	f.HandleAudit(context.Background(), e)

	select {
	case got := <-arrived:
		if got.Instance != "explicit" {
			t.Fatalf("instance = %q, want the caller's value", got.Instance)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("nothing arrived")
	}
}

// The audit log is the record of truth and this feed is a copy of it, so a
// collector that is down loses events rather than holding the instance up. The
// loss is logged, never silent — that is what LogAttrs is for.
func TestEventNamesItselfInLogs(t *testing.T) {
	attrs := sampleEvent().LogAttrs()
	if len(attrs) != 4 || attrs[0] != "audit_id" || attrs[2] != "action" {
		t.Fatalf("LogAttrs = %v; a dropped event must be identifiable in the log", attrs)
	}
	// The details blob is deliberately not in the log line: it can carry a whole
	// request body, and a log is not the place for it.
	for _, a := range attrs {
		if s, ok := a.(string); ok && s == "details" {
			t.Fatal("details must not be logged")
		}
	}
}

// A slow instance provider must not become latency on every concurrent audit.
//
// The provider is a settings read — a database round trip — and HandleAudit runs
// inside an HTTP handler that has ALREADY written its audit row. Holding the
// cache mutex across that read made every concurrent mutating request queue
// behind one query, after their own work had succeeded: a database that merely
// got slow would have shown up as write latency across the whole product,
// contributed by the one component whose contract is "never blocks".
func TestASlowInstanceProviderDoesNotSerialiseCallers(t *testing.T) {
	release := make(chan struct{})
	var calls int32
	f := New(Options{
		URL: func(context.Context) (string, error) { return "", nil },
		Instance: func(context.Context) (string, error) {
			// The first caller blocks in here; every other one must be able to
			// finish without waiting for it.
			if atomic.AddInt32(&calls, 1) == 1 {
				<-release
			}
			return "prod", nil
		},
	})

	blocked := make(chan struct{})
	go func() {
		f.HandleAudit(context.Background(), sampleEvent())
		close(blocked)
	}()
	// Let the first caller get into the provider before the others start.
	for atomic.LoadInt32(&calls) == 0 {
		time.Sleep(time.Millisecond)
	}

	done := make(chan struct{})
	go func() {
		for range 20 {
			f.HandleAudit(context.Background(), sampleEvent())
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("concurrent HandleAudit calls queued behind one slow settings read")
	}

	close(release)
	<-blocked
}

// A failed instance lookup must not be cached as an answer.
//
// The provider returned "" for a database it could not reach, that "" was
// written into the cache with a fresh timestamp, and for the next thirty
// seconds every event went out with no instance at all — on a collector serving
// several Statwardens, half a minute of events that cannot be attributed to any of
// them. The last good name is stale but true, and the expiry stays in place so
// the next event retries.
func TestAFailedInstanceReadKeepsTheLastKnownName(t *testing.T) {
	var fail atomic.Bool
	now := time.Unix(1700000000, 0)
	var buf bytes.Buffer
	f := New(Options{
		URL:    func(context.Context) (string, error) { return "", nil },
		Logger: slog.New(slog.NewTextHandler(&buf, nil)),
		Instance: func(context.Context) (string, error) {
			if fail.Load() {
				return "", errors.New("connection refused")
			}
			return "prod", nil
		},
	})
	f.now = func() time.Time { return now }

	if got := f.cachedInstance(context.Background()); got != "prod" {
		t.Fatalf("first read = %q, want prod", got)
	}
	// Past the TTL, with the settings read now failing.
	fail.Store(true)
	now = now.Add(instanceTTL + time.Second)
	if got := f.cachedInstance(context.Background()); got != "prod" {
		t.Errorf("after a failed refresh the name is %q, want the last known \"prod\"", got)
	}
	if !strings.Contains(buf.String(), "cannot read the instance name") {
		t.Errorf("a failed refresh was silent:\n%s", buf.String())
	}

	// And the failure was not cached: the next read retries, and succeeds.
	fail.Store(false)
	if got := f.cachedInstance(context.Background()); got != "prod" {
		t.Errorf("recovery read = %q", got)
	}
}

// A failed read must not stop the event going out. The instance is a label; an
// audit event without one still beats no audit event.
func TestEventsStillFlowWhileTheInstanceIsUnreadable(t *testing.T) {
	arrived := make(chan Event, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e Event
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &e)
		arrived <- e
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	f := New(Options{
		URL:      func(context.Context) (string, error) { return srv.URL, nil },
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Instance: func(context.Context) (string, error) { return "", errors.New("nope") },
	})
	f.HandleAudit(context.Background(), sampleEvent())
	select {
	case e := <-arrived:
		if e.Action == "" {
			t.Error("the event arrived empty")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event arrived; an unreadable instance name must not stop the feed")
	}
}
