package teams

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
	if _, ok := registry.Get("teams"); !ok {
		t.Fatal("teams provider did not self-register")
	}
}

func TestValidate(t *testing.T) {
	n := New()
	tests := []struct {
		name    string
		cfg     string
		wantErr bool
	}{
		{"ok", `{"webhook_url":"https://outlook.office.com/webhook/abc"}`, false},
		{"missing url", `{}`, true},
		{"bad scheme", `{"webhook_url":"ftp://x"}`, true},
		{"not a url", `{"webhook_url":"::::"}`, true},
		{"broken json", `{"webhook_url":`, true},
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
	var gotMethod, gotContentType string
	var gotBody messageCard
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New()
	cfg := json.RawMessage(`{"webhook_url":"` + srv.URL + `"}`)
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
	if gotContentType != "application/json" {
		t.Fatalf("content-type = %q", gotContentType)
	}
	if gotBody.Type != "MessageCard" {
		t.Fatalf("@type = %q, want MessageCard", gotBody.Type)
	}
	if gotBody.ThemeColor != "e01e5a" {
		t.Fatalf("down themeColor = %q, want e01e5a", gotBody.ThemeColor)
	}
	if gotBody.Summary != "API is DOWN" || gotBody.Text != "connection refused" {
		t.Fatalf("summary/text not mapped: %+v", gotBody)
	}
	if len(gotBody.Sections) != 1 {
		t.Fatalf("want 1 section, got %d", len(gotBody.Sections))
	}
	var haveMonitor bool
	for _, f := range gotBody.Sections[0].Facts {
		if f.Name == "Monitor" && f.Value == "API" {
			haveMonitor = true
		}
	}
	if !haveMonitor {
		t.Fatalf("monitor fact missing: %+v", gotBody.Sections[0].Facts)
	}
}

func TestSendUpColor(t *testing.T) {
	var gotBody messageCard
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New()
	cfg := json.RawMessage(`{"webhook_url":"` + srv.URL + `"}`)
	ev := domain.NotificationEvent{Kind: domain.EventKindUp, Title: "API recovered"}
	if err := n.Send(t.Context(), cfg, ev); err != nil {
		t.Fatalf("Send error = %v", err)
	}
	if gotBody.ThemeColor != "2eb67d" {
		t.Fatalf("up themeColor = %q, want 2eb67d", gotBody.ThemeColor)
	}
}

func TestSendNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	n := New()
	cfg := json.RawMessage(`{"webhook_url":"` + srv.URL + `"}`)
	if err := n.Send(t.Context(), cfg, domain.NotificationEvent{Kind: domain.EventKindTest}); err == nil {
		t.Fatal("expected error on 500 response")
	}
}
