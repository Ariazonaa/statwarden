package webhook

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// A webhook URL is often its own credential (a secret path segment, a token in
// the query), so the provider's errors name the host and nothing more — on a
// non-2xx answer as well as on a transport failure.
func TestErrorsDoNotRepeatTheURL(t *testing.T) {
	const secret = "s3cr3t-hook-path"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	for _, target := range []string{
		srv.URL + "/hooks/" + secret + "?token=" + secret, // answers 500
		"http://127.0.0.1:1/hooks/" + secret,              // refuses the connection
	} {
		cfg, _ := json.Marshal(Config{URL: target})
		err := New().Send(t.Context(), cfg, domain.NotificationEvent{Kind: domain.EventKindTest})
		if err == nil {
			t.Fatalf("Send to %s succeeded", target)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error repeats the URL's secret: %v", err)
		}
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
