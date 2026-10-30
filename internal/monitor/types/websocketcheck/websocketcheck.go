// Package websocketcheck probes WebSocket endpoints by performing the RFC
// 6455 opening handshake (HTTP/1.1 Upgrade) and verifying the 101 response.
// It is a sibling of httpcheck rather than a config option on it because the
// architecture reserves one type subpackage per monitor type (see
// docs/architecture.md repository layout) and the dynamic monitor form keys
// its fields off the type name.
package websocketcheck

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6455 mandates SHA-1 for Sec-WebSocket-Accept
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
	"github.com/Ariazonaa/statwarden/internal/monitor/sslcheck"
)

// TypeName is the registry name stored in monitors.type.
const TypeName = "websocket"

// defaultTimeout is the per-type default when the monitor sets none.
const defaultTimeout = 15 * time.Second

// wsGUID is the fixed RFC 6455 GUID used to compute Sec-WebSocket-Accept.
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Config is the jsonb monitor configuration for the websocket type.
type Config struct {
	// URL is the ws:// or wss:// endpoint. Required.
	URL string `json:"url"`
	// Headers are extra handshake request headers (cookies, API keys, …).
	Headers map[string]string `json:"headers,omitempty"`
	// IgnoreTLSErrors skips certificate verification for wss:// targets.
	IgnoreTLSErrors bool `json:"ignore_tls_errors,omitempty"`
}

// Checker implements registry.Checker for WebSocket handshake checks.
type Checker struct {
	// rootCAs augments certificate verification in tests; nil uses the
	// system roots.
	rootCAs *x509.CertPool
}

func init() {
	registry.Register(&Checker{})
}

// Type implements registry.Checker.
func (*Checker) Type() string { return TypeName }

// DefaultTimeout implements registry.TimeoutDefaulter.
func (*Checker) DefaultTimeout() time.Duration { return defaultTimeout }

// Validate implements registry.Checker.
func (*Checker) Validate(cfg json.RawMessage) error {
	c, err := parseConfig(cfg)
	if err != nil {
		return err
	}
	return c.validate()
}

// Check implements registry.Checker: it dials the endpoint, performs the
// WebSocket opening handshake and reports UP on a valid 101 response.
func (ch *Checker) Check(ctx context.Context, cfg json.RawMessage) (domain.CheckResult, error) {
	c, err := parseConfig(cfg)
	if err != nil {
		return domain.CheckResult{}, err
	}
	if err := c.validate(); err != nil {
		return domain.CheckResult{}, err
	}
	u, err := url.Parse(c.URL)
	if err != nil {
		return domain.CheckResult{}, fmt.Errorf("websocket: invalid url: %w", err)
	}

	start := time.Now()
	down := func(msg string) (domain.CheckResult, error) {
		return domain.CheckResult{
			Status:  domain.StatusDown,
			Latency: time.Since(start),
			Message: msg,
		}, nil
	}

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", hostPort(u))
	if err != nil {
		return down(err.Error())
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return down(fmt.Sprintf("set deadline: %v", err))
		}
	}

	var tlsState *tls.ConnectionState
	if u.Scheme == "wss" {
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName:         u.Hostname(),
			InsecureSkipVerify: c.IgnoreTLSErrors, //nolint:gosec // opt-in per monitor for self-signed endpoints
			RootCAs:            ch.rootCAs,
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return down(fmt.Sprintf("tls handshake: %v", err))
		}
		state := tlsConn.ConnectionState()
		tlsState = &state
		conn = tlsConn
	}

	req, key, err := c.handshakeRequest(ctx, u)
	if err != nil {
		return domain.CheckResult{}, err
	}
	if err := req.Write(conn); err != nil {
		return down(fmt.Sprintf("send handshake: %v", err))
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	latency := time.Since(start)
	if err != nil {
		return down(fmt.Sprintf("read handshake response: %v", err))
	}
	defer resp.Body.Close() //nolint:errcheck // handshake connection is torn down anyway

	result := domain.CheckResult{
		Latency: latency,
		Cert:    sslcheck.FromTLSState(tlsState, time.Now()),
	}
	if msg := assessHandshake(resp, key); msg != "" {
		result.Status = domain.StatusDown
		result.Message = msg
	} else {
		result.Status = domain.StatusUp
		result.Message = "websocket handshake completed"
	}
	return result, nil
}

// handshakeRequest builds the RFC 6455 upgrade request and returns it with
// the generated Sec-WebSocket-Key.
func (c Config) handshakeRequest(ctx context.Context, u *url.URL) (*http.Request, string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, "", fmt.Errorf("websocket: generate key: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(raw)

	httpURL := *u
	if u.Scheme == "wss" {
		httpURL.Scheme = "https"
	} else {
		httpURL.Scheme = "http"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpURL.String(), nil)
	if err != nil {
		return nil, "", fmt.Errorf("websocket: build request: %w", err)
	}
	for name, value := range c.Headers {
		req.Header.Set(name, value)
	}
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", key)
	req.Header.Set("Sec-WebSocket-Version", "13")
	return req, key, nil
}

// assessHandshake verifies the server's side of the opening handshake and
// returns a failure message, or "" when the handshake is valid.
func assessHandshake(resp *http.Response, key string) string {
	if resp.StatusCode != http.StatusSwitchingProtocols {
		// The CODE, not resp.Status: same reason as httpcheck's, and missed here
		// on the first pass through this very file.
		return fmt.Sprintf("unexpected status code %d in websocket handshake", resp.StatusCode)
	}
	if upgrade := resp.Header.Get("Upgrade"); !strings.EqualFold(upgrade, "websocket") {
		// The header VALUE is server-controlled text and is not echoed — see
		// registry.Checker.Check.
		return "handshake Upgrade header is not \"websocket\""
	}
	sum := sha1.Sum([]byte(key + wsGUID)) //nolint:gosec // RFC 6455 mandates SHA-1 here
	expected := base64.StdEncoding.EncodeToString(sum[:])
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != expected {
		return "invalid Sec-WebSocket-Accept header"
	}
	return ""
}

// hostPort returns u.Host with the scheme default port added when missing.
func hostPort(u *url.URL) string {
	if u.Port() != "" {
		return u.Host
	}
	if u.Scheme == "wss" {
		return net.JoinHostPort(u.Hostname(), "443")
	}
	return net.JoinHostPort(u.Hostname(), "80")
}

// parseConfig decodes the jsonb blob, rejecting unknown fields.
func parseConfig(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, fmt.Errorf("websocket: empty config")
	}
	dec := json.NewDecoder(bytes.NewReader(cfg))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("websocket: invalid config: %w", err)
	}
	return c, nil
}

// validate performs the semantic checks behind Validate; no I/O.
func (c Config) validate() error {
	if c.URL == "" {
		return fmt.Errorf("websocket: %q is required", "url")
	}
	u, err := url.Parse(c.URL)
	switch {
	case err != nil:
		return fmt.Errorf("websocket: invalid url: %w", err)
	case u.Scheme != "ws" && u.Scheme != "wss":
		return fmt.Errorf("websocket: url must start with ws:// or wss://, got %q", c.URL)
	case u.Host == "":
		return fmt.Errorf("websocket: url %q has no host", c.URL)
	case u.User != nil:
		// Same rule, and same reason, as httpcheck: url is the monitor's display
		// target, returned as config.url and again as the derived view.url, so a
		// credential inside it is one the read path cannot redact without
		// blanking what the monitor even checks. headers is redacted wholesale
		// and is where an Authorization value belongs.
		return fmt.Errorf("websocket: url must not carry credentials; put them in %q instead", "headers")
	}
	for name := range c.Headers {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("websocket: request header with empty name")
		}
	}
	return nil
}
