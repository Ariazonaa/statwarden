package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Ariazonaa/statwarden/internal/version"

	"github.com/Ariazonaa/statwarden/internal/httpx"
)

// maxResponseBytes bounds a decoded response body, mirroring the server's own
// 1 MiB request cap. A malformed or hostile endpoint must not be able to
// exhaust the CLI's memory.
const maxResponseBytes = 1 << 20

const defaultTimeout = 30 * time.Second

// client talks to an Statwarden instance over its REST API. It never touches the
// database: everything goes through the same HTTP surface the web UI uses, so
// the CLI works unchanged against a remote instance.
//
// Authentication is a single `Authorization: Bearer` header. API-key principals
// are exempt from the server's CSRF middleware, so there is no cookie, no
// double-submit token and no login round-trip to manage.
type client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// clientOptions configures a client.
type clientOptions struct {
	BaseURL string
	APIKey  string
	Client  *http.Client
	Timeout time.Duration
}

// newClient builds a client. The HTTP client is injectable so tests can point
// it at an httptest server.
func newClient(o clientOptions) *client {
	if o.Timeout <= 0 {
		o.Timeout = defaultTimeout
	}
	if o.Client == nil {
		o.Client = &http.Client{}
	}
	if o.Client.Timeout == 0 {
		o.Client.Timeout = o.Timeout
	}
	// Go strips the Authorization header on a cross-domain redirect, but
	// relying on that implicitly is fragile. A CLI pointed at a hostile or
	// merely misconfigured base URL should fail loudly rather than chase
	// redirects while holding an API key.
	//
	// Refusing only a different HOST left the downgrade open: https://host →
	// http://host is the same host, so the redirect was followed and the key
	// went out unencrypted. httpx.LeavesOrigin covers both. The CLI refuses
	// rather than stripping — a CLI that silently drops its credential just
	// reports a confusing 401.
	if o.Client.CheckRedirect == nil {
		o.Client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) > 0 && httpx.LeavesOrigin(via[0].URL, req.URL) {
				return fmt.Errorf("refusing to follow a redirect that leaves the origin (%s -> %s)",
					via[0].URL.Redacted(), req.URL.Redacted())
			}
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return nil
		}
	}
	return &client{
		baseURL: strings.TrimRight(o.BaseURL, "/"),
		apiKey:  o.APIKey,
		http:    o.Client,
	}
}

// APIError is a decoded RFC 7807 problem+json response.
type APIError struct {
	Status int
	Title  string
	Detail string
}

func (e *APIError) Error() string {
	msg := e.Detail
	if msg == "" {
		msg = e.Title
	}
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	switch e.Status {
	case http.StatusUnauthorized:
		// The generic server text is the single most likely first-run
		// confusion, so name the concrete causes.
		return fmt.Sprintf("%s (401): the API key is unknown, revoked or expired", msg)
	case http.StatusForbidden:
		return fmt.Sprintf("%s (403): the API key lacks the `write` scope, or the owning user has the `viewer` role", msg)
	}
	return fmt.Sprintf("%s (%d)", msg, e.Status)
}

// statusOf returns the HTTP status carried by err, or 0 if err is not an
// *APIError.
func statusOf(err error) int {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status
	}
	return 0
}

// transportError reports a request that never produced an HTTP response: the
// name did not resolve, the connection was refused, TLS failed, or the deadline
// passed.
//
// The headline is a fixed English sentence on purpose. The underlying error is
// an *url.Error wrapping an operating-system message that is LOCALISED to the
// machine's language ("Es konnte keine Verbindung hergestellt werden…" on a
// German Windows box), which makes it useless to match on in CI. It also
// repeats the URL that the *url.Error already names. So the stable, actionable
// sentence leads and the OS text follows as context.
type transportError struct {
	Method string
	URL    string
	Err    error
}

func (e *transportError) Error() string {
	msg := fmt.Sprintf("cannot reach the Statwarden API (%s %s): %s", e.Method, e.URL, e.reason())
	if detail := osDetail(e.Err); detail != "" {
		msg += "\ndetail: " + detail
	}
	return msg
}

func (e *transportError) Unwrap() error { return e.Err }

// reason classifies the failure into a fixed sentence that says what to do
// about it. Every branch returns a constant, so log matching is stable across
// locales and Go versions.
func (e *transportError) reason() string {
	switch {
	case errors.Is(e.Err, context.Canceled):
		return "the request was cancelled"
	case errors.Is(e.Err, context.DeadlineExceeded):
		return "the request timed out; raise --timeout or check the instance is responding"
	}

	var dnsErr *net.DNSError
	if errors.As(e.Err, &dnsErr) {
		return "the host name could not be resolved; check the base URL for a typo"
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(e.Err, &certErr) {
		return "the TLS certificate could not be verified; check the certificate chain and the system trust store"
	}
	var recordErr tls.RecordHeaderError
	if errors.As(e.Err, &recordErr) {
		return "the server did not speak TLS; the base URL says https but the port is serving plain http"
	}
	var netErr net.Error
	if errors.As(e.Err, &netErr) && netErr.Timeout() {
		return "the request timed out; raise --timeout or check the instance is responding"
	}
	if errors.Is(e.Err, errConnRefused) {
		return "the connection was refused; check the instance is running and the base URL host and port are correct"
	}
	if errors.Is(e.Err, errConnReset) {
		return "the connection was reset by the peer"
	}
	var opErr *net.OpError
	if errors.As(e.Err, &opErr) {
		return "the connection failed; check the base URL host and port, and that no firewall or proxy is blocking it"
	}
	return "the request could not be completed"
}

// osDetail strips the *url.Error wrapper so the URL is not repeated in the
// detail line the caller already printed in the headline.
func osDetail(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		err = urlErr.Err
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

// do performs one request. body, when non-nil, is JSON-encoded; out, when
// non-nil, receives the decoded JSON response.
func (c *client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	target := c.baseURL + "/api/v1" + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "statwarden-cli/"+version.Version)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return &transportError{Method: method, URL: target, Err: err}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		_ = resp.Body.Close()
	}()

	limited := io.LimitReader(resp.Body, maxResponseBytes)

	if resp.StatusCode >= 400 {
		return c.redactProblem(decodeProblem(resp.StatusCode, limited))
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(limited).Decode(out); err != nil {
		return fmt.Errorf("decode response from %s %s: %w", method, target, err)
	}
	return nil
}

// redactProblem removes the API key from server-supplied error text.
//
// The CLI goes to some length to keep the key out of argv; that is wasted if a
// server (or a proxy in front of it) reflects the Authorization header into its
// error body and the CLI then prints it to a terminal or a CI log, where it
// outlives the request. The key is already known to whoever sent it back — the
// point is not to widen the audience.
func (c *client) redactProblem(err error) error {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	apiErr.Title = redactSecret(apiErr.Title, c.apiKey)
	apiErr.Detail = redactSecret(apiErr.Detail, c.apiKey)
	return err
}

// redactSecret replaces every occurrence of secret in text. Secrets shorter
// than 8 bytes are left alone: no real API key is that short, and a short
// string would match innocent substrings and mangle the message.
func redactSecret(text, secret string) string {
	if len(secret) < 8 {
		return text
	}
	return strings.ReplaceAll(text, secret, "[redacted API key]")
}

// decodeProblem turns an error response into an *APIError. A body that is not
// problem+json (a reverse proxy's HTML error page, say) still yields a usable
// error carrying the status.
func decodeProblem(status int, body io.Reader) error {
	var p struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	raw, err := io.ReadAll(body)
	if err == nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, &p)
	}
	return &APIError{Status: status, Title: p.Title, Detail: p.Detail}
}
