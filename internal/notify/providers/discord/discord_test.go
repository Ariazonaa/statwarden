package discord

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
	if _, ok := registry.Get("discord"); !ok {
		t.Fatal("discord provider did not self-register")
	}
}

func TestValidate(t *testing.T) {
	n := New()
	tests := []struct {
		name    string
		cfg     string
		wantErr bool
	}{
		{"ok", `{"webhook_url":"https://discord.com/api/webhooks/1/abc"}`, false},
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
		w.WriteHeader(http.StatusNoContent) // Discord returns 204 on success
	}))
	defer srv.Close()

	n := New()
	cfg := json.RawMessage(`{"webhook_url":"` + srv.URL + `","username":"Statwarden","avatar_url":"https://ex/a.png"}`)
	ev := domain.NotificationEvent{
		Kind: domain.EventKindUp, MonitorID: 2, MonitorName: "Web",
		MonitorURL: "https://web.example.com", Status: domain.StatusUp,
		Title: "Web recovered", Body: "back to 200 OK", At: time.Unix(1700000000, 0),
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
	if gotBody.Username != "Statwarden" || gotBody.AvatarURL != "https://ex/a.png" {
		t.Fatalf("config not mapped: %+v", gotBody)
	}
	if len(gotBody.Embeds) != 1 {
		t.Fatalf("want 1 embed, got %d", len(gotBody.Embeds))
	}
	em := gotBody.Embeds[0]
	if em.Color != 0x2ECC71 {
		t.Fatalf("up color = %#x, want 0x2ECC71", em.Color)
	}
	if em.Description != "back to 200 OK" {
		t.Fatalf("body not mapped: %+v", em)
	}
	if em.Timestamp != "2023-11-14T22:13:20Z" {
		t.Fatalf("timestamp = %q", em.Timestamp)
	}
}

func TestSendNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	n := New()
	cfg := json.RawMessage(`{"webhook_url":"` + srv.URL + `"}`)
	if err := n.Send(t.Context(), cfg, domain.NotificationEvent{Kind: domain.EventKindTest}); err == nil {
		t.Fatal("expected error on 400 response")
	}
}
