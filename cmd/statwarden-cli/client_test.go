package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// newTestClient points a client at an httptest server.
func newTestClient(t *testing.T, h http.HandlerFunc) *client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return newClient(clientOptions{BaseURL: srv.URL, APIKey: "test-secret"})
}

func TestClientSendsBearerToken(t *testing.T) {
	var gotAuth, gotAccept, gotUA string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		gotUA = r.Header.Get("User-Agent")
		_, _ = io.WriteString(w, `{"items":[],"total":0,"limit":50,"offset":0}`)
	})

	if _, err := c.ListMonitors(context.Background(), MonitorFilter{}); err != nil {
		t.Fatalf("ListMonitors: %v", err)
	}
	if gotAuth != "Bearer test-secret" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer test-secret")
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q, want application/json", gotAccept)
	}
	if !strings.HasPrefix(gotUA, "statwarden-cli/") {
		t.Errorf("User-Agent = %q, want a statwarden-cli/ prefix", gotUA)
	}
}

func TestClientDecodesListEnvelope(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("status"); got != "down" {
			t.Errorf("status query = %q, want down", got)
		}
		if got := r.URL.Query().Get("limit"); got != "10" {
			t.Errorf("limit query = %q, want 10", got)
		}
		_, _ = io.WriteString(w, `{"items":[{"id":7,"name":"api","type":"http","status":"down","active":true,"url":"https://x"}],"total":42,"limit":10,"offset":0}`)
	})

	list, err := c.ListMonitors(context.Background(), MonitorFilter{Status: "down", Limit: 10})
	if err != nil {
		t.Fatalf("ListMonitors: %v", err)
	}
	if list.Total != 42 {
		t.Errorf("Total = %d, want 42", list.Total)
	}
	if len(list.Items) != 1 || list.Items[0].ID != 7 || list.Items[0].Name != "api" {
		t.Fatalf("Items = %+v, want one monitor id 7 named api", list.Items)
	}
}

func TestClientCreateMonitorPostsBodyAndAccepts201(t *testing.T) {
	var gotBody map[string]any
	var gotMethod, gotPath string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":3,"name":"api","type":"http"}`)
	})

	m, err := c.CreateMonitor(context.Background(), MonitorCreateRequest{Name: "api", Type: "http", Interval: 60})
	if err != nil {
		t.Fatalf("CreateMonitor: %v", err)
	}
	if m.ID != 3 {
		t.Errorf("ID = %d, want 3", m.ID)
	}
	if gotMethod != "POST" || gotPath != "/api/v1/monitors" {
		t.Errorf("request = %s %s, want POST /api/v1/monitors", gotMethod, gotPath)
	}
	if gotBody["name"] != "api" || gotBody["type"] != "http" {
		t.Errorf("body = %v, want name=api type=http", gotBody)
	}
	// Unset optional fields must be omitted so the server applies its own
	// defaults rather than receiving zero values.
	if _, ok := gotBody["active"]; ok {
		t.Error("body carries `active` although --paused was not given; the server would create every monitor paused")
	}
	if _, ok := gotBody["timeout_seconds"]; ok {
		t.Error("body carries `timeout_seconds` although it was not set")
	}
}

func TestClientCreateMonitorSendsExplicitPaused(t *testing.T) {
	var gotBody map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":1}`)
	})

	paused := false
	if _, err := c.CreateMonitor(context.Background(), MonitorCreateRequest{Name: "n", Type: "http", Active: &paused}); err != nil {
		t.Fatalf("CreateMonitor: %v", err)
	}
	if got, ok := gotBody["active"]; !ok || got != false {
		t.Errorf("body active = %v (present %v), want false", got, ok)
	}
}

func TestClientMapsProblemJSON(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"type":"about:blank","title":"Bad Request","status":400,"detail":"unknown monitor type \"nope\""}`)
	})

	_, err := c.CreateMonitor(context.Background(), MonitorCreateRequest{Name: "n", Type: "nope"})
	if err == nil {
		t.Fatal("CreateMonitor: expected an error for a 400 response")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is %T, want *APIError", err)
	}
	if apiErr.Status != 400 {
		t.Errorf("Status = %d, want 400", apiErr.Status)
	}
	// The server's `detail` is the actionable part and must survive verbatim.
	if !strings.Contains(apiErr.Error(), `unknown monitor type "nope"`) {
		t.Errorf("Error() = %q, want it to carry the server's detail", apiErr.Error())
	}
}

func TestAPIErrorExplainsAuthFailures(t *testing.T) {
	tests := []struct {
		name   string
		err    *APIError
		wantIn string
	}{
		{"401 names the key", &APIError{Status: 401, Detail: "unauthorized"}, "revoked or expired"},
		{"403 names scope and role", &APIError{Status: 403, Detail: "insufficient role"}, "`write` scope"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); !strings.Contains(got, tc.wantIn) {
				t.Errorf("Error() = %q, want it to contain %q", got, tc.wantIn)
			}
		})
	}
}

func TestClientHandlesNonJSONErrorBody(t *testing.T) {
	// A reverse proxy in front of the instance may answer with HTML. The CLI
	// must still produce a usable error rather than a decode failure.
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "<html>502 Bad Gateway</html>")
	})

	_, err := c.ListMonitors(context.Background(), MonitorFilter{})
	if statusOf(err) != http.StatusBadGateway {
		t.Fatalf("statusOf(%v) = %d, want 502", err, statusOf(err))
	}
}

func TestClientHandlesNoContent(t *testing.T) {
	var gotPath string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})

	if err := c.SetMonitorActive(context.Background(), 5, false); err != nil {
		t.Fatalf("SetMonitorActive: %v", err)
	}
	if gotPath != "/api/v1/monitors/5/pause" {
		t.Errorf("path = %q, want /api/v1/monitors/5/pause", gotPath)
	}

	if err := c.SetMonitorActive(context.Background(), 5, true); err != nil {
		t.Fatalf("SetMonitorActive resume: %v", err)
	}
	if gotPath != "/api/v1/monitors/5/resume" {
		t.Errorf("path = %q, want /api/v1/monitors/5/resume", gotPath)
	}

	if err := c.DeleteMonitor(context.Background(), 5); err != nil {
		t.Fatalf("DeleteMonitor: %v", err)
	}
	if gotPath != "/api/v1/monitors/5" {
		t.Errorf("path = %q, want /api/v1/monitors/5", gotPath)
	}
}

// TestTransportErrorIsStableAndEnglish pins the message shape for a request
// that never reached the server. The raw error is an *url.Error wrapping an
// operating-system message that is localised to the machine's language, so it
// cannot be the headline: CI greps would break on a German or Japanese runner.
func TestTransportErrorIsStableAndEnglish(t *testing.T) {
	// A port nothing is listening on: closing the server frees its address.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead := srv.URL
	srv.Close()

	c := newClient(clientOptions{BaseURL: dead, APIKey: "test-secret"})
	_, err := c.ListMonitors(context.Background(), MonitorFilter{})
	if err == nil {
		t.Fatal("ListMonitors: expected an error against a closed port")
	}

	var te *transportError
	if !errors.As(err, &te) {
		t.Fatalf("error is %T, want *transportError", err)
	}

	msg := err.Error()
	headline, _, _ := strings.Cut(msg, "\n")

	if !strings.Contains(headline, "cannot reach the Statwarden API") {
		t.Errorf("headline = %q, want the stable English lead-in", headline)
	}
	if !strings.Contains(headline, "connection was refused") {
		t.Errorf("headline = %q, want it to name the refused connection", headline)
	}
	// The old message read:
	//   GET http://x/api/v1/monitors: Get "http://x/api/v1/monitors": dial tcp …
	// naming the URL twice before saying anything useful.
	target := dead + "/api/v1/monitors"
	if got := strings.Count(msg, target); got != 1 {
		t.Errorf("the full URL appears %d times in %q, want exactly 1", got, msg)
	}
	// The OS text is still reachable, just demoted to context.
	if !strings.Contains(msg, "\ndetail: ") {
		t.Errorf("error = %q, want the OS detail kept on a `detail:` line", msg)
	}
	// The underlying error must stay unwrappable for callers that inspect it.
	if !errors.Is(err, errConnRefused) {
		t.Errorf("errors.Is(err, errConnRefused) = false, want the cause preserved: %v", err)
	}
}

func TestTransportErrorReasons(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		wantIn string
	}{
		{"cancelled", context.Canceled, "cancelled"},
		{"deadline", context.DeadlineExceeded, "timed out"},
		{"dns", &net.DNSError{Err: "no such host", Name: "nope.invalid"}, "host name could not be resolved"},
		{"refused", &net.OpError{Op: "dial", Err: errConnRefused}, "connection was refused"},
		{"reset", &net.OpError{Op: "read", Err: errConnReset}, "reset by the peer"},
		{"other network failure", &net.OpError{Op: "dial", Err: errors.New("network is unreachable")}, "connection failed"},
		{"unclassified", errors.New("something else"), "could not be completed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			te := &transportError{Method: "GET", URL: "http://x/api/v1/monitors", Err: tc.err}
			if got := te.Error(); !strings.Contains(got, tc.wantIn) {
				t.Errorf("Error() = %q, want it to contain %q", got, tc.wantIn)
			}
		})
	}
}

// osDetail exists to stop the URL being printed twice.
func TestOSDetailStripsTheURLErrorWrapper(t *testing.T) {
	inner := errors.New("dial tcp 127.0.0.1:9: connection refused")
	wrapped := &url.Error{Op: "Get", URL: "http://x/api/v1/monitors", Err: inner}

	got := osDetail(wrapped)
	if got != inner.Error() {
		t.Errorf("osDetail = %q, want the bare cause %q", got, inner.Error())
	}
	if strings.Contains(got, "http://x/api/v1/monitors") {
		t.Errorf("osDetail = %q, want the repeated URL stripped", got)
	}
}

func TestClientRefusesCrossHostRedirect(t *testing.T) {
	// Redirecting a credential-bearing request to another host must fail
	// loudly rather than be chased.
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request reached the redirect target carrying %q", r.Header.Get("Authorization"))
	}))
	defer elsewhere.Close()

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/api/v1/monitors", http.StatusFound)
	})

	if _, err := c.ListMonitors(context.Background(), MonitorFilter{}); err == nil {
		t.Fatal("ListMonitors: expected an error for a cross-host redirect")
	}
}
