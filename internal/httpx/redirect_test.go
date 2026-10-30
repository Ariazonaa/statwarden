package httpx

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"
	"time"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

func TestLeavesOrigin(t *testing.T) {
	for _, tc := range []struct {
		name     string
		from, to string
		want     bool
		why      string
	}{
		{"identical", "https://api.example.com/a", "https://api.example.com/b", false,
			"a different path on the same origin is the ordinary case"},
		{"subdomain", "https://example.com/", "https://api.example.com/", false,
			"net/http trusts subdomains for Authorization; parity is deliberate"},
		{"different domain", "https://example.com/", "https://evil.test/", true,
			"the obvious case"},
		{"lookalike suffix", "https://example.com/", "https://evilexample.com/", true,
			"a suffix match is not a subdomain"},
		{"https to http, same host", "https://example.com/", "http://example.com/", true,
			"THE bug: net/http sees the same domain and keeps Authorization, " +
				"so the credential goes out in the clear"},
		{"http to https", "http://example.com/", "https://example.com/", false,
			"an upgrade exposes nothing"},
		{"explicit default port", "https://example.com/", "https://example.com:443/", false,
			"the same origin written two ways"},
		{"different port", "https://example.com/", "https://example.com:8443/", true,
			"a different port is a different service; net/http ignores the port"},
		{"scheme case", "HTTPS://example.com/", "https://EXAMPLE.com/", false,
			"scheme and host are case-insensitive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := LeavesOrigin(mustURL(t, tc.from), mustURL(t, tc.to))
			if got != tc.want {
				t.Fatalf("LeavesOrigin(%s, %s) = %v, want %v — %s",
					tc.from, tc.to, got, tc.want, tc.why)
			}
		})
	}
}

func TestLeavesOriginTreatsNilAsUntrusted(t *testing.T) {
	if !LeavesOrigin(nil, mustURL(t, "https://example.com/")) {
		t.Fatal("a nil origin must not be trusted")
	}
	if !LeavesOrigin(mustURL(t, "https://example.com/"), nil) {
		t.Fatal("a nil destination must not be trusted")
	}
}

// The end-to-end property: a redirect that leaves the origin arrives with no
// credentials on it, including the ones net/http would have kept.
func TestStripCredentialsOnRedirect(t *testing.T) {
	var got http.Header
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
	}))
	defer target.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/next", http.StatusFound)
	}))
	defer origin.Close()

	client := &http.Client{
		CheckRedirect: StripCredentialsOnRedirect(5, []string{"X-Api-Key", "User-Agent"}),
	}
	req, err := http.NewRequest(http.MethodGet, origin.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret-token")
	req.Header.Set("Cookie", "session=secret")
	req.Header.Set("X-Api-Key", "secret-key")
	req.Header.Set("User-Agent", "Statwarden-Test")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	for _, name := range []string{"Authorization", "Cookie", "X-Api-Key"} {
		if v := got.Get(name); v != "" {
			t.Errorf("%s survived the redirect as %q", name, v)
		}
	}
	if got.Get("User-Agent") != "Statwarden-Test" {
		t.Errorf("User-Agent = %q, want it kept: it is not a credential and "+
			"dropping it only makes requests harder to attribute", got.Get("User-Agent"))
	}
}

// Within the origin, credentials must survive — the guard must not break the
// ordinary case of a service redirecting to its own canonical path.
func TestSameOriginRedirectKeepsCredentials(t *testing.T) {
	var got string
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/end", http.StatusFound)
	})
	mux.HandleFunc("/end", func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := &http.Client{CheckRedirect: StripCredentialsOnRedirect(5, nil)}
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/start", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer keep-me")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if got != "Bearer keep-me" {
		t.Fatalf("Authorization on a same-origin redirect = %q, want it kept", got)
	}
}

func TestStripCredentialsOnRedirectBoundsTheChain(t *testing.T) {
	hops := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops++
		http.Redirect(w, r, "/again", http.StatusFound)
	}))
	defer srv.Close()

	client := &http.Client{CheckRedirect: StripCredentialsOnRedirect(3, nil)}
	if _, err := client.Get(srv.URL); err == nil { //nolint:bodyclose // never succeeds
		t.Fatal("an endless redirect chain was followed to completion")
	}
	if hops > 4 {
		t.Fatalf("followed %d hops with a limit of 3", hops)
	}
}

func TestSensitiveHeadersCoversWhatNetHTTPStrips(t *testing.T) {
	// If net/http ever grows another one, this list has to grow with it: the
	// whole point is to apply ITS set under a stricter rule.
	for _, name := range []string{"Authorization", "Www-Authenticate", "Cookie", "Cookie2", "Proxy-Authorization"} {
		if !slices.Contains(SensitiveHeaders, name) {
			t.Errorf("%s is missing from SensitiveHeaders", name)
		}
	}
}

// NotifierClient must arrive with the policy already attached, including any
// caller-supplied credential header — that is its entire reason for existing, and
// a nil CheckRedirect silently reverts to net/http's laxer default.
func TestNotifierClientCarriesTheStripPolicy(t *testing.T) {
	c := NotifierClient(3*time.Second, "X-Gotify-Key")
	if c.Timeout != 3*time.Second {
		t.Fatalf("Timeout = %s, want 3s", c.Timeout)
	}
	if c.CheckRedirect == nil {
		t.Fatal("CheckRedirect is nil, so net/http's default applies and an " +
			"https→http hop on the same host keeps the credential")
	}

	// A downgrade on the same host: net/http would not treat this as leaving, so
	// it is the case the policy exists for.
	req := &http.Request{URL: mustURL(t, "http://gotify.example/message"), Header: http.Header{}}
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("X-Gotify-Key", "app-token")
	req.Header.Set("Content-Type", "application/json")
	via := []*http.Request{{URL: mustURL(t, "https://gotify.example/message")}}

	if err := c.CheckRedirect(req, via); err != nil {
		t.Fatalf("CheckRedirect: %v", err)
	}
	for _, h := range []string{"Authorization", "X-Gotify-Key"} {
		if got := req.Header.Get(h); got != "" {
			t.Errorf("%s survived the downgrade as %q", h, got)
		}
	}
	// Non-credential headers must be left alone.
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want it untouched", got)
	}

	// And the chain is bounded.
	long := make([]*http.Request, DefaultMaxRedirects)
	for i := range long {
		long[i] = &http.Request{URL: mustURL(t, "https://gotify.example/")}
	}
	if err := c.CheckRedirect(req, long); err == nil {
		t.Fatalf("a chain of %d redirects was not stopped", DefaultMaxRedirects)
	}
}
