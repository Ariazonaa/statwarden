package gotify

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/notify/registry"
)

func TestSelfRegisters(t *testing.T) {
	if _, ok := registry.Get("gotify"); !ok {
		t.Fatal("gotify provider did not self-register")
	}
}

func TestValidate(t *testing.T) {
	n := New()
	tests := []struct {
		name    string
		cfg     string
		wantErr bool
	}{
		{"ok", `{"server_url":"https://gotify.example.com","app_token":"AbCdEf"}`, false},
		{"ok priority", `{"server_url":"https://g","app_token":"t","priority":8}`, false},
		{"missing server", `{"app_token":"t"}`, true},
		{"bad scheme", `{"server_url":"ftp://x","app_token":"t"}`, true},
		{"missing token", `{"server_url":"https://g"}`, true},
		{"priority too high", `{"server_url":"https://g","app_token":"t","priority":42}`, true},
		{"broken json", `{"server_url":`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := n.Validate(json.RawMessage(tt.cfg))
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate(%s) error = %v, wantErr %v", tt.cfg, err, tt.wantErr)
			}
		})
	}
}

func TestSend(t *testing.T) {
	var gotMethod, gotPath, gotContentType, gotKey string
	var gotBody payload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		gotKey = r.Header.Get("X-Gotify-Key")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New()
	cfg := json.RawMessage(`{"server_url":"` + srv.URL + `","app_token":"secrettoken","priority":7}`)
	ev := domain.NotificationEvent{
		Kind: domain.EventKindDown, MonitorName: "API",
		MonitorURL: "https://api.example.com", Status: domain.StatusDown,
		Title: "API is DOWN", Body: "connection refused",
	}
	if err := n.Send(t.Context(), cfg, ev); err != nil {
		t.Fatalf("Send error = %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/message" {
		t.Fatalf("path = %q, want /message", gotPath)
	}
	if gotContentType != "application/json" {
		t.Fatalf("content-type = %q", gotContentType)
	}
	if gotKey != "secrettoken" {
		t.Fatalf("X-Gotify-Key = %q, want secrettoken", gotKey)
	}
	if gotBody.Title != "API is DOWN" {
		t.Fatalf("title = %q", gotBody.Title)
	}
	if gotBody.Priority != 7 {
		t.Fatalf("priority = %d, want 7", gotBody.Priority)
	}
}

func TestSendDefaultPriority(t *testing.T) {
	var gotBody payload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New()
	cfg := json.RawMessage(`{"server_url":"` + srv.URL + `","app_token":"t"}`)
	if err := n.Send(t.Context(), cfg, domain.NotificationEvent{Kind: domain.EventKindTest, Title: "t"}); err != nil {
		t.Fatalf("Send error = %v", err)
	}
	if gotBody.Priority != defaultPriority {
		t.Fatalf("priority = %d, want default %d", gotBody.Priority, defaultPriority)
	}
}

func TestSendNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	n := New()
	cfg := json.RawMessage(`{"server_url":"` + srv.URL + `","app_token":"t"}`)
	if err := n.Send(t.Context(), cfg, domain.NotificationEvent{Kind: domain.EventKindTest}); err == nil {
		t.Fatal("expected error on 401 response")
	}
}
