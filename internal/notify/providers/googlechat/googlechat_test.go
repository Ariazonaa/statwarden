package googlechat

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
	if _, ok := registry.Get("googlechat"); !ok {
		t.Fatal("googlechat provider did not self-register")
	}
}

func TestValidate(t *testing.T) {
	n := New()
	tests := []struct {
		name    string
		cfg     string
		wantErr bool
	}{
		{"ok", `{"webhook_url":"https://chat.googleapis.com/v1/spaces/AAA/messages?key=k&token=t"}`, false},
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
	cfg := json.RawMessage(`{"webhook_url":"` + srv.URL + `"}`)
	ev := domain.NotificationEvent{
		Kind: domain.EventKindDown, MonitorID: 1, MonitorName: "API",
		MonitorURL: "https://api.example.com", Status: domain.StatusDown,
		Title: "API is DOWN", Body: "connection refused",
	}
	if err := n.Send(t.Context(), cfg, ev); err != nil {
		t.Fatalf("Send error = %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want POST", gotMethod)
	}
	if gotContentType != "application/json; charset=UTF-8" {
		t.Fatalf("content-type = %q", gotContentType)
	}
	if len(gotBody.CardsV2) != 1 {
		t.Fatalf("want 1 card, got %d", len(gotBody.CardsV2))
	}
	if gotBody.CardsV2[0].Card.Header.Title != "API is DOWN" {
		t.Fatalf("card title = %q", gotBody.CardsV2[0].Card.Header.Title)
	}
	if gotBody.Text == "" {
		t.Fatal("summary text missing")
	}
	var haveMonitor bool
	for _, s := range gotBody.CardsV2[0].Card.Sections {
		for _, wdg := range s.Widgets {
			if wdg.DecoratedText != nil && wdg.DecoratedText.TopLabel == "Monitor" && wdg.DecoratedText.Text == "API" {
				haveMonitor = true
			}
		}
	}
	if !haveMonitor {
		t.Fatalf("monitor widget missing: %+v", gotBody.CardsV2[0].Card.Sections)
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
