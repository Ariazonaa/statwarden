package api

import (
	"context"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/push"
)

// beatRecorder is a PushRecorder that keeps the beats it was handed, for the
// tests that are about what the handler passes on rather than what it answers.
type beatRecorder struct {
	mu     sync.Mutex
	tokens []string
	beats  []push.Beat
}

func (b *beatRecorder) Record(_ context.Context, token string, beat push.Beat) (domain.Monitor, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokens = append(b.tokens, token)
	b.beats = append(b.beats, beat)
	return domain.Monitor{ID: 1, Status: domain.StatusUp}, nil
}

func (b *beatRecorder) last(t *testing.T) push.Beat {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.beats) == 0 {
		t.Fatal("no beat reached the recorder")
	}
	return b.beats[len(b.beats)-1]
}

func newPushHarness(t *testing.T) (*harness, *beatRecorder) {
	t.Helper()
	rec := &beatRecorder{}
	h := newHarnessTweaked(t, func(o *Options) { o.PushRecorder = rec })
	return h, rec
}

const wellFormedPushID = "0123456789abcdef0123456789abcdef"

// A string that cannot be a push token is answered like an unknown one, and is
// never used as a rate-limiter key. The per-token limiter keys on whatever the
// path carried, so an unauthenticated caller could fill its map with arbitrarily
// long distinct keys that live for a whole window each.
func TestPushRejectsMalformedTokenBeforeTheLimiter(t *testing.T) {
	h, rec := newPushHarness(t)
	for _, tok := range []string{
		strings.Repeat("a", 4096),
		"tok123",
		strings.ToUpper(wellFormedPushID), // GenerateToken emits lowercase only
		wellFormedPushID[:31] + "g",
	} {
		res := h.request(http.MethodPost, "/api/v1/push/"+tok, nil)
		if res.Code != http.StatusNotFound {
			t.Fatalf("malformed token %.40q = %d, want 404", tok, res.Code)
		}
	}
	if len(rec.tokens) != 0 {
		t.Fatalf("malformed tokens reached the recorder: %d calls", len(rec.tokens))
	}
	h.srv.pushLimiter.mu.Lock()
	keys := len(h.srv.pushLimiter.hits)
	h.srv.pushLimiter.mu.Unlock()
	if keys != 0 {
		t.Fatalf("malformed tokens created %d per-token limiter entries, want 0", keys)
	}

	if res := h.request(http.MethodPost, "/api/v1/push/"+wellFormedPushID, nil); res.Code != http.StatusOK {
		t.Fatalf("well-formed token = %d, want 200", res.Code)
	}
}

// ping is stored in an int4 column and widened to a time.Duration in
// milliseconds on the way; an unchecked value wrapped in both places.
func TestPushPingIsClampedToTheColumnRange(t *testing.T) {
	h, rec := newPushHarness(t)
	res := h.request(http.MethodPost, "/api/v1/push/"+wellFormedPushID+"?ping=9223372036854775807", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("push = %d", res.Code)
	}
	beat := rec.last(t)
	if beat.Ping == nil || *beat.Ping != math.MaxInt32 {
		t.Fatalf("ping = %v, want clamped to %d", beat.Ping, math.MaxInt32)
	}
}

// msg is free text from an unauthenticated caller that lands in every
// heartbeat row, the WebSocket feed and the alert body, so it is bounded — and
// made storable, since PostgreSQL text refuses NUL and invalid UTF-8 and the
// beat would be lost to a 500.
func TestPushMessageIsBoundedAndStorable(t *testing.T) {
	h, rec := newPushHarness(t)
	long := strings.Repeat("ä", maxPushMessageBytes)
	res := h.request(http.MethodPost, "/api/v1/push/"+wellFormedPushID+"?msg="+url.QueryEscape(long), nil)
	if res.Code != http.StatusOK {
		t.Fatalf("push = %d", res.Code)
	}
	msg := rec.last(t).Message
	if len(msg) > maxPushMessageBytes {
		t.Fatalf("message is %d bytes, want at most %d", len(msg), maxPushMessageBytes)
	}
	if !utf8.ValidString(msg) {
		t.Fatal("truncation split a multi-byte character")
	}

	res = h.request(http.MethodPost, "/api/v1/push/"+wellFormedPushID+"?msg=ok%00%FFdone", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("push = %d", res.Code)
	}
	msg = rec.last(t).Message
	if strings.ContainsRune(msg, 0) || !utf8.ValidString(msg) {
		t.Fatalf("message %q still carries bytes PostgreSQL text rejects", msg)
	}
	if !strings.HasPrefix(msg, "ok") || !strings.HasSuffix(msg, "done") {
		t.Fatalf("message %q lost its readable parts", msg)
	}
}
