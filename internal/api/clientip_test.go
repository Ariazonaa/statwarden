package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

// mustPrefixes parses CIDRs the way config.Load does, failing the test on typos.
func mustPrefixes(t *testing.T, cidrs ...string) []netip.Prefix {
	t.Helper()
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			t.Fatalf("ParsePrefix(%q): %v", c, err)
		}
		out = append(out, p.Masked())
	}
	return out
}

func TestResolveClientIP(t *testing.T) {
	tests := []struct {
		name     string
		trusted  []string
		peer     string
		forwards []string // one entry per X-Forwarded-For header
		want     string
	}{
		{
			name: "no trusted proxies ignores a spoofed header",
			peer: "203.0.113.9:4444",
			// The attacker sets this themselves; it must not be believed.
			forwards: []string{"1.2.3.4"},
			want:     "203.0.113.9",
		},
		{
			name:     "no trusted proxies and no header uses the peer",
			peer:     "203.0.113.9:4444",
			forwards: nil,
			want:     "203.0.113.9",
		},
		{
			name:     "untrusted peer is used even when a trusted list exists",
			trusted:  []string{"10.0.0.0/8"},
			peer:     "203.0.113.9:4444",
			forwards: []string{"1.2.3.4"},
			want:     "203.0.113.9",
		},
		{
			name:     "trusted peer honours a single forwarded client",
			trusted:  []string{"10.0.0.0/8"},
			peer:     "10.1.2.3:4444",
			forwards: []string{"198.51.100.23"},
			want:     "198.51.100.23",
		},
		{
			name:    "chain of trusted proxies resolves to the rightmost untrusted hop",
			trusted: []string{"10.0.0.0/8"},
			peer:    "10.0.0.1:4444",
			// Client -> edge (10.0.0.9) -> inner (10.0.0.1) -> app. The leftmost
			// entry is attacker-supplied garbage; 198.51.100.23 is what the first
			// proxy we trust actually saw.
			forwards: []string{"1.2.3.4, 198.51.100.23, 10.0.0.9"},
			want:     "198.51.100.23",
		},
		{
			name:     "chain split across repeated headers is walked back to front",
			trusted:  []string{"10.0.0.0/8"},
			peer:     "10.0.0.1:4444",
			forwards: []string{"1.2.3.4, 198.51.100.23", "10.0.0.9"},
			want:     "198.51.100.23",
		},
		{
			name:     "all hops trusted falls back to the peer",
			trusted:  []string{"10.0.0.0/8"},
			peer:     "10.0.0.1:4444",
			forwards: []string{"10.0.0.9, 10.0.0.8"},
			want:     "10.0.0.1",
		},
		{
			name:     "trusted peer with no header uses the peer",
			trusted:  []string{"10.0.0.0/8"},
			peer:     "10.0.0.1:4444",
			forwards: nil,
			want:     "10.0.0.1",
		},
		{
			name:     "malformed hop stops the walk at the peer",
			trusted:  []string{"10.0.0.0/8"},
			peer:     "10.0.0.1:4444",
			forwards: []string{"198.51.100.23, not-an-ip, 10.0.0.9"},
			want:     "10.0.0.1",
		},
		{
			name:     "forwarded entry carrying a port is accepted",
			trusted:  []string{"10.0.0.0/8"},
			peer:     "10.0.0.1:4444",
			forwards: []string{"198.51.100.23:51234"},
			want:     "198.51.100.23",
		},
		{
			name:    "ipv6 proxy forwarding an ipv6 client",
			trusted: []string{"2001:db8::/32"},
			peer:    "[2001:db8::1]:4444",
			// 2001:db8::2 is a trusted hop; the client sits outside the prefix.
			forwards: []string{"2606:4700::9, 2001:db8::2"},
			want:     "2606:4700::9",
		},
		{
			name:     "ipv6 chain entirely inside the trusted prefix falls back to the peer",
			trusted:  []string{"2001:db8::/32"},
			peer:     "[2001:db8::1]:4444",
			forwards: []string{"2001:db8::9, 2001:db8::2"},
			want:     "2001:db8::1",
		},
		{
			name:     "ipv4-mapped peer matches an ipv4 cidr",
			trusted:  []string{"10.0.0.0/8"},
			peer:     "[::ffff:10.0.0.1]:4444",
			forwards: []string{"198.51.100.23"},
			want:     "198.51.100.23",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &server{trustedProxies: mustPrefixes(t, tc.trusted...)}
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tc.peer
			for _, f := range tc.forwards {
				r.Header.Add("X-Forwarded-For", f)
			}
			if got := s.resolveClientIP(r); got != tc.want {
				t.Errorf("resolveClientIP() = %q, want %q", got, tc.want)
			}
		})
	}
}

// loginProbe fires a login attempt from a fixed peer with an optional
// X-Forwarded-For header and reports the status code.
//
// Each probe uses a DIFFERENT login. These tests are about the per-IP request
// bucket, and the account lockout — keyed on (login, IP) — would otherwise trip
// first at its own lower threshold and make them measure the wrong mechanism.
// Varying the login isolates the one under test.
var loginProbeSeq int

func loginProbe(t *testing.T, h http.Handler, peer, forwarded string) int {
	t.Helper()
	loginProbeSeq++
	body, err := json.Marshal(loginRequest{
		Email:    fmt.Sprintf("nobody%d@example.com", loginProbeSeq),
		Password: "wrong-password",
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = peer
	if forwarded != "" {
		req.Header.Set("X-Forwarded-For", forwarded)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func newIPHarness(t *testing.T, trusted ...string) http.Handler {
	t.Helper()
	return New(Options{
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:          NewMemStore(),
		TrustedProxies: mustPrefixes(t, trusted...),
	})
}

// TestLoginRateLimitIgnoresSpoofedForwardedFor is the regression test for the
// bypass: without a trusted-proxy list, rotating X-Forwarded-For on every
// request must not hand the caller a fresh login bucket.
func TestLoginRateLimitIgnoresSpoofedForwardedFor(t *testing.T) {
	h := newIPHarness(t)

	const peer = "203.0.113.9:4444"
	for i := 0; i < loginRateLimit; i++ {
		spoof := netip.AddrFrom4([4]byte{1, 2, 3, byte(i)}).String()
		if code := loginProbe(t, h, peer, spoof); code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d = 429, want the bucket to still have room", i+1)
		}
	}
	if code := loginProbe(t, h, peer, "1.2.3.99"); code != http.StatusTooManyRequests {
		t.Errorf("attempt %d = %d, want 429: a spoofed X-Forwarded-For must not reset the bucket",
			loginRateLimit+1, code)
	}
}

// TestLoginRateLimitHonoursTrustedProxy checks the other half: once a proxy is
// trusted, the forwarded client IP is what the limiter keys on, so one abusive
// client cannot lock out everyone else behind the same proxy.
func TestLoginRateLimitHonoursTrustedProxy(t *testing.T) {
	h := newIPHarness(t, "10.0.0.0/8")

	const peer = "10.0.0.1:4444"
	for i := 0; i < loginRateLimit; i++ {
		if code := loginProbe(t, h, peer, "198.51.100.23"); code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d from the abusive client = 429, want room left", i+1)
		}
	}
	if code := loginProbe(t, h, peer, "198.51.100.23"); code != http.StatusTooManyRequests {
		t.Errorf("attempt %d from the abusive client = %d, want 429", loginRateLimit+1, code)
	}
	// A different real client behind the same proxy still gets its own bucket.
	if code := loginProbe(t, h, peer, "198.51.100.77"); code == http.StatusTooManyRequests {
		t.Error("a second forwarded client = 429, want its own bucket")
	}
}

// TestLoginRateLimitProxyChainBucket exercises a multi-hop chain end to end:
// the bucket must follow the rightmost untrusted hop, not the leftmost entry
// the client wrote.
func TestLoginRateLimitProxyChainBucket(t *testing.T) {
	h := newIPHarness(t, "10.0.0.0/8", "192.168.0.0/16")

	const peer = "10.0.0.1:4444"
	// The client rotates the leftmost entry every request; the real hop
	// (198.51.100.23, appended by the edge proxy) stays put, so the bucket must
	// still fill up.
	for i := 0; i < loginRateLimit; i++ {
		spoof := netip.AddrFrom4([4]byte{1, 2, 3, byte(i)}).String()
		chain := spoof + ", 198.51.100.23, 192.168.1.5"
		if code := loginProbe(t, h, peer, chain); code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d = 429, want room left", i+1)
		}
	}
	if code := loginProbe(t, h, peer, "9.9.9.9, 198.51.100.23, 192.168.1.5"); code != http.StatusTooManyRequests {
		t.Errorf("attempt %d = %d, want 429: the chain must resolve to the same client",
			loginRateLimit+1, code)
	}
}
