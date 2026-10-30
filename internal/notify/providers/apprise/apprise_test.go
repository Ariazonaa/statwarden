package apprise

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
	if _, ok := registry.Get("apprise"); !ok {
		t.Fatal("apprise provider did not self-register")
	}
}

func TestValidate(t *testing.T) {
	n := New()
	tests := []struct {
		name    string
		cfg     string
		wantErr bool
	}{
		{"ok urls", `{"server_url":"https://apprise.example.com","urls":["mailto://a:b@c"]}`, false},
		{"ok config_key", `{"server_url":"https://a","config_key":"team"}`, false},
		{"missing server", `{"urls":["x://y"]}`, true},
		{"bad scheme", `{"server_url":"ftp://x","urls":["a://b"]}`, true},
		{"neither urls nor key", `{"server_url":"https://a"}`, true},
		{"both urls and key", `{"server_url":"https://a","urls":["a://b"],"config_key":"k"}`, true},
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

func TestSendStateless(t *testing.T) {
	var gotMethod, gotPath, gotContentType string
	var gotBody payload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New()
	cfg := json.RawMessage(`{"server_url":"` + srv.URL + `","urls":["mailto://a:b@c","tgram://tok/chat"],"tag":"ops"}`)
	ev := domain.NotificationEvent{
		Kind: domain.EventKindDown, MonitorName: "API",
		Status: domain.StatusDown, Title: "API is DOWN", Body: "connection refused",
	}
	if err := n.Send(t.Context(), cfg, ev); err != nil {
		t.Fatalf("Send error = %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/notify" {
		t.Fatalf("path = %q, want /notify", gotPath)
	}
	if gotContentType != "application/json" {
		t.Fatalf("content-type = %q", gotContentType)
	}
	if gotBody.URLs != "mailto://a:b@c,tgram://tok/chat" {
		t.Fatalf("urls = %q", gotBody.URLs)
	}
	if gotBody.Tag != "ops" {
		t.Fatalf("tag = %q", gotBody.Tag)
	}
	if gotBody.Type != "failure" {
		t.Fatalf("type = %q, want failure", gotBody.Type)
	}
	if gotBody.Title != "API is DOWN" {
		t.Fatalf("title = %q", gotBody.Title)
	}
}

func TestSendConfigKey(t *testing.T) {
	var gotPath string
	var gotBody payload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New()
	cfg := json.RawMessage(`{"server_url":"` + srv.URL + `","config_key":"team1","tag":"all"}`)
	if err := n.Send(t.Context(), cfg, domain.NotificationEvent{Kind: domain.EventKindUp, Title: "up"}); err != nil {
		t.Fatalf("Send error = %v", err)
	}
	if gotPath != "/notify/team1" {
		t.Fatalf("path = %q, want /notify/team1", gotPath)
	}
	if gotBody.URLs != "" {
		t.Fatalf("urls should be empty for config_key send, got %q", gotBody.URLs)
	}
	if gotBody.Type != "success" {
		t.Fatalf("type = %q, want success", gotBody.Type)
	}
}

func TestSendNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	n := New()
	cfg := json.RawMessage(`{"server_url":"` + srv.URL + `","urls":["a://b"]}`)
	if err := n.Send(t.Context(), cfg, domain.NotificationEvent{Kind: domain.EventKindTest}); err == nil {
		t.Fatal("expected error on 500 response")
	}
}
