// Package httpcheck implements the HTTP(S) monitor family: status-code
// lists/ranges, keyword and JSON-path body assertions, response-header and
// response-time assertions, custom methods/bodies/headers, basic/bearer/mTLS
// auth, redirect and TLS toggles, and per-monitor HTTP/SOCKS5 proxies.
// WebSocket handshake checks live in the sibling package websocketcheck.
package httpcheck

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/httpx"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
	"github.com/Ariazonaa/statwarden/internal/monitor/sslcheck"
)

// TypeName is the registry name stored in monitors.type.
const TypeName = "http"

// defaultTimeout is the per-type default when the monitor sets none.
const defaultTimeout = 15 * time.Second

// defaultMaxRedirects caps followed redirects when max_redirects is unset.
const defaultMaxRedirects = 10

// defaultUserAgent identifies Statwarden when user_agent is not configured.
const defaultUserAgent = "Statwarden/1.0 (uptime monitor)"

// maxBodyBytes limits how much of a response body is read for assertions,
// protecting against huge responses. Bodies are truncated at this size and
// assertions run on the truncated prefix. Variable so tests can shrink it.
var maxBodyBytes int64 = 10 << 20 // 10 MiB

// Checker implements registry.Checker for the HTTP(S) family.
type Checker struct {
	// rootCAs augments certificate verification in tests (httptest TLS
	// servers); nil uses the system roots.
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

// Check implements registry.Checker. The engine supplies the timeout via ctx.
// Network-level failures are DOWN results; config errors are error returns.
func (ch *Checker) Check(ctx context.Context, cfg json.RawMessage) (domain.CheckResult, error) {
	c, err := parseConfig(cfg)
	if err != nil {
		return domain.CheckResult{}, err
	}
	if err := c.validate(); err != nil {
		return domain.CheckResult{}, err
	}

	req, err := c.buildRequest(ctx)
	if err != nil {
		return domain.CheckResult{}, err
	}
	client, err := ch.newClient(c)
	if err != nil {
		return domain.CheckResult{}, err
	}
	defer client.CloseIdleConnections()

	start := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(start)
	if err != nil {
		return domain.CheckResult{
			Status:  domain.StatusDown,
			Latency: latency,
			Message: err.Error(),
		}, nil
	}
	defer resp.Body.Close() //nolint:errcheck // nothing useful to do with a close error

	result := domain.CheckResult{
		Latency: latency,
		Cert:    sslcheck.FromTLSState(resp.TLS, time.Now()),
	}

	var (
		body      []byte
		truncated bool
	)
	if c.needsBody() {
		// One byte past the cap, so a body that exactly fills it is
		// distinguishable from one that overflows it. Whether the read was
		// complete decides what the body assertions are allowed to conclude.
		body, err = io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
		if err != nil {
			result.Status = domain.StatusDown
			result.Message = fmt.Sprintf("read response body: %v", err)
			return result, nil
		}
		if int64(len(body)) > maxBodyBytes {
			truncated = true
			body = body[:maxBodyBytes]
		}
	}

	if msg := c.assess(resp, latency, body, truncated); msg != "" {
		result.Status = domain.StatusDown
		result.Message = msg
	} else {
		result.Status = domain.StatusUp
		// The numeric code, not resp.Status: the status LINE carries a reason phrase
		// the remote server writes, which is a small server-controlled text channel
		// into the same heartbeat message every authenticated reader can fetch.
		result.Message = strconv.Itoa(resp.StatusCode)
	}
	return result, nil
}

// buildRequest assembles the request from the config: method, body,
// headers, User-Agent and authentication.
func (c Config) buildRequest(ctx context.Context) (*http.Request, error) {
	method := c.Method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if c.Body != "" {
		body = strings.NewReader(c.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL, body)
	if err != nil {
		return nil, fmt.Errorf("http: build request: %w", err)
	}

	switch c.BodyEncoding {
	case BodyEncodingJSON:
		req.Header.Set("Content-Type", "application/json")
	case BodyEncodingForm:
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for name, value := range c.Headers {
		// Host is the one header net/http does not take from Header at all: the
		// request line and the Host header are built from req.Host (falling back to
		// the URL), so Header.Set("Host", …) was accepted, stored, and never sent.
		// A monitor configured to check a particular virtual host through a load
		// balancer therefore checked whatever the URL's host resolves to instead,
		// while the configuration said otherwise — the config was not wrong, it was
		// ignored. Matched case-insensitively because HTTP header names are.
		//
		// SNI still comes from the URL, as it does for `curl -H 'Host: …'`. Naming a
		// vhost is not the same as reaching a different server, and this only claims
		// the former.
		if strings.EqualFold(name, "Host") {
			req.Host = value
			continue
		}
		req.Header.Set(name, value)
	}
	switch {
	case c.UserAgent != "":
		req.Header.Set("User-Agent", c.UserAgent)
	case req.Header.Get("User-Agent") == "":
		req.Header.Set("User-Agent", defaultUserAgent)
	}

	switch c.AuthMethod {
	case AuthBasic:
		req.SetBasicAuth(c.BasicUsername, c.BasicPassword)
	case AuthBearer:
		req.Header.Set("Authorization", "Bearer "+c.BearerToken)
	}
	return req, nil
}

// newClient builds a per-check client honouring TLS, mTLS, proxy and
// redirect settings. A fresh client per check keeps monitors isolated from
// each other's connection state.
func (ch *Checker) newClient(c Config) (*http.Client, error) {
	tlsCfg := &tls.Config{
		InsecureSkipVerify: c.IgnoreTLSErrors, //nolint:gosec // opt-in per monitor for self-signed targets
		RootCAs:            ch.rootCAs,
	}
	if c.TLSClientCertPEM != "" {
		cert, err := tls.X509KeyPair([]byte(c.TLSClientCertPEM), []byte(c.TLSClientKeyPEM))
		if err != nil {
			return nil, fmt.Errorf("http: load TLS client certificate: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}

	transport := &http.Transport{TLSClientConfig: tlsCfg}
	if c.ProxyURL != "" {
		proxyURL, err := url.Parse(c.ProxyURL)
		if err != nil {
			return nil, fmt.Errorf("http: invalid proxy_url: %w", err)
		}
		// net/http natively supports http, https, socks5 and socks5h
		// proxy URLs (including user:pass@ userinfo).
		transport.Proxy = http.ProxyURL(proxyURL)
	}

	client := &http.Client{Transport: transport}
	if c.FollowRedirects != nil && !*c.FollowRedirects {
		client.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // evaluate the 3xx response itself
		}
	} else {
		maxRedirects := c.MaxRedirects
		if maxRedirects == 0 {
			maxRedirects = defaultMaxRedirects
		}
		// Two classes of credential ride on these requests, and net/http
		// protects neither adequately.
		//
		// Configured request headers are credential-bearing wholesale — that is
		// exactly why Checker.Secrets lists "headers" — and net/http knows
		// nothing about them, so an X-Api-Key was replayed verbatim to whatever
		// host answered the 302.
		//
		// Authorization, built from basic_password or bearer_token, net/http
		// DOES strip — but only when the hop leaves the domain. An https→http
		// redirect to the same host does not leave it by that rule, so the
		// credential went out in the clear over exactly the network path TLS
		// was chosen to protect. It was not in the list below either, because
		// it is not a configured header. httpx treats the downgrade, and a
		// change of port, as leaving the origin.
		custom := make([]string, 0, len(c.Headers))
		for name := range c.Headers {
			custom = append(custom, name)
		}
		client.CheckRedirect = httpx.StripCredentialsOnRedirect(maxRedirects, custom)
	}
	return client, nil
}

// assess runs all configured assertions in order (status code, response
// time, header, keywords, JSON path) and returns the first failure message,
// or "" when the check passes.
func (c Config) assess(resp *http.Response, latency time.Duration, body []byte, truncated bool) string {
	if !c.AcceptedStatusCodes.Match(resp.StatusCode) {
		// The CODE, not resp.Status: the status line's reason phrase is written by
		// the remote server.
		return fmt.Sprintf("unexpected status code %d", resp.StatusCode)
	}
	if c.MaxResponseTimeMS > 0 {
		if threshold := time.Duration(c.MaxResponseTimeMS) * time.Millisecond; latency > threshold {
			return fmt.Sprintf("degraded: response took %d ms, threshold is %d ms",
				latency.Milliseconds(), c.MaxResponseTimeMS)
		}
	}
	if c.HeaderName != "" {
		if msg := c.assessHeader(resp); msg != "" {
			return msg
		}
	}
	if c.Keyword != "" && !bytes.Contains(body, []byte(c.Keyword)) {
		if truncated {
			return fmt.Sprintf("keyword %q not found in the first %d MiB of the response body "+
				"(the rest was not read)", c.Keyword, maxBodyBytes>>20)
		}
		return fmt.Sprintf("keyword %q not found in response body", c.Keyword)
	}
	// A truncated body cannot establish ABSENCE.
	//
	// Only the first 10 MiB is read, and a forbidden keyword past that point was
	// invisible — so the check reported UP, which is the one direction a
	// monitoring system must never be wrong in. "I did not find it" and "it is
	// not there" are the same statement only when the whole body was read.
	//
	// The must-contain assertion above has the opposite asymmetry and is safe:
	// finding the keyword in the prefix proves it is present, and not finding it
	// already reports DOWN.
	if c.KeywordAbsent != "" && truncated && !bytes.Contains(body, []byte(c.KeywordAbsent)) {
		return fmt.Sprintf("cannot verify that %q is absent: the response exceeded %d MiB "+
			"and was not read in full", c.KeywordAbsent, maxBodyBytes>>20)
	}
	if c.KeywordAbsent != "" && bytes.Contains(body, []byte(c.KeywordAbsent)) {
		return fmt.Sprintf("forbidden keyword %q present in response body", c.KeywordAbsent)
	}
	if c.JSONPath != "" {
		// A truncated body cannot be parsed, for the same reason it cannot
		// establish absence above. gjson.ValidBytes on a document cut off
		// mid-structure reports invalid, so this used to answer "response body is
		// not valid JSON" — sending the operator after a JSON problem that does not
		// exist, when the real answer is that the response is larger than the read
		// cap. Wrong in the safe DIRECTION, and still the wrong reason.
		if truncated {
			return fmt.Sprintf("cannot evaluate json path %q: the response exceeded %d MiB "+
				"and was not read in full", c.JSONPath, maxBodyBytes>>20)
		}
		if msg := c.assessJSON(body); msg != "" {
			return msg
		}
	}
	return ""
}

// assessHeader checks presence and (exact or regex) value of HeaderName.
func (c Config) assessHeader(resp *http.Response) string {
	if len(resp.Header.Values(c.HeaderName)) == 0 {
		return fmt.Sprintf("response header %q missing", c.HeaderName)
	}
	// The observed value is deliberately not echoed, for the reason assessJSON
	// gives: this message reaches every authenticated reader through the
	// heartbeat log and the outbound webhook, so quoting what the response said
	// turns a pass/fail check into a read channel.
	got := resp.Header.Get(c.HeaderName)
	if c.HeaderValue != "" && got != c.HeaderValue {
		return fmt.Sprintf("response header %q does not have the expected value %q",
			c.HeaderName, c.HeaderValue)
	}
	if c.HeaderValueRegex != "" {
		// The pattern is compile-checked in validate().
		if !regexp.MustCompile(c.HeaderValueRegex).MatchString(got) {
			return fmt.Sprintf("response header %q does not match regex %q",
				c.HeaderName, c.HeaderValueRegex)
		}
	}
	return ""
}

// assessJSON evaluates the gjson path assertion against the body.
func (c Config) assessJSON(body []byte) string {
	if !gjson.ValidBytes(body) {
		return "response body is not valid JSON"
	}
	value := gjson.GetBytes(body, c.JSONPath)
	if !value.Exists() {
		return fmt.Sprintf("json path %q not found in response", c.JSONPath)
	}
	if c.JSONExpected != "" && value.String() != c.JSONExpected {
		// The OBSERVED value is deliberately not echoed.
		//
		// This message becomes the heartbeat's message, which any authenticated
		// reader can fetch — a viewer, a read-only API key — and which
		// internal/outbound copies into the state-change webhook payload. A
		// writer could therefore point an HTTP monitor at an internal address,
		// set json_path to the field they wanted and json_expected to something
		// that never matches, trigger it with POST /monitors/{id}/check, and read
		// the response back in clear. That is a full SSRF read channel out of a
		// check that is only supposed to report pass or fail.
		//
		// The operator still gets what they need to debug: which path was
		// checked, what was expected, and that it did not match.
		return fmt.Sprintf("json path %q does not have the expected value %q", c.JSONPath, c.JSONExpected)
	}
	return ""
}
