package telegram

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/notify/registry"
)

func TestSelfRegisters(t *testing.T) {
	if _, ok := registry.Get("telegram"); !ok {
		t.Fatal("telegram provider did not self-register")
	}
}

func TestValidate(t *testing.T) {
	n := New()
	tests := []struct {
		name    string
		cfg     string
		wantErr bool
	}{
		{"ok", `{"bot_token":"123:abc","chat_id":"-100987"}`, false},
		{"missing token", `{"chat_id":"1"}`, true},
		{"token without colon", `{"bot_token":"abc","chat_id":"1"}`, true},
		{"missing chat", `{"bot_token":"123:abc"}`, true},
		{"broken json", `{"bot_token":`, true},
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
	var gotPath, gotMethod string
	var gotReq sendMessageRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotReq)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer srv.Close()

	n := New()
	n.baseURL = srv.URL // override public API host for the test
	cfg := json.RawMessage(`{"bot_token":"123:secret","chat_id":"-100987"}`)
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
	if gotPath != "/bot123:secret/sendMessage" {
		t.Fatalf("path = %q, want /bot123:secret/sendMessage", gotPath)
	}
	if gotReq.ChatID != "-100987" {
		t.Fatalf("chat_id = %q", gotReq.ChatID)
	}
	if !strings.Contains(gotReq.Text, "API is DOWN") || !strings.Contains(gotReq.Text, "connection refused") {
		t.Fatalf("text missing title/body: %q", gotReq.Text)
	}
}

func TestSendAPIError(t *testing.T) {
	// Telegram signals failure with a non-2xx status and an "ok:false" body.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"chat not found"}`))
	}))
	defer srv.Close()

	n := New()
	n.baseURL = srv.URL
	cfg := json.RawMessage(`{"bot_token":"123:secret","chat_id":"x"}`)
	err := n.Send(t.Context(), cfg, domain.NotificationEvent{Kind: domain.EventKindTest})
	if err == nil {
		t.Fatal("expected error on 400 response")
	}
	if !strings.Contains(err.Error(), "chat not found") {
		t.Fatalf("error should carry API description, got %v", err)
	}
}

func TestSendNotOK(t *testing.T) {
	// Some errors arrive as HTTP 200 with ok:false.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"description":"bad request"}`))
	}))
	defer srv.Close()

	n := New()
	n.baseURL = srv.URL
	cfg := json.RawMessage(`{"bot_token":"123:secret","chat_id":"x"}`)
	if err := n.Send(t.Context(), cfg, domain.NotificationEvent{Kind: domain.EventKindTest}); err == nil {
		t.Fatal("expected error when ok:false")
	}
}
