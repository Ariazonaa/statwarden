package outbound

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
)

func sampleChange() domain.StateChange {
	return domain.StateChange{
		MonitorID:   7,
		MonitorName: "api",
		From:        domain.StatusUp,
		To:          domain.StatusDown,
		At:          time.Unix(1700000000, 0).UTC(),
		Message:     "connection refused",
	}
}

func TestPayloadFor(t *testing.T) {
	p := PayloadFor(sampleChange())
	if p.Event != "state_change" || p.To != "down" || p.Status != "down" || p.From != "up" {
		t.Fatalf("payload = %+v", p)
	}
}

func TestWebhookDelivers(t *testing.T) {
	var (
		mu       sync.Mutex
		gotBody  Payload
		gotCT    string
		received bool
	)
	arrived := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotCT = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		received = true
		w.WriteHeader(http.StatusNoContent)
		close(arrived)
	}))
	defer srv.Close()

	h := New(Options{URL: func(context.Context) (string, error) { return srv.URL, nil }})
	h.HandleStateChange(sampleChange())

	select {
	case <-arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("webhook was not received")
	}

	mu.Lock()
	defer mu.Unlock()
	if !received {
		t.Fatal("webhook was not received")
	}
	if gotCT != "application/json" {
		t.Fatalf("content-type = %q", gotCT)
	}
	if gotBody.MonitorID != 7 || gotBody.To != "down" {
		t.Fatalf("delivered payload = %+v", gotBody)
	}
}

func TestWebhookNoURLIsNoop(t *testing.T) {
	// An empty URL must not attempt any delivery (no panic, no error surfaced).
	h := New(Options{URL: func(context.Context) (string, error) { return "", nil }})
	h.HandleStateChange(sampleChange())
}

func TestWebhookServerErrorLogged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	// deliver returns an error on >=300; HandleStateChange swallows it (logged).
	h := New(Options{URL: func(context.Context) (string, error) { return srv.URL, nil }})
	if err := h.deliver(context.Background(), srv.URL, PayloadFor(sampleChange())); err == nil {
		t.Fatal("expected error on 500 response")
	}
}

// HandleStateChange runs on an engine worker, whose contract is to return
// immediately. A target that accepts the connection and then goes silent used
// to hold that worker for the full timeout — ten seconds per transition, at the
// moment a fleet-wide incident is producing them by the dozen.
func TestHandleStateChangeDoesNotWaitForTheTarget(t *testing.T) {
	blocked := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-blocked // never answers until the test says so
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	defer close(blocked)

	h := New(Options{
		URL:     func(context.Context) (string, error) { return srv.URL, nil },
		Timeout: 30 * time.Second,
	})

	returned := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		h.HandleStateChange(sampleChange())
		returned <- time.Since(start)
	}()

	select {
	case took := <-returned:
		if took > time.Second {
			t.Fatalf("HandleStateChange took %s; it waited for the target", took)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HandleStateChange blocked on an unresponsive target")
	}
}

// Order is preserved: a single delivery goroutine, not one per event.
func TestDeliveriesKeepTheirOrder(t *testing.T) {
	var (
		mu   sync.Mutex
		seen []int64
	)
	got := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p Payload
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &p)
		mu.Lock()
		seen = append(seen, p.MonitorID)
		n := len(seen)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		if n == 5 {
			got <- struct{}{}
		}
	}))
	defer srv.Close()

	h := New(Options{URL: func(context.Context) (string, error) { return srv.URL, nil }})
	for i := int64(1); i <= 5; i++ {
		sc := sampleChange()
		sc.MonitorID = i
		h.HandleStateChange(sc)
	}

	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("not all payloads arrived")
	}
	mu.Lock()
	defer mu.Unlock()
	for i, id := range seen {
		if id != int64(i+1) {
			t.Fatalf("delivery order = %v, want 1..5", seen)
		}
	}
}

// TestShutdownFlushesTheQueue
//
// Payloads are accepted from the engine on the understanding that they will be
// sent. Delivery moved onto its own goroutine so an unresponsive target could
// not park a checker worker; without a drain that change turned every restart
// into a silent gap in the feed, because whatever was queued died with the
// goroutine.
func TestShutdownFlushesTheQueue(t *testing.T) {
	var (
		mu   sync.Mutex
		seen int
	)
	arrived := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		seen++
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		arrived <- struct{}{}
	}))
	defer srv.Close()

	h := New(Options{URL: func(context.Context) (string, error) { return srv.URL, nil }})
	ctx, cancel := context.WithCancel(context.Background())
	h.Run(ctx)

	for i := int64(1); i <= 3; i++ {
		sc := sampleChange()
		sc.MonitorID = i
		h.HandleStateChange(sc)
	}
	cancel() // shutdown, with work possibly still queued

	deadline := time.After(5 * time.Second)
	for range 3 {
		select {
		case <-arrived:
		case <-deadline:
			mu.Lock()
			got := seen
			mu.Unlock()
			t.Fatalf("only %d of 3 queued payloads were delivered; the rest died "+
				"with the goroutine", got)
		}
	}
}
