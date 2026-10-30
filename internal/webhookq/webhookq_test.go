package webhookq

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

type payload struct {
	N int `json:"n"`
}

func (p payload) LogAttrs() []any { return []any{"n", p.N} }

// plain has no LogAttrs; the queue must still work, just without the detail.
type plain struct {
	N int `json:"n"`
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestQueueDelivers(t *testing.T) {
	arrived := make(chan payload, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p payload
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &p)
		arrived <- p
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	q := New[payload](Options{Name: "test", Logger: discard(), URL: func(context.Context) (string, error) { return srv.URL, nil }})
	q.Enqueue(payload{N: 1})

	select {
	case got := <-arrived:
		if got.N != 1 {
			t.Fatalf("payload = %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("nothing arrived")
	}
}

// One goroutine, not one per payload: a collector reading a feed needs the
// events in the order they happened.
func TestDeliveryOrderIsPreserved(t *testing.T) {
	var (
		mu   sync.Mutex
		seen []int
	)
	done := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p payload
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &p)
		mu.Lock()
		seen = append(seen, p.N)
		n := len(seen)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		if n == 5 {
			done <- struct{}{}
		}
	}))
	defer srv.Close()

	q := New[payload](Options{Name: "test", Logger: discard(), URL: func(context.Context) (string, error) { return srv.URL, nil }})
	for i := 1; i <= 5; i++ {
		q.Enqueue(payload{N: i})
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("not all payloads arrived")
	}
	mu.Lock()
	defer mu.Unlock()
	for i, n := range seen {
		if n != i+1 {
			t.Fatalf("order = %v, want 1..5", seen)
		}
	}
}

// Payloads are accepted on the understanding that they will be sent. Without a
// drain, every restart would be a silent gap in the feed.
func TestShutdownFlushesTheQueue(t *testing.T) {
	arrived := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		arrived <- struct{}{}
	}))
	defer srv.Close()

	q := New[payload](Options{Name: "test", Logger: discard(), URL: func(context.Context) (string, error) { return srv.URL, nil }})
	ctx, cancel := context.WithCancel(context.Background())
	q.Run(ctx)
	for i := 1; i <= 3; i++ {
		q.Enqueue(payload{N: i})
	}
	cancel()

	deadline := time.After(5 * time.Second)
	for range 3 {
		select {
		case <-arrived:
		case <-deadline:
			t.Fatal("queued payloads died with the goroutine instead of being drained")
		}
	}
}

// A full queue is only reachable when the target has been silent for a long
// time. Dropping is the right answer; doing it silently is not.
func TestAFullQueueLogsTheDrop(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(&writerFunc{&buf, &mu}, nil))

	blocked := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-blocked
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	defer close(blocked)

	q := New[payload](Options{
		Name: "test", Logger: log, QueueSize: 1, Timeout: 30 * time.Second,
		URL: func(context.Context) (string, error) { return srv.URL, nil },
	})
	// One is taken by the delivery goroutine (and blocks), one fills the buffer,
	// the rest have nowhere to go.
	for i := range 10 {
		q.Enqueue(payload{N: i})
	}
	// The drop is synchronous with Enqueue, so by here it has been logged.
	mu.Lock()
	out := buf.String()
	mu.Unlock()
	if !strings.Contains(out, "queue full") {
		t.Fatalf("a dropped payload was not logged:\n%s", out)
	}
	// LogAttrs is what makes the line say WHICH payload was lost.
	if !strings.Contains(out, "n=") {
		t.Errorf("the drop line does not name the payload:\n%s", out)
	}
}

// A payload that does not describe itself is still deliverable; it just gets a
// less specific log line.
func TestAPayloadWithoutLogAttrsWorks(t *testing.T) {
	arrived := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		arrived <- struct{}{}
	}))
	defer srv.Close()
	q := New[plain](Options{Name: "test", Logger: discard(), URL: func(context.Context) (string, error) { return srv.URL, nil }})
	q.Enqueue(plain{N: 1})
	select {
	case <-arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("nothing arrived")
	}
}

func TestDeliverReportsANonSuccessStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	q := New[payload](Options{Name: "test", Logger: discard()})
	if err := q.Deliver(context.Background(), srv.URL, "", payload{N: 1}); err == nil {
		t.Fatal("a 500 was reported as a successful delivery")
	}
}

// writerFunc is a mutex-guarded io.Writer, so the test goroutine can read the
// buffer while a delivery goroutine may still be writing to it.
type writerFunc struct {
	buf *bytes.Buffer
	mu  *sync.Mutex
}

func (w *writerFunc) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// After shutdown, a payload is REPORTED rather than parked in a channel nobody
// reads. The window is real: the process context is cancelled first and the HTTP
// server then finishes its in-flight requests, every one of which can still
// audit. Silently swallowing those is the exact failure the drain and the
// queue-full warning exist to prevent.
func TestAPayloadQueuedAfterShutdownIsLogged(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(&writerFunc{&buf, &mu}, nil))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	q := New[payload](Options{Name: "test", Logger: log, URL: func(context.Context) (string, error) { return srv.URL, nil }})
	ctx, cancel := context.WithCancel(context.Background())
	q.Run(ctx)
	cancel()

	// Wait for the loop to finish its drain and mark itself stopped.
	deadline := time.Now().Add(3 * time.Second)
	for !q.stopped.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the delivery loop never marked itself stopped after the context ended")
		}
		time.Sleep(time.Millisecond)
	}

	q.Enqueue(payload{N: 99})
	mu.Lock()
	out := buf.String()
	mu.Unlock()
	if !strings.Contains(out, "shut down") {
		t.Fatalf("a payload queued after shutdown vanished without a word:\n%s", out)
	}
	if !strings.Contains(out, "n=99") {
		t.Errorf("the line does not say which payload was lost:\n%s", out)
	}
}

// A provider that CANNOT answer is not a provider that says "no target".
//
// Both used to arrive as "", and "" means "forwarding is off", so a settings
// read that failed — a database failover, a restart, a connection storm — turned
// off the feed for exactly as long as it failed, and left nothing behind to say
// so. The payload is still dropped (there is nowhere to put it), but loudly.
func TestAFailingURLLookupIsLoggedNotSwallowed(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(&syncWriter{w: &buf, mu: &mu}, nil))
	q := New[payload](Options{
		Name:   "test",
		Logger: log,
		URL: func(context.Context) (string, error) {
			return "", errors.New("connection refused")
		},
	})
	q.Enqueue(payload{N: 7})
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(buf.String(), "cannot read the target URL")
	})
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(buf.String(), "n=7") {
		t.Errorf("the log does not say WHICH payload was dropped:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "connection refused") {
		t.Errorf("the log does not say why:\n%s", buf.String())
	}
}

// A configured-empty URL stays silent: that is an integration nobody set up,
// not a failure, and it must not fill the log on every event.
func TestAnEmptyURLIsStillSilent(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(&syncWriter{w: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	q := New[payload](Options{
		Name:   "test",
		Logger: log,
		URL:    func(context.Context) (string, error) { return "", nil },
	})
	ctx, cancel := context.WithCancel(context.Background())
	q.Run(ctx)
	q.Enqueue(payload{N: 1})
	cancel()
	if err := q.Wait(context.Background()); err != nil {
		t.Fatalf("Wait = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(buf.String(), "cannot read") {
		t.Errorf("an unconfigured target logged a failure:\n%s", buf.String())
	}
}

// Wait joins the delivery goroutine, so a caller can shut down resources the
// DRAIN still needs — the pool behind the URL lookup, above all.
func TestWaitJoinsTheDrain(t *testing.T) {
	var delivered int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		atomic.AddInt32(&delivered, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	q := New[payload](Options{
		Name: "test", Logger: discard(),
		URL: func(context.Context) (string, error) { return srv.URL, nil },
	})
	ctx, cancel := context.WithCancel(context.Background())
	q.Run(ctx)
	q.Enqueue(payload{N: 1})
	q.Enqueue(payload{N: 2})
	cancel()

	// Nothing has been delivered yet, and Wait must not return while that is
	// still true.
	done := make(chan struct{})
	go func() { _ = q.Wait(context.Background()); close(done) }()
	select {
	case <-done:
		t.Fatal("Wait returned before the drain finished")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return after the drain finished")
	}
	if n := atomic.LoadInt32(&delivered); n != 2 {
		t.Errorf("delivered %d payloads before Wait returned, want 2", n)
	}
}

// Wait on a queue nobody started has nothing to join and must not hang.
func TestWaitOnAnUnstartedQueue(t *testing.T) {
	q := New[payload](Options{Name: "test", Logger: discard()})
	if err := q.Wait(context.Background()); err != nil {
		t.Fatalf("Wait = %v, want nil", err)
	}
}

// Wait is bounded: an unresponsive collector delays the exit, it must not
// prevent it.
func TestWaitGivesUpWithTheContext(t *testing.T) {
	// Order matters: srv.Close() waits for in-flight handlers, so the handler
	// has to be released BEFORE it runs. Deferred calls run last-registered
	// first, so close(block) is registered second.
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block)

	q := New[payload](Options{
		Name: "test", Logger: discard(), Timeout: 5 * time.Second,
		URL: func(context.Context) (string, error) { return srv.URL, nil },
	})
	ctx, cancel := context.WithCancel(context.Background())
	q.Run(ctx)
	q.Enqueue(payload{N: 1})
	cancel()

	short, cancelShort := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelShort()
	if err := q.Wait(short); err == nil {
		t.Fatal("Wait returned nil while the drain was still stuck; it must report the timeout")
	}
}

// syncWriter serialises writes from the delivery goroutine and the test.
type syncWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

// A payload accepted during the shutdown window must still be delivered.
//
// The drain is a single non-blocking sweep, so rooting the queue in the process
// context made it fire at the signal — microseconds in, against an empty queue —
// and set `stopped` long before the engine and the HTTP server had finished
// producing their last events. Those were then refused as "queued after the
// drain". The queue therefore gets its own context, cancelled at the END of
// shutdown; this is the property that arrangement buys.
func TestPayloadsAcceptedDuringShutdownAreStillDelivered(t *testing.T) {
	var delivered int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&delivered, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	q := New[payload](Options{
		Name: "test", Logger: discard(),
		URL: func(context.Context) (string, error) { return srv.URL, nil },
	})
	ctx, cancel := context.WithCancel(context.Background())
	q.Run(ctx)

	q.Enqueue(payload{N: 1})
	// The work that a graceful shutdown is still doing: engine drain, in-flight
	// requests. It happens BEFORE the queue is told to stop.
	q.Enqueue(payload{N: 2})
	q.Enqueue(payload{N: 3})

	cancel()
	if err := q.Wait(context.Background()); err != nil {
		t.Fatalf("Wait = %v", err)
	}
	if n := atomic.LoadInt32(&delivered); n != 3 {
		t.Errorf("delivered %d of 3 payloads; the rest were refused as queued after the drain", n)
	}
}
