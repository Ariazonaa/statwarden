package pushover

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/notify/registry"
)

func TestSelfRegisters(t *testing.T) {
	if _, ok := registry.Get("pushover"); !ok {
		t.Fatal("pushover provider did not self-register")
	}
}

func TestValidate(t *testing.T) {
	n := New()
	tests := []struct {
		name    string
		cfg     string
		wantErr bool
	}{
		{"ok", `{"token":"t","user":"u"}`, false},
		{"ok priority", `{"token":"t","user":"u","priority":2}`, false},
		{"missing token", `{"user":"u"}`, true},
		{"missing user", `{"token":"t"}`, true},
		{"bad priority", `{"token":"t","user":"u","priority":5}`, true},
		{"broken json", `{"token":`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := n.Validate(json.RawMessage(tt.cfg)); (err != nil) != tt.wantErr {
				t.Fatalf("Validate(%s) error = %v, wantErr %v", tt.cfg, err, tt.wantErr)
			}
		})
	}
}

func TestSend(t *testing.T) {
	var gotMethod, gotPath, gotContentType string
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		gotForm, _ = url.ParseQuery(string(body))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":1,"request":"abc"}`))
	}))
	defer srv.Close()

	n := New()
	n.baseURL = srv.URL
	cfg := json.RawMessage(`{"token":"app-token","user":"user-key","device":"phone"}`)
	ev := domain.NotificationEvent{
		Kind: domain.EventKindDown, MonitorID: 1, MonitorName: "API",
		MonitorURL: "https://api.example.com", Status: domain.StatusDown,
		Title: "API is DOWN", Body: "connection refused", At: time.Unix(1700000000, 0),
	}
	if err := n.Send(t.Context(), cfg, ev); err != nil {
		t.Fatalf("Send error = %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/1/messages.json" {
		t.Fatalf("path = %q, want /1/messages.json", gotPath)
	}
	if gotContentType != "application/x-www-form-urlencoded" {
		t.Fatalf("content-type = %q", gotContentType)
	}
	if gotForm.Get("token") != "app-token" || gotForm.Get("user") != "user-key" {
		t.Fatalf("credentials not mapped: %v", gotForm)
	}
	if gotForm.Get("title") != "API is DOWN" || gotForm.Get("message") != "connection refused" {
		t.Fatalf("title/message not mapped: %v", gotForm)
	}
	if gotForm.Get("device") != "phone" {
		t.Fatalf("device = %q", gotForm.Get("device"))
	}
	if gotForm.Get("priority") != "1" {
		t.Fatalf("priority = %q, want 1 (down)", gotForm.Get("priority"))
	}
	if gotForm.Get("url") != "https://api.example.com" {
		t.Fatalf("url = %q", gotForm.Get("url"))
	}
	if gotForm.Get("timestamp") != "1700000000" {
		t.Fatalf("timestamp = %q", gotForm.Get("timestamp"))
	}
}

func TestSendExplicitPriorityZero(t *testing.T) {
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotForm, _ = url.ParseQuery(string(body))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New()
	n.baseURL = srv.URL
	cfg := json.RawMessage(`{"token":"t","user":"u","priority":0}`)
	ev := domain.NotificationEvent{Kind: domain.EventKindDown, Title: "x", Body: "y"}
	if err := n.Send(t.Context(), cfg, ev); err != nil {
		t.Fatalf("Send error = %v", err)
	}
	if gotForm.Get("priority") != "0" {
		t.Fatalf("explicit priority 0 lost: %q", gotForm.Get("priority"))
	}
}

func TestSendNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"status":0,"errors":["application token is invalid"]}`))
	}))
	defer srv.Close()

	n := New()
	n.baseURL = srv.URL
	cfg := json.RawMessage(`{"token":"bad","user":"u"}`)
	if err := n.Send(t.Context(), cfg, domain.NotificationEvent{Kind: domain.EventKindTest}); err == nil {
		t.Fatal("expected error on 400 response")
	}
}
