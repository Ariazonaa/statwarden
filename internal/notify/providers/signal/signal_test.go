package signal

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/notify/registry"
)

func TestSelfRegisters(t *testing.T) {
	if _, ok := registry.Get("signal"); !ok {
		t.Fatal("signal provider did not self-register")
	}
}

func TestValidate(t *testing.T) {
	n := New()
	tests := []struct {
		name    string
		cfg     string
		wantErr bool
	}{
		{"ok", `{"server_url":"https://sig.example.com","number":"+491511","recipients":["+491522"]}`, false},
		{"missing server", `{"number":"+1","recipients":["+2"]}`, true},
		{"bad scheme", `{"server_url":"ftp://x","number":"+1","recipients":["+2"]}`, true},
		{"missing number", `{"server_url":"https://s","recipients":["+2"]}`, true},
		{"no recipients", `{"server_url":"https://s","number":"+1","recipients":[]}`, true},
		{"empty recipient", `{"server_url":"https://s","number":"+1","recipients":["  "]}`, true},
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
	var gotMethod, gotPath, gotContentType string
	var gotBody payload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	n := New()
	cfg := json.RawMessage(`{"server_url":"` + srv.URL + `","number":"+491511","recipients":["+491522","+491533"]}`)
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
	if gotPath != "/v2/send" {
		t.Fatalf("path = %q, want /v2/send", gotPath)
	}
	if gotContentType != "application/json" {
		t.Fatalf("content-type = %q", gotContentType)
	}
	if gotBody.Number != "+491511" {
		t.Fatalf("number = %q", gotBody.Number)
	}
	if !reflect.DeepEqual(gotBody.Recipients, []string{"+491522", "+491533"}) {
		t.Fatalf("recipients = %v", gotBody.Recipients)
	}
	if gotBody.Message == "" {
		t.Fatal("message empty")
	}
}

func TestSendNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	n := New()
	cfg := json.RawMessage(`{"server_url":"` + srv.URL + `","number":"+1","recipients":["+2"]}`)
	if err := n.Send(t.Context(), cfg, domain.NotificationEvent{Kind: domain.EventKindTest}); err == nil {
		t.Fatal("expected error on 400 response")
	}
}
