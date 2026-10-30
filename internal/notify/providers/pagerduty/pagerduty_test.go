package pagerduty

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
