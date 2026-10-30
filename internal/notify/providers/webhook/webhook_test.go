package webhook

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
	if _, ok := registry.Get("webhook"); !ok {
		t.Fatal("webhook provider did not self-register")
	}
}

func TestValidate(t *testing.T) {
	n := New()
	tests := []struct {
		name    string
		cfg     string
		wantErr bool
	}{
		{"ok", `{"url":"https://hooks.example.com/x"}`, false},
		{"missing url", `{}`, true},
		{"bad scheme", `{"url":"ftp://x"}`, true},
		{"not a url", `{"url":"::::"}`, true},
		{"broken json", `{"url":`, true},
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
	var gotBody payload
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New()
	cfg := json.RawMessage(`{"url":"` + srv.URL + `","headers":{"Authorization":"Bearer t"}}`)
	ev := domain.NotificationEvent{
		Kind: domain.EventKindDown, MonitorID: 1, MonitorName: "API",
		Status: domain.StatusDown, Title: "t", Body: "b", At: time.Now(),
	}
	if err := n.Send(t.Context(), cfg, ev); err != nil {
		t.Fatalf("Send error = %v", err)
	}
	if gotBody.MonitorName != "API" || gotBody.Kind != "down" {
		t.Fatalf("unexpected payload: %+v", gotBody)
	}
	if gotAuth != "Bearer t" {
		t.Fatalf("custom header not sent: %q", gotAuth)
	}
}

func TestSendNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	n := New()
	cfg := json.RawMessage(`{"url":"` + srv.URL + `"}`)
	if err := n.Send(t.Context(), cfg, domain.NotificationEvent{Kind: domain.EventKindTest}); err == nil {
		t.Fatal("expected error on 500 response")
	}
}
