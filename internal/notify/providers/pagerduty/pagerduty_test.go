package pagerduty

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/notify/registry"
)

func TestSelfRegisters(t *testing.T) {
	if _, ok := registry.Get("pagerduty"); !ok {
		t.Fatal("pagerduty provider did not self-register")
	}
}

func TestValidate(t *testing.T) {
	n := New()
	tests := []struct {
		name    string
		cfg     string
		wantErr bool
	}{
		{"ok", `{"routing_key":"R0123456789ABCDEF0123456789ABCDEF"}`, false},
		{"missing key", `{}`, true},
		{"broken json", `{"routing_key":`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := n.Validate(json.RawMessage(tt.cfg)); (err != nil) != tt.wantErr {
				t.Fatalf("Validate(%s) error = %v, wantErr %v", tt.cfg, err, tt.wantErr)
			}
		})
	}
}

func TestSendTrigger(t *testing.T) {
	var gotMethod, gotPath, gotContentType string
	var gotBody event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"success"}`))
	}))
	defer srv.Close()

	n := New()
	n.baseURL = srv.URL
	cfg := json.RawMessage(`{"routing_key":"R0123456789ABCDEF0123456789ABCDEF"}`)
	ev := domain.NotificationEvent{
		Kind: domain.EventKindDown, MonitorID: 42, MonitorName: "API",
		MonitorURL: "https://api.example.com", Status: domain.StatusDown,
		Title: "API is DOWN", Body: "connection refused", At: time.Unix(1700000000, 0),
	}
	if err := n.Send(t.Context(), cfg, ev); err != nil {
		t.Fatalf("Send error = %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/v2/enqueue" {
		t.Fatalf("path = %q, want /v2/enqueue", gotPath)
	}
	if gotContentType != "application/json" {
		t.Fatalf("content-type = %q", gotContentType)
	}
	if gotBody.EventAction != "trigger" {
		t.Fatalf("event_action = %q, want trigger", gotBody.EventAction)
	}
	if gotBody.DedupKey != "statwarden-monitor-42" {
		t.Fatalf("dedup_key = %q, want statwarden-monitor-42", gotBody.DedupKey)
	}
	if gotBody.Payload == nil {
		t.Fatal("trigger must carry a payload")
	}
	if gotBody.Payload.Severity != "critical" {
		t.Fatalf("severity = %q, want critical", gotBody.Payload.Severity)
	}
	if gotBody.Payload.Summary != "API is DOWN" {
		t.Fatalf("summary = %q", gotBody.Payload.Summary)
	}
}

func TestSendResolve(t *testing.T) {
	var gotBody event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"success"}`))
	}))
	defer srv.Close()

	n := New()
	n.baseURL = srv.URL
	cfg := json.RawMessage(`{"routing_key":"R0123456789ABCDEF0123456789ABCDEF"}`)
	ev := domain.NotificationEvent{Kind: domain.EventKindUp, MonitorID: 42, Title: "API recovered"}
	if err := n.Send(t.Context(), cfg, ev); err != nil {
		t.Fatalf("Send error = %v", err)
	}
	if gotBody.EventAction != "resolve" {
		t.Fatalf("event_action = %q, want resolve", gotBody.EventAction)
	}
	if gotBody.DedupKey != "statwarden-monitor-42" {
		t.Fatalf("dedup_key = %q, want statwarden-monitor-42", gotBody.DedupKey)
	}
	if gotBody.Payload != nil {
		t.Fatalf("resolve must not carry a payload: %+v", gotBody.Payload)
	}
}

// recordingServer captures every Events API request in order.
func recordingServer(t *testing.T) (*httptest.Server, func() []event) {
	t.Helper()
	var mu sync.Mutex
	var got []event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var e event
		_ = json.Unmarshal(body, &e)
		mu.Lock()
		got = append(got, e)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"success"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []event {
		mu.Lock()
		defer mu.Unlock()
		return append([]event(nil), got...)
	}
}

// Each kind of problem is its own incident, and its "back to normal" event
// resolves it.
//
// Everything used to share statwarden-monitor-<id>: a certificate warning, a
// latency anomaly or a flapping notice was folded into the outage incident (or
// opened it), and since only an UP ever resolved, those incidents stayed open
// forever — while an UP after a real outage also closed an unrelated
// certificate warning.
func TestEachEventClassHasItsOwnIncident(t *testing.T) {
	tests := []struct {
		kind   domain.NotificationEventKind
		action string
		key    string
	}{
		{domain.EventKindDown, "trigger", "statwarden-monitor-42"},
		{domain.EventKindUp, "resolve", "statwarden-monitor-42"},
		{domain.EventKindLatencyAnomaly, "trigger", "statwarden-monitor-42-latency"},
		{domain.EventKindLatencyNormal, "resolve", "statwarden-monitor-42-latency"},
		{domain.EventKindFlapping, "trigger", "statwarden-monitor-42-flapping"},
		{domain.EventKindFlappingStable, "resolve", "statwarden-monitor-42-flapping"},
		{domain.EventKindCertExpiry, "trigger", "statwarden-monitor-42-cert"},
		{domain.EventKindMaintenanceStart, "trigger", "statwarden-monitor-42-maintenance"},
		{domain.EventKindMaintenanceEnd, "resolve", "statwarden-monitor-42-maintenance"},
	}
	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			srv, sent := recordingServer(t)
			n := New()
			n.baseURL = srv.URL
			cfg := json.RawMessage(`{"routing_key":"R0123456789ABCDEF0123456789ABCDEF"}`)
			ev := domain.NotificationEvent{Kind: tt.kind, MonitorID: 42, MonitorName: "API", Title: "t"}
			if err := n.Send(t.Context(), cfg, ev); err != nil {
				t.Fatalf("Send error = %v", err)
			}
			got := sent()
			if len(got) != 1 {
				t.Fatalf("requests = %d, want 1", len(got))
			}
			if got[0].EventAction != tt.action || got[0].DedupKey != tt.key {
				t.Fatalf("got %s %q, want %s %q", got[0].EventAction, got[0].DedupKey, tt.action, tt.key)
			}
			if (got[0].EventAction == "trigger") != (got[0].Payload != nil) {
				t.Fatalf("a trigger must carry a payload and a resolve must not: %+v", got[0])
			}
		})
	}
}

// A test send is not an incident. It used to trigger statwarden-monitor-0 —
// shared by every test press and every digest — and leave it open for good.
// It now opens its own incident and resolves it at once, which still proves
// the routing key works end to end.
func TestATestEventResolvesItself(t *testing.T) {
	srv, sent := recordingServer(t)
	n := New()
	n.baseURL = srv.URL
	cfg := json.RawMessage(`{"routing_key":"R0123456789ABCDEF0123456789ABCDEF"}`)
	at := time.Unix(1700000000, 0)
	if err := n.Send(t.Context(), cfg, domain.NotificationEvent{Kind: domain.EventKindTest, Title: "Test", At: at}); err != nil {
		t.Fatalf("Send error = %v", err)
	}
	got := sent()
	if len(got) != 2 || got[0].EventAction != "trigger" || got[1].EventAction != "resolve" {
		t.Fatalf("requests = %+v, want a trigger followed by a resolve", got)
	}
	if got[0].DedupKey != got[1].DedupKey {
		t.Fatalf("the resolve (%q) does not close the trigger (%q)", got[1].DedupKey, got[0].DedupKey)
	}
	if !strings.HasPrefix(got[0].DedupKey, "statwarden-test-") || got[0].DedupKey == "statwarden-monitor-0" {
		t.Fatalf("dedup_key = %q, want a key of its own", got[0].DedupKey)
	}
}

func TestSendNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"status":"invalid event","message":"Event object is invalid"}`))
	}))
	defer srv.Close()

	n := New()
	n.baseURL = srv.URL
	cfg := json.RawMessage(`{"routing_key":"x"}`)
	if err := n.Send(t.Context(), cfg, domain.NotificationEvent{Kind: domain.EventKindDown}); err == nil {
		t.Fatal("expected error on 400 response")
	}
}
