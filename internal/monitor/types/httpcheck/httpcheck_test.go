package httpcheck

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
)

// mustCfg marshals a Config for Check calls.
func mustCfg(t *testing.T, c Config) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// runCheck executes one check and fails the test on config errors.
func runCheck(t *testing.T, ch *Checker, cfg json.RawMessage) domain.CheckResult {
	t.Helper()
	res, err := ch.Check(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Check() unexpected error: %v", err)
	}
	return res
}

// serverPool returns a cert pool trusting the httptest TLS server.
func serverPool(t *testing.T, srv *httptest.Server) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return pool
}

// generateClientCert creates a self-signed client certificate and returns it
// as PEM strings plus a pool that trusts it (for server-side verification).
func generateClientCert(t *testing.T) (certPEM, keyPEM string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "statwarden-test-client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool = x509.NewCertPool()
	pool.AddCert(cert)
	return certPEM, keyPEM, pool
}

func TestRegistration(t *testing.T) {
	c, ok := registry.Get(TypeName)
	if !ok {
		t.Fatal("http checker not self-registered")
	}
	if _, passive := c.(registry.PassiveChecker); passive {
		t.Fatal("http checker must not be passive")
	}
	td, ok := c.(registry.TimeoutDefaulter)
	if !ok || td.DefaultTimeout() <= 0 {
		t.Fatal("http checker must provide a positive default timeout")
	}
}

func TestCheckStatusCodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code := 200
		if _, err := fmt.Sscanf(r.URL.Path, "/code/%d", &code); err != nil {
			code = 200
		}
		w.WriteHeader(code)
	}))
	defer srv.Close()

	tests := []struct {
		name       string
		path       string
		accepted   StatusCodeSet
		wantStatus domain.MonitorStatus
	}{
		{"200 default accepted", "/code/200", nil, domain.StatusUp},
		{"204 default accepted", "/code/204", nil, domain.StatusUp},
		{"500 default rejected", "/code/500", nil, domain.StatusDown},
		{"404 default rejected", "/code/404", nil, domain.StatusDown},
		{"401 explicitly accepted", "/code/401", StatusCodeSet{{401, 401}}, domain.StatusUp},
		{"200 rejected when list excludes it", "/code/200", StatusCodeSet{{301, 301}}, domain.StatusDown},
		{"range accepts 204", "/code/204", StatusCodeSet{{200, 299}}, domain.StatusUp},
		{"range rejects 300", "/code/300", StatusCodeSet{{200, 299}}, domain.StatusDown},
		{"multi-range accepts 418", "/code/418", StatusCodeSet{{200, 299}, {418, 418}}, domain.StatusUp},
	}
	var ch Checker
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := mustCfg(t, Config{URL: srv.URL + tt.path, AcceptedStatusCodes: tt.accepted})
			res := runCheck(t, &ch, cfg)
			if res.Status != tt.wantStatus {
				t.Fatalf("Check(%s) status = %s (%s), want %s", tt.path, res.Status, res.Message, tt.wantStatus)
			}
			if res.Latency <= 0 {
				t.Fatal("latency not measured")
			}
		})
	}
}

// TestCheckRangeConfigFromJSON exercises the "200-299,301" string syntax end
// to end, as the API would store it.
func TestCheckRangeConfigFromJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer srv.Close()

	var ch Checker
	cfg := fmt.Sprintf(`{"url":%q,"accepted_status_codes":["200-299,301,418"]}`, srv.URL)
	res := runCheck(t, &ch, json.RawMessage(cfg))
	if res.Status != domain.StatusUp {
		t.Fatalf("418 should be accepted by \"200-299,301,418\"; got %s (%s)", res.Status, res.Message)
	}
}

func TestCheckAssertions(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/body", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello world - all systems OK")
	})
	mux.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"healthy","count":42,"checks":[{"state":"pass"}]}`)
	})
	mux.HandleFunc("/notjson", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html>oops</html>")
	})
	mux.HandleFunc("/header", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Custom", "abc123")
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(60 * time.Millisecond)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tests := []struct {
		name       string
		cfg        Config
		wantStatus domain.MonitorStatus
		wantMsgSub string // substring the DOWN message must contain
	}{
		{"keyword present", Config{URL: srv.URL + "/body", Keyword: "systems OK"}, domain.StatusUp, ""},
		{"keyword missing", Config{URL: srv.URL + "/body", Keyword: "maintenance"}, domain.StatusDown, "keyword"},
		{"keyword absent ok", Config{URL: srv.URL + "/body", KeywordAbsent: "error"}, domain.StatusUp, ""},
		{"keyword absent violated", Config{URL: srv.URL + "/body", KeywordAbsent: "world"}, domain.StatusDown, "forbidden keyword"},
		{"both keywords pass", Config{URL: srv.URL + "/body", Keyword: "hello", KeywordAbsent: "error"}, domain.StatusUp, ""},

		{"json path exists", Config{URL: srv.URL + "/json", JSONPath: "status"}, domain.StatusUp, ""},
		{"json value match", Config{URL: srv.URL + "/json", JSONPath: "status", JSONExpected: "healthy"}, domain.StatusUp, ""},
		{"json number match", Config{URL: srv.URL + "/json", JSONPath: "count", JSONExpected: "42"}, domain.StatusUp, ""},
		{"json nested path", Config{URL: srv.URL + "/json", JSONPath: "checks.0.state", JSONExpected: "pass"}, domain.StatusUp, ""},
		{"json value mismatch", Config{URL: srv.URL + "/json", JSONPath: "status", JSONExpected: "sick"}, domain.StatusDown, "expected value \"sick\""},
		{"json path missing", Config{URL: srv.URL + "/json", JSONPath: "nope.deep"}, domain.StatusDown, "not found"},
		{"json invalid body", Config{URL: srv.URL + "/notjson", JSONPath: "status"}, domain.StatusDown, "not valid JSON"},

		{"header present", Config{URL: srv.URL + "/header", HeaderName: "X-Custom"}, domain.StatusUp, ""},
		{"header missing", Config{URL: srv.URL + "/header", HeaderName: "X-Absent"}, domain.StatusDown, "missing"},
		{"header exact match", Config{URL: srv.URL + "/header", HeaderName: "X-Custom", HeaderValue: "abc123"}, domain.StatusUp, ""},
		{"header exact mismatch", Config{URL: srv.URL + "/header", HeaderName: "X-Custom", HeaderValue: "xyz"}, domain.StatusDown, "expected value \"xyz\""},
		{"header regex match", Config{URL: srv.URL + "/header", HeaderName: "X-Custom", HeaderValueRegex: "^abc[0-9]+$"}, domain.StatusUp, ""},
		{"header regex mismatch", Config{URL: srv.URL + "/header", HeaderName: "X-Custom", HeaderValueRegex: "^[0-9]+$"}, domain.StatusDown, "does not match regex"},

		{"response time within threshold", Config{URL: srv.URL + "/body", MaxResponseTimeMS: 5000}, domain.StatusUp, ""},
		{"response time exceeded", Config{URL: srv.URL + "/slow", MaxResponseTimeMS: 1}, domain.StatusDown, "degraded"},
	}
	var ch Checker
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := runCheck(t, &ch, mustCfg(t, tt.cfg))
			if res.Status != tt.wantStatus {
				t.Fatalf("status = %s (%s), want %s", res.Status, res.Message, tt.wantStatus)
			}
			if tt.wantMsgSub != "" && !strings.Contains(res.Message, tt.wantMsgSub) {
				t.Fatalf("message %q does not contain %q", res.Message, tt.wantMsgSub)
			}
		})
	}
}

func TestCheckRequestShaping(t *testing.T) {
	type seen struct {
		method, body, contentType, userAgent, custom, auth string
		basicUser, basicPass                               string
		basicOK                                            bool
	}
	var got seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		user, pass, ok := r.BasicAuth()
		got = seen{
			method:      r.Method,
			body:        string(body),
			contentType: r.Header.Get("Content-Type"),
			userAgent:   r.Header.Get("User-Agent"),
			custom:      r.Header.Get("X-Api-Key"),
			auth:        r.Header.Get("Authorization"),
			basicUser:   user, basicPass: pass, basicOK: ok,
		}
	}))
	defer srv.Close()

	var ch Checker
	tests := []struct {
		name   string
		cfg    Config
		verify func(t *testing.T)
	}{
		{
			"defaults to GET with Statwarden user agent",
			Config{URL: srv.URL},
			func(t *testing.T) {
				if got.method != http.MethodGet {
					t.Fatalf("method = %s, want GET", got.method)
				}
				if !strings.HasPrefix(got.userAgent, "Statwarden/") {
					t.Fatalf("user agent = %q, want Statwarden default", got.userAgent)
				}
			},
		},
		{
			"POST json body",
			Config{URL: srv.URL, Method: "POST", Body: `{"a":1}`, BodyEncoding: BodyEncodingJSON},
			func(t *testing.T) {
				if got.method != http.MethodPost || got.body != `{"a":1}` {
					t.Fatalf("method/body = %s/%q", got.method, got.body)
				}
				if got.contentType != "application/json" {
					t.Fatalf("content type = %q, want application/json", got.contentType)
				}
			},
		},
		{
			"PUT form body",
			Config{URL: srv.URL, Method: "PUT", Body: "a=1&b=2", BodyEncoding: BodyEncodingForm},
			func(t *testing.T) {
				if got.method != http.MethodPut || got.body != "a=1&b=2" {
					t.Fatalf("method/body = %s/%q", got.method, got.body)
				}
				if got.contentType != "application/x-www-form-urlencoded" {
					t.Fatalf("content type = %q, want form encoding", got.contentType)
				}
			},
		},
		{
			"raw body without content type",
			Config{URL: srv.URL, Method: "POST", Body: "plain payload"},
			func(t *testing.T) {
				if got.body != "plain payload" {
					t.Fatalf("body = %q", got.body)
				}
				if got.contentType != "" {
					t.Fatalf("content type = %q, want none for raw body", got.contentType)
				}
			},
		},
		{
			"DELETE method",
			Config{URL: srv.URL, Method: "DELETE"},
			func(t *testing.T) {
				if got.method != http.MethodDelete {
					t.Fatalf("method = %s, want DELETE", got.method)
				}
			},
		},
		{
			"custom headers and user agent",
			Config{URL: srv.URL, Headers: map[string]string{"X-Api-Key": "secret", "Content-Type": "text/csv"}, UserAgent: "custom-agent/2.0"},
			func(t *testing.T) {
				if got.custom != "secret" {
					t.Fatalf("X-Api-Key = %q", got.custom)
				}
				if got.contentType != "text/csv" {
					t.Fatalf("content type = %q, want header override", got.contentType)
				}
				if got.userAgent != "custom-agent/2.0" {
					t.Fatalf("user agent = %q", got.userAgent)
				}
			},
		},
		{
			"basic auth",
			Config{URL: srv.URL, AuthMethod: AuthBasic, BasicUsername: "alice", BasicPassword: "s3cret"},
			func(t *testing.T) {
				if !got.basicOK || got.basicUser != "alice" || got.basicPass != "s3cret" {
					t.Fatalf("basic auth = %v/%q/%q", got.basicOK, got.basicUser, got.basicPass)
				}
			},
		},
		{
			"bearer token",
			Config{URL: srv.URL, AuthMethod: AuthBearer, BearerToken: "tok123"},
			func(t *testing.T) {
				if got.auth != "Bearer tok123" {
					t.Fatalf("Authorization = %q, want Bearer tok123", got.auth)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got = seen{}
			res := runCheck(t, &ch, mustCfg(t, tt.cfg))
			if res.Status != domain.StatusUp {
				t.Fatalf("status = %s (%s), want up", res.Status, res.Message)
			}
			tt.verify(t)
		})
	}
}

func TestCheckRedirects(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "landed")
	})
	mux.HandleFunc("/hop/", func(w http.ResponseWriter, r *http.Request) {
		var n int
		if _, err := fmt.Sscanf(r.URL.Path, "/hop/%d", &n); err != nil || n <= 1 {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/hop/%d", n-1), http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	boolPtr := func(b bool) *bool { return &b }
	tests := []struct {
		name       string
		cfg        Config
		wantStatus domain.MonitorStatus
		wantMsgSub string
	}{
		{"follows by default", Config{URL: srv.URL + "/hop/3", Keyword: "landed"}, domain.StatusUp, ""},
		{"max redirects exceeded", Config{URL: srv.URL + "/hop/5", MaxRedirects: 2}, domain.StatusDown, "stopped after 2 redirects"},
		{"max redirects sufficient", Config{URL: srv.URL + "/hop/2", MaxRedirects: 5}, domain.StatusUp, ""},
		{"no follow rejects 302 by default", Config{URL: srv.URL + "/hop/1", FollowRedirects: boolPtr(false)}, domain.StatusDown, "unexpected status code"},
		{"no follow with accepted 302", Config{URL: srv.URL + "/hop/1", FollowRedirects: boolPtr(false), AcceptedStatusCodes: StatusCodeSet{{301, 302}}}, domain.StatusUp, ""},
	}
	var ch Checker
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := runCheck(t, &ch, mustCfg(t, tt.cfg))
			if res.Status != tt.wantStatus {
				t.Fatalf("status = %s (%s), want %s", res.Status, res.Message, tt.wantStatus)
			}
			if tt.wantMsgSub != "" && !strings.Contains(res.Message, tt.wantMsgSub) {
				t.Fatalf("message %q does not contain %q", res.Message, tt.wantMsgSub)
			}
		})
	}
}

func TestCheckTLS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "secure ok")
	}))
	defer srv.Close()

	t.Run("untrusted cert is down", func(t *testing.T) {
		var ch Checker // no rootCAs: the httptest cert is untrusted
		res := runCheck(t, &ch, mustCfg(t, Config{URL: srv.URL}))
		if res.Status != domain.StatusDown {
			t.Fatalf("status = %s, want down for untrusted certificate", res.Status)
		}
	})

	t.Run("ignore_tls_errors accepts and captures cert", func(t *testing.T) {
		var ch Checker
		res := runCheck(t, &ch, mustCfg(t, Config{URL: srv.URL, IgnoreTLSErrors: true, Keyword: "secure ok"}))
		if res.Status != domain.StatusUp {
			t.Fatalf("status = %s (%s), want up", res.Status, res.Message)
		}
		if res.Cert == nil || res.Cert.FingerprintSHA256 == "" {
			t.Fatalf("cert not captured on ignore_tls_errors path: %+v", res.Cert)
		}
	})

	t.Run("trusted cert captures cert info", func(t *testing.T) {
		ch := Checker{rootCAs: serverPool(t, srv)}
		res := runCheck(t, &ch, mustCfg(t, Config{URL: srv.URL}))
		if res.Status != domain.StatusUp {
			t.Fatalf("status = %s (%s), want up", res.Status, res.Message)
		}
		if res.Cert == nil || res.Cert.FingerprintSHA256 == "" || res.Cert.ValidTo.IsZero() {
			t.Fatalf("incomplete cert info: %+v", res.Cert)
		}
	})
}

func TestCheckMTLS(t *testing.T) {
	certPEM, keyPEM, clientPool := generateClientCert(t)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "mtls ok")
	}))
	srv.TLS = &tls.Config{
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  clientPool,
	}
	srv.StartTLS()
	defer srv.Close()

	t.Run("with client cert", func(t *testing.T) {
		ch := Checker{rootCAs: serverPool(t, srv)}
		cfg := mustCfg(t, Config{
			URL:              srv.URL,
			TLSClientCertPEM: certPEM,
			TLSClientKeyPEM:  keyPEM,
			Keyword:          "mtls ok",
		})
		res := runCheck(t, &ch, cfg)
		if res.Status != domain.StatusUp {
			t.Fatalf("status = %s (%s), want up", res.Status, res.Message)
		}
		if res.Cert == nil {
			t.Fatal("server cert not captured on mTLS path")
		}
	})

	t.Run("without client cert", func(t *testing.T) {
		ch := Checker{rootCAs: serverPool(t, srv)}
		res := runCheck(t, &ch, mustCfg(t, Config{URL: srv.URL}))
		if res.Status != domain.StatusDown {
			t.Fatalf("status = %s, want down without client certificate", res.Status)
		}
	})
}

func TestCheckHTTPProxy(t *testing.T) {
	// The proxy answers absolute-URI requests itself; the target host does
	// not resolve, so an UP result proves the proxy was used.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !r.URL.IsAbs() {
			http.Error(w, "not a proxy request", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, "via-proxy")
	}))
	defer proxy.Close()

	var ch Checker
	cfg := mustCfg(t, Config{
		URL:      "http://statwarden-proxy-target.invalid/health",
		ProxyURL: proxy.URL,
		Keyword:  "via-proxy",
	})
	res := runCheck(t, &ch, cfg)
	if res.Status != domain.StatusUp {
		t.Fatalf("status = %s (%s), want up through proxy", res.Status, res.Message)
	}
}

func TestCheckBodyLimit(t *testing.T) {
	old := maxBodyBytes
	maxBodyBytes = 64
	t.Cleanup(func() { maxBodyBytes = old })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "needle-early ", strings.Repeat("x", 200), " needle-late")
	}))
	defer srv.Close()

	var ch Checker
	t.Run("keyword within limit", func(t *testing.T) {
		res := runCheck(t, &ch, mustCfg(t, Config{URL: srv.URL, Keyword: "needle-early"}))
		if res.Status != domain.StatusUp {
			t.Fatalf("status = %s (%s), want up", res.Status, res.Message)
		}
	})
	t.Run("keyword beyond limit is not seen", func(t *testing.T) {
		res := runCheck(t, &ch, mustCfg(t, Config{URL: srv.URL, Keyword: "needle-late"}))
		if res.Status != domain.StatusDown {
			t.Fatalf("status = %s, want down: keyword lies beyond the body read limit", res.Status)
		}
	})
}

func TestCheckConnectionFailure(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	url := srv.URL
	srv.Close() // guaranteed-refused local port

	var ch Checker
	res := runCheck(t, &ch, mustCfg(t, Config{URL: url}))
	if res.Status != domain.StatusDown || res.Message == "" {
		t.Fatalf("Check(dead server) = %+v, want down with message", res)
	}
}

func TestCheckHonoursContextTimeout(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already expired, mimicking the engine's per-check deadline

	var ch Checker
	res, err := ch.Check(ctx, mustCfg(t, Config{URL: srv.URL}))
	if err != nil {
		t.Fatalf("timeouts must be DOWN results, not errors; got %v", err)
	}
	if res.Status != domain.StatusDown {
		t.Fatalf("Check(cancelled ctx) status = %s, want down", res.Status)
	}
}

// TestCheckRejectsInvalidConfig ensures Check re-validates stored configs.
func TestCheckRejectsInvalidConfig(t *testing.T) {
	var ch Checker
	if _, err := ch.Check(context.Background(), json.RawMessage(`{"url":"https://e.com","header_name":"X","header_value_regex":"["}`)); err == nil {
		t.Fatal("Check must reject configs with an invalid regex")
	}
	if _, err := ch.Check(context.Background(), json.RawMessage(`{"url":"ftp://e.com"}`)); err == nil {
		t.Fatal("Check must reject configs with an unsupported scheme")
	}
}

// --- credential headers across redirects ---------------------------------------

// The origin rule itself moved to internal/httpx, where the CLI and the
// webhook notifier use the same one — it had been written three times and one
// copy was wrong. Its table lives with it; TestLeavesOrigin there is this test,
// with one expectation deliberately inverted:
//
//	{"https://example.com:8443/a", "https://example.com/b", false, "port is not part of the rule"}
//
// A different port is a different service on the same machine, and net/http's
// own rule ignores it — which is exactly the kind of "close enough" that this
// branch keeps finding credentials leaking through. It now counts as leaving
// the origin. The ordinary http→https upgrade is exempted explicitly, since
// that changes the effective port 80→443 by definition.

// TestRedirectHookStripsCredentialHeaders pins that a configured header is not
// replayed to a host the check was never configured for.
//
// net/http strips only its OWN sensitive set (Authorization, Cookie, …) when a
// redirect leaves the domain, so an X-Api-Key rode along to wherever the target
// pointed: anything able to answer a 302 was handed the credential. The hook is
// driven directly here rather than through two servers, so the assertion is
// about the credential and not about how the test host resolves names.
func TestRedirectHookStripsCredentialHeaders(t *testing.T) {
	var ch Checker
	cfg := Config{
		URL:       "https://origin.example/a",
		Headers:   map[string]string{"X-Api-Key": "super-secret", "Accept": "application/json"},
		UserAgent: "statwarden-test",
	}
	client, err := ch.newClient(cfg)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	newReq := func(raw string) *http.Request {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			t.Fatalf("new request %q: %v", raw, err)
		}
		for name, value := range cfg.Headers {
			req.Header.Set(name, value)
		}
		req.Header.Set("User-Agent", cfg.UserAgent)
		return req
	}
	first := newReq(cfg.URL)

	// A foreign host loses every configured header — they are credential-bearing
	// wholesale, which is why Checker.Secrets lists "headers".
	away := newReq("https://attacker.test/b")
	if err := client.CheckRedirect(away, []*http.Request{first}); err != nil {
		t.Fatalf("CheckRedirect: %v", err)
	}
	if got := away.Header.Get("X-Api-Key"); got != "" {
		t.Fatalf("X-Api-Key survived a cross-origin redirect: %q", got)
	}
	if got := away.Header.Get("Accept"); got != "" {
		t.Fatalf("Accept survived: configured headers are stripped wholesale, got %q", got)
	}
	// User-Agent is modelled separately and is not a secret.
	if got := away.Header.Get("User-Agent"); got != "statwarden-test" {
		t.Fatalf("User-Agent = %q, want it preserved", got)
	}

	// Staying inside the origin keeps them, or every monitor behind an ordinary
	// redirect would quietly lose its credentials.
	for _, target := range []string{"https://origin.example/c", "https://api.origin.example/c"} {
		stay := newReq(target)
		if err := client.CheckRedirect(stay, []*http.Request{first}); err != nil {
			t.Fatalf("CheckRedirect(%s): %v", target, err)
		}
		if got := stay.Header.Get("X-Api-Key"); got != "super-secret" {
			t.Fatalf("%s: X-Api-Key = %q, want it preserved", target, got)
		}
	}
}

// TestAuthorizationIsDroppedOnAnHTTPSDowngrade covers the half of the redirect
// problem net/http does not.
//
// net/http strips Authorization when a redirect leaves the DOMAIN, and an
// https→http hop to the same host does not leave it by that rule. A target
// answering with a redirect to its own http:// URL was therefore handed the
// monitor's basic_password or bearer_token in the clear, over exactly the
// network path the operator chose TLS to protect.
//
// Only the operator's own configured headers were being stripped. Authorization
// was not among them: it is built from basic_password/bearer_token rather than
// declared in headers, so the stripping never saw it.
func TestAuthorizationIsDroppedOnAnHTTPSDowngrade(t *testing.T) {
	var (
		mu  sync.Mutex
		got http.Header
	)
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r.Header.Clone()
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer plain.Close()

	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/next", http.StatusFound)
	}))
	defer tlsSrv.Close()

	ch := Checker{}
	res := runCheck(t, &ch, mustCfg(t, Config{
		URL:             tlsSrv.URL,
		AuthMethod:      "bearer",
		BearerToken:     "secret-token",
		Headers:         map[string]string{"X-Api-Key": "secret-key"},
		IgnoreTLSErrors: true,
		MaxRedirects:    5,
	}))
	if res.Status != domain.StatusUp {
		t.Fatalf("check status = %s (%s), want up — the redirect should still be followed",
			res.Status, res.Message)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, name := range []string{"Authorization", "X-Api-Key"} {
		if v := got.Get(name); v != "" {
			t.Errorf("%s survived an https→http redirect as %q", name, v)
		}
	}
}

// TestTruncatedBodyCannotProveAbsence
//
// Only the first maxBodyBytes of a response is read. A forbidden keyword past
// that point was invisible, so keyword_absent reported UP — the one direction a
// monitoring system must never be wrong in. "I did not find it" and "it is not
// there" are the same statement only when the whole body was read.
func TestTruncatedBodyCannotProveAbsence(t *testing.T) {
	orig := maxBodyBytes
	maxBodyBytes = 64
	defer func() { maxBodyBytes = orig }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// The forbidden word sits past the read limit.
		_, _ = w.Write([]byte(strings.Repeat("a", 200) + "ERROR"))
	}))
	defer srv.Close()

	ch := Checker{}
	res := runCheck(t, &ch, mustCfg(t, Config{URL: srv.URL, KeywordAbsent: "ERROR"}))
	if res.Status != domain.StatusDown {
		t.Fatalf("status = %s (%s), want down: the body was cut short before the "+
			"forbidden keyword, so absence was never established", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "cannot verify") {
		t.Fatalf("message = %q, want it to say the check could not be evaluated", res.Message)
	}
}

// A body that fits is judged normally — the guard must not turn every
// keyword_absent check into a permanent DOWN.
func TestCompleteBodyStillProvesAbsence(t *testing.T) {
	orig := maxBodyBytes
	maxBodyBytes = 64
	defer func() { maxBodyBytes = orig }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("all good"))
	}))
	defer srv.Close()

	ch := Checker{}
	res := runCheck(t, &ch, mustCfg(t, Config{URL: srv.URL, KeywordAbsent: "ERROR"}))
	if res.Status != domain.StatusUp {
		t.Fatalf("status = %s (%s), want up", res.Status, res.Message)
	}
}

// A body exactly at the limit is complete, not truncated: the off-by-one here
// decides between a working check and a monitor that is permanently DOWN.
func TestBodyExactlyAtTheLimitCountsAsComplete(t *testing.T) {
	orig := maxBodyBytes
	maxBodyBytes = 64
	defer func() { maxBodyBytes = orig }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("a", 64)))
	}))
	defer srv.Close()

	ch := Checker{}
	res := runCheck(t, &ch, mustCfg(t, Config{URL: srv.URL, KeywordAbsent: "ERROR"}))
	if res.Status != domain.StatusUp {
		t.Fatalf("status = %s (%s), want up: a body of exactly the limit was read "+
			"in full", res.Status, res.Message)
	}
}

// A truncated body cannot be parsed, and the message has to say that rather than
// blame the JSON.
//
// gjson.ValidBytes on a document cut off mid-structure reports invalid, so the
// check answered "response body is not valid JSON" for a response that is perfectly
// valid and merely larger than the read cap. Wrong in the safe direction — DOWN,
// not UP — and still the wrong reason, which sends the operator after a problem
// that does not exist. assess already threads `truncated` into both keyword
// assertions; the JSON one was the omission.
func TestTruncatedBodyIsNotReportedAsInvalidJSON(t *testing.T) {
	orig := maxBodyBytes
	maxBodyBytes = 64
	defer func() { maxBodyBytes = orig }()

	// Valid JSON, comfortably over the cap.
	big := `{"status":"ok","filler":"` + strings.Repeat("x", 200) + `"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(big))
	}))
	defer srv.Close()

	ch := Checker{}
	res := runCheck(t, &ch, mustCfg(t, Config{URL: srv.URL, JSONPath: "status"}))
	if res.Status != domain.StatusDown {
		t.Fatalf("status = %s, want down: the assertion could not be evaluated", res.Status)
	}
	if strings.Contains(res.Message, "not valid JSON") {
		t.Fatalf("message blames the JSON for a truncated read: %q", res.Message)
	}
	if !strings.Contains(res.Message, "not read in full") {
		t.Fatalf("message = %q, want it to name the read cap as the reason", res.Message)
	}
}

// A configured Host header actually reaches the server.
//
// net/http builds the request line and the Host header from req.Host, never from
// Header, so Header.Set("Host", …) was accepted, stored and silently never sent. A
// monitor pointed at a load balancer to check one particular virtual host was
// therefore checking whatever the URL's host resolves to, while its configuration
// said otherwise — the config was not rejected, it was ignored.
func TestHostHeaderIsHonoured(t *testing.T) {
	var gotHost string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ch := Checker{}
	res := runCheck(t, &ch, mustCfg(t, Config{
		URL:     srv.URL,
		Headers: map[string]string{"Host": "vhost.internal"},
	}))
	if res.Status != domain.StatusUp {
		t.Fatalf("status = %s (%s), want up", res.Status, res.Message)
	}
	if gotHost != "vhost.internal" {
		t.Fatalf("server saw Host %q, want %q — the configured header was dropped",
			gotHost, "vhost.internal")
	}

	// Case-insensitively, because HTTP header names are.
	gotHost = ""
	res = runCheck(t, &ch, mustCfg(t, Config{
		URL:     srv.URL,
		Headers: map[string]string{"host": "lower.internal"},
	}))
	if res.Status != domain.StatusUp {
		t.Fatalf("status = %s (%s), want up", res.Status, res.Message)
	}
	if gotHost != "lower.internal" {
		t.Fatalf("server saw Host %q, want %q", gotHost, "lower.internal")
	}
}

// The failure message must not carry the value it read out of the response.
//
// The message becomes the heartbeat's message, which any authenticated reader
// can fetch — a viewer, a read-only API key — and which internal/outbound copies
// into the state-change webhook payload. Echoing the observed value turned a
// writer-createable check into a full SSRF read channel: point it at an internal
// address, set json_path to the field you want and json_expected to something
// that never matches, trigger it, and read the answer back in clear.
func TestAFailedAssertionDoesNotEchoTheResponseValue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Secret-Header", "header-secret-value")
		_, _ = w.Write([]byte(`{"status":"AKIAIOSFODNN7EXAMPLE"}`))
	}))
	defer srv.Close()

	var ch Checker
	for _, tt := range []struct {
		name   string
		cfg    Config
		secret string
	}{
		{
			"json path",
			Config{URL: srv.URL, JSONPath: "status", JSONExpected: "never"},
			"AKIAIOSFODNN7EXAMPLE",
		},
		{
			"header",
			Config{URL: srv.URL, HeaderName: "X-Secret-Header", HeaderValue: "never"},
			"header-secret-value",
		},
		{
			// The regex branch is a separate return and was left echoing the value
			// when the other two were fixed.
			"header regex",
			Config{URL: srv.URL, HeaderName: "X-Secret-Header", HeaderValueRegex: "^never$"},
			"header-secret-value",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			res := runCheck(t, &ch, mustCfg(t, tt.cfg))
			if res.Status != domain.StatusDown {
				t.Fatalf("status = %s, want down — the assertion must fail for this to prove anything", res.Status)
			}
			if strings.Contains(res.Message, tt.secret) {
				t.Errorf("the failure message carries the value read from the response: %q", res.Message)
			}
			// It still has to be useful to whoever is debugging it: the assertion
			// the operator configured is theirs, and naming it is the point.
			if !strings.Contains(res.Message, "never") {
				t.Errorf("the message does not say what was expected: %q", res.Message)
			}
		})
	}
}

// The HTTP status LINE is prose the remote server writes: RFC 9110 lets it send
// any reason phrase it likes after the code, and Go hands it through verbatim in
// resp.Status. Both places that reported a status used the line rather than the
// code, so a target could write into the heartbeat message on a check that was
// merely PASSING — no assertion needed. Only the number is ours to report.
func TestTheStatusLineReasonPhraseNeverReachesTheMessage(t *testing.T) {
	const secret = "OK; internal-build-4711; db=pg-primary.corp"

	// A raw listener, because httptest always writes Go's canonical phrase.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	code := make(chan int, 1)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			status := <-code
			_, _ = bufio.NewReader(conn).ReadString('\n')
			_, _ = fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", status, secret)
			_ = conn.Close()
		}
	}()

	var ch Checker
	url := "http://" + ln.Addr().String() + "/"

	// Passing: the message is the check's own summary, and it must not be the
	// server's line either.
	code <- 200
	if res := runCheck(t, &ch, mustCfg(t, Config{URL: url})); res.Status != domain.StatusUp {
		t.Fatalf("status = %s, want up", res.Status)
	} else if strings.Contains(res.Message, secret) {
		t.Errorf("a PASSING check published the server's reason phrase: %q", res.Message)
	} else if !strings.Contains(res.Message, "200") {
		t.Errorf("the message dropped the status code: %q", res.Message)
	}

	// Failing the status-code assertion: same rule.
	code <- 403
	if res := runCheck(t, &ch, mustCfg(t, Config{URL: url})); res.Status != domain.StatusDown {
		t.Fatalf("status = %s, want down", res.Status)
	} else if strings.Contains(res.Message, secret) {
		t.Errorf("the failure message carries the server's reason phrase: %q", res.Message)
	} else if !strings.Contains(res.Message, "403") {
		t.Errorf("the message dropped the status code: %q", res.Message)
	}
}
