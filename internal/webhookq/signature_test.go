package webhookq

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testSecret = "whsec-0123456789abcdef"

func TestSignVerifyRoundTrip(t *testing.T) {
	at := time.Unix(1790000000, 0)
	body := []byte(`{"n":1}`)
	h := Sign(testSecret, at, body)
	if !strings.HasPrefix(h, "t=1790000000,v1=") {
		t.Fatalf("header = %q, want t=…,v1=…", h)
	}
	if err := Verify(testSecret, h, body, at.Add(time.Minute), DefaultTolerance); err != nil {
		t.Fatalf("Verify of a fresh signature: %v", err)
	}
}

// Every part of what is signed has to matter, or the signature proves nothing.
func TestVerifyRejectsTampering(t *testing.T) {
	at := time.Unix(1790000000, 0)
	body := []byte(`{"n":1}`)
	h := Sign(testSecret, at, body)
	cases := map[string]error{
		"another body":   Verify(testSecret, h, []byte(`{"n":2}`), at, DefaultTolerance),
		"another secret": Verify("whsec-somebody-else-entirely", h, body, at, DefaultTolerance),
		// Moving t forward to dodge the replay window must break the MAC.
		"a forged timestamp": Verify(testSecret, strings.Replace(h, "t=1790000000", "t=1790000600", 1),
			body, at.Add(10*time.Minute), DefaultTolerance),
		"no header":      Verify(testSecret, "", body, at, DefaultTolerance),
		"garbage header": Verify(testSecret, "t=abc,v1=zz", body, at, DefaultTolerance),
	}
	for name, err := range cases {
		if err == nil {
			t.Errorf("%s: Verify accepted it", name)
		}
	}
}

// A captured request replayed later is refused once it falls outside the
// tolerance — in either direction, so a clock running ahead is covered too.
func TestVerifyRefusesAStaleTimestamp(t *testing.T) {
	at := time.Unix(1790000000, 0)
	body := []byte(`{}`)
	h := Sign(testSecret, at, body)
	if err := Verify(testSecret, h, body, at.Add(6*time.Minute), DefaultTolerance); err == nil {
		t.Error("a six-minute-old signature was accepted")
	}
	if err := Verify(testSecret, h, body, at.Add(-6*time.Minute), DefaultTolerance); err == nil {
		t.Error("a signature from six minutes in the future was accepted")
	}
}

// capture records the headers and body of every request it receives.
type capture struct {
	header chan http.Header
	body   chan []byte
}

func newCapture(t *testing.T) (*capture, *httptest.Server) {
	t.Helper()
	c := &capture{header: make(chan http.Header, 4), body: make(chan []byte, 4)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.header <- r.Header.Clone()
		c.body <- b
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return c, srv
}

// The queue signs the exact bytes it sends, so a receiver verifying the raw body
// it got succeeds.
func TestQueueSignsWhenASecretIsSet(t *testing.T) {
	c, srv := newCapture(t)
	now := time.Now()
	q := New[payload](Options{
		Name:   "test",
		Logger: discard(),
		URL:    func(context.Context) (string, error) { return srv.URL, nil },
		Secret: func(context.Context) (string, error) { return testSecret, nil },
		Now:    func() time.Time { return now },
	})
	q.Enqueue(payload{N: 7})

	select {
	case h := <-c.header:
		body := <-c.body
		if err := Verify(testSecret, h.Get(SignatureHeader), body, now, DefaultTolerance); err != nil {
			t.Fatalf("receiver-side Verify failed: %v (header %q)", err, h.Get(SignatureHeader))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("nothing arrived")
	}
}

// No secret, no header: an unconfigured instance behaves exactly as before.
func TestQueueSendsUnsignedWithoutASecret(t *testing.T) {
	c, srv := newCapture(t)
	q := New[payload](Options{
		Name:   "test",
		Logger: discard(),
		URL:    func(context.Context) (string, error) { return srv.URL, nil },
	})
	q.Enqueue(payload{N: 1})
	select {
	case h := <-c.header:
		if v := h.Get(SignatureHeader); v != "" {
			t.Fatalf("unsigned delivery carries %s: %q", SignatureHeader, v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("nothing arrived")
	}
}

// A secret that cannot be read must not turn into an unsigned delivery — that
// would be a silent downgrade of exactly the guarantee the operator configured.
func TestAFailingSecretLookupDropsRatherThanDowngrades(t *testing.T) {
	c, srv := newCapture(t)
	var (
		logs bytes.Buffer
		mu   sync.Mutex
	)
	q := New[payload](Options{
		Name:   "test",
		Logger: slog.New(slog.NewTextHandler(&syncWriter{w: &logs, mu: &mu}, nil)),
		URL:    func(context.Context) (string, error) { return srv.URL, nil },
		Secret: func(context.Context) (string, error) { return "", errors.New("database is down") },
	})
	ctx, cancel := context.WithCancel(context.Background())
	q.Run(ctx)
	q.Enqueue(payload{N: 1})
	cancel()
	if err := q.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case h := <-c.header:
		t.Fatalf("a payload went out although its secret could not be read (signature %q)",
			h.Get(SignatureHeader))
	default:
	}
	mu.Lock()
	logged := logs.String()
	mu.Unlock()
	if !strings.Contains(logged, "cannot read the signing secret") {
		t.Fatalf("the drop was not logged: %s", logged)
	}
}
