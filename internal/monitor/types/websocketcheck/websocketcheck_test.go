package websocketcheck

import (
	"context"
	"crypto/sha1"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
)

// acceptHandshake writes a valid RFC 6455 101 response for the given key,
// optionally corrupting one aspect to exercise failure paths.
func upgradeHandler(mode string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if mode == "reject" {
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		key := r.Header.Get("Sec-WebSocket-Key")
		sum := sha1.Sum([]byte(key + wsGUID))
		accept := base64.StdEncoding.EncodeToString(sum[:])
		if mode == "badaccept" {
			accept = "totally-wrong"
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		upgradeHdr := "websocket"
		if mode == "badupgrade" {
			upgradeHdr = "h2c"
		}
		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: " + upgradeHdr + "\r\n" +
			"Connection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + accept + "\r\n\r\n")
		_ = buf.Flush()
	}
}

func mustCfg(t *testing.T, c Config) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestRegistration(t *testing.T) {
	c, ok := registry.Get(TypeName)
	if !ok {
		t.Fatal("websocket checker not self-registered")
	}
	td, ok := c.(registry.TimeoutDefaulter)
	if !ok || td.DefaultTimeout() <= 0 {
		t.Fatal("websocket checker must provide a positive default timeout")
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     string
		wantErr bool
	}{
		{"minimal ws", `{"url":"ws://example.com/socket"}`, false},
		{"wss", `{"url":"wss://example.com/socket"}`, false},
		{"with headers", `{"url":"ws://example.com","headers":{"Origin":"https://app"}}`, false},
		{"ignore tls", `{"url":"wss://example.com","ignore_tls_errors":true}`, false},
		{"empty config", ``, true},
		{"empty object", `{}`, true},
		{"http scheme rejected", `{"url":"http://example.com"}`, true},
		{"no host", `{"url":"ws://"}`, true},
		// The url is a display field the read path masks but cannot blank, so a
		// credential belongs in headers, which is redacted wholesale.
		{"credentials in url", `{"url":"wss://user:pass@example.com/socket"}`, true},
		{"username in url", `{"url":"wss://user@example.com/socket"}`, true},
		{"empty header name", `{"url":"ws://example.com","headers":{"":"v"}}`, true},
		{"unknown field", `{"url":"ws://example.com","subprotocol":"chat"}`, true},
		{"invalid json", `{"url":`, true},
	}
	var ch Checker
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ch.Validate(json.RawMessage(tt.cfg))
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate(%s) error = %v, wantErr %v", tt.cfg, err, tt.wantErr)
			}
		})
	}
}

// wsURL rewrites an http(s) test-server URL to its ws(s) equivalent.
func wsURL(httpURL string, secure bool) string {
	if secure {
		return "wss" + strings.TrimPrefix(httpURL, "https")
	}
	return "ws" + strings.TrimPrefix(httpURL, "http")
}

func TestCheckHandshake(t *testing.T) {
	tests := []struct {
		name       string
		mode       string
		wantStatus domain.MonitorStatus
		wantMsgSub string
	}{
		{"successful handshake", "ok", domain.StatusUp, ""},
		{"server rejects upgrade", "reject", domain.StatusDown, "unexpected status code"},
		{"wrong accept key", "badaccept", domain.StatusDown, "Sec-WebSocket-Accept"},
		{"wrong upgrade header", "badupgrade", domain.StatusDown, "Upgrade header"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(upgradeHandler(tt.mode))
			defer srv.Close()

			var ch Checker
			res, err := ch.Check(context.Background(), mustCfg(t, Config{URL: wsURL(srv.URL, false)}))
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			if res.Status != tt.wantStatus {
				t.Fatalf("status = %s (%s), want %s", res.Status, res.Message, tt.wantStatus)
			}
			if tt.wantMsgSub != "" && !strings.Contains(res.Message, tt.wantMsgSub) {
				t.Fatalf("message %q does not contain %q", res.Message, tt.wantMsgSub)
			}
			if res.Latency <= 0 {
				t.Fatal("latency not measured")
			}
		})
	}
}

func TestCheckWSS(t *testing.T) {
	srv := httptest.NewTLSServer(upgradeHandler("ok"))
	defer srv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())

	t.Run("trusted wss captures cert", func(t *testing.T) {
		ch := Checker{rootCAs: pool}
		res, err := ch.Check(context.Background(), mustCfg(t, Config{URL: wsURL(srv.URL, true)}))
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != domain.StatusUp {
			t.Fatalf("status = %s (%s), want up", res.Status, res.Message)
		}
		if res.Cert == nil || res.Cert.FingerprintSHA256 == "" {
			t.Fatalf("wss handshake must capture certificate: %+v", res.Cert)
		}
	})

	t.Run("untrusted wss is down", func(t *testing.T) {
		var ch Checker
		res, err := ch.Check(context.Background(), mustCfg(t, Config{URL: wsURL(srv.URL, true)}))
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != domain.StatusDown {
			t.Fatalf("status = %s, want down for untrusted cert", res.Status)
		}
	})

	t.Run("ignore_tls_errors accepts", func(t *testing.T) {
		var ch Checker
		res, err := ch.Check(context.Background(), mustCfg(t, Config{URL: wsURL(srv.URL, true), IgnoreTLSErrors: true}))
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != domain.StatusUp {
			t.Fatalf("status = %s (%s), want up", res.Status, res.Message)
		}
	})
}

func TestCheckConnectionFailure(t *testing.T) {
	srv := httptest.NewServer(upgradeHandler("ok"))
	url := wsURL(srv.URL, false)
	srv.Close() // refused port

	var ch Checker
	res, err := ch.Check(context.Background(), mustCfg(t, Config{URL: url}))
	if err != nil {
		t.Fatalf("connection failures must be DOWN results, not errors; got %v", err)
	}
	if res.Status != domain.StatusDown || res.Message == "" {
		t.Fatalf("Check(dead server) = %+v, want down with message", res)
	}
}

func TestCheckHonoursContextCancel(t *testing.T) {
	srv := httptest.NewServer(upgradeHandler("ok"))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var ch Checker
	res, err := ch.Check(ctx, mustCfg(t, Config{URL: wsURL(srv.URL, false)}))
	if err != nil {
		t.Fatalf("cancellation must be a DOWN result, not an error; got %v", err)
	}
	if res.Status != domain.StatusDown {
		t.Fatalf("status = %s, want down for cancelled context", res.Status)
	}
}

// A failing assertion must not carry the OBSERVED value into the message.
//
// The message is served by GET /monitors/{id}/heartbeats to every authenticated
// reader, and a writer can aim a monitor at any address the process reaches — so
// an echo here is a read channel for internal services. What the operator needs
// for debugging is which assertion failed and what it EXPECTED, and that is
// their own text. See registry.Checker.Check for the policy.
func TestAFailedAssertionDoesNotEchoTheObservedValue(t *testing.T) {
	const secret = "internal-proxy-v4.corp.example"

	resp := &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: http.Header{}}
	resp.Header.Set("Upgrade", secret)
	if msg := assessHandshake(resp, "key"); msg == "" {
		t.Fatal("a non-websocket Upgrade header reported no failure at all")
	} else if strings.Contains(msg, secret) {
		t.Errorf("the Upgrade header value reached the heartbeat message: %q", msg)
	}

	resp.Header.Set("Upgrade", "websocket")
	resp.Header.Set("Sec-WebSocket-Accept", secret)
	if msg := assessHandshake(resp, "key"); msg == "" {
		t.Fatal("a bad Sec-WebSocket-Accept reported no failure at all")
	} else if strings.Contains(msg, secret) {
		t.Errorf("the Sec-WebSocket-Accept value reached the heartbeat message: %q", msg)
	}

	// The status LINE too: its reason phrase is prose the remote server writes.
	resp = &http.Response{
		StatusCode: http.StatusForbidden,
		Status:     "403 " + secret,
		Header:     http.Header{},
	}
	if msg := assessHandshake(resp, "key"); !strings.Contains(msg, "403") {
		t.Errorf("the message dropped the status code: %q", msg)
	} else if strings.Contains(msg, secret) {
		t.Errorf("the server's reason phrase reached the heartbeat message: %q", msg)
	}
}
