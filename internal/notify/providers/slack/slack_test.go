package slack

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
	if _, ok := registry.Get("slack"); !ok {
		t.Fatal("slack provider did not self-register")
	}
}

func TestValidate(t *testing.T) {
	n := New()
	tests := []struct {
		name    string
		cfg     string
		wantErr bool
	}{
		{"ok", `{"webhook_url":"https://hooks.slack.com/services/T/B/X"}`, false},
		{"missing url", `{}`, true},
		{"bad scheme", `{"webhook_url":"ftp://x"}`, true},
		{"not a url", `{"webhook_url":"::::"}`, true},
		{"broken json", `{"webhook_url":`, true},
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
	var gotMethod, gotContentType string
	var gotBody payload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New()
	cfg := json.RawMessage(`{"webhook_url":"` + srv.URL + `","channel":"#alerts","username":"bot","icon_emoji":":x:"}`)
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
	if gotBody.Channel != "#alerts" || gotBody.Username != "bot" || gotBody.IconEmoji != ":x:" {
		t.Fatalf("config not mapped into payload: %+v", gotBody)
	}
	if len(gotBody.Attachments) != 1 {
		t.Fatalf("want 1 attachment, got %d", len(gotBody.Attachments))
	}
	att := gotBody.Attachments[0]
	if att.Color != "#e01e5a" {
		t.Fatalf("down color = %q, want #e01e5a", att.Color)
	}
	if att.Title != "API is DOWN" || att.Text != "connection refused" {
		t.Fatalf("title/body not mapped: %+v", att)
	}
	if att.Ts != 1700000000 {
		t.Fatalf("ts = %d, want 1700000000", att.Ts)
	}
	var haveMonitor bool
	for _, f := range att.Fields {
		if f.Title == "Monitor" && f.Value == "API" {
			haveMonitor = true
		}
	}
	if !haveMonitor {
		t.Fatalf("monitor field missing: %+v", att.Fields)
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
