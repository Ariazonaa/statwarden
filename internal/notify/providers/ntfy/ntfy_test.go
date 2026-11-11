package ntfy

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
	if _, ok := registry.Get("ntfy"); !ok {
		t.Fatal("ntfy provider did not self-register")
	}
}

func TestValidate(t *testing.T) {
	n := New()
	tests := []struct {
		name    string
		cfg     string
		wantErr bool
	}{
		{"ok default server", `{"topic":"alerts"}`, false},
		{"ok custom server", `{"server_url":"https://ntfy.example.com","topic":"alerts"}`, false},
		{"missing topic", `{}`, true},
		{"bad server", `{"server_url":"ftp://x","topic":"alerts"}`, true},
		{"token and basic", `{"topic":"a","token":"tk","username":"u"}`, true},
		{"bad priority", `{"topic":"a","priority":9}`, true},
		{"broken json", `{"topic":`, true},
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
	var gotMethod, gotPath, gotTitle, gotPriority, gotTags, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotTitle = r.Header.Get("X-Title")
		gotPriority = r.Header.Get("X-Priority")
		gotTags = r.Header.Get("X-Tags")
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New()
	cfg := json.RawMessage(`{"server_url":"` + srv.URL + `","topic":"alerts","token":"tk-123","tags":["warning"]}`)
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
	if gotPath != "/alerts" {
		t.Fatalf("path = %q, want /alerts", gotPath)
	}
	if gotTitle != "API is DOWN" {
		t.Fatalf("X-Title = %q", gotTitle)
	}
	if gotPriority != "5" {
		t.Fatalf("X-Priority = %q, want 5 (down)", gotPriority)
	}
	if !strings.Contains(gotTags, "red_circle") || !strings.Contains(gotTags, "warning") {
		t.Fatalf("X-Tags = %q, want red_circle + warning", gotTags)
	}
	if gotAuth != "Bearer tk-123" {
		t.Fatalf("auth = %q, want Bearer tk-123", gotAuth)
	}
	if !strings.Contains(gotBody, "connection refused") || !strings.Contains(gotBody, "https://api.example.com") {
		t.Fatalf("body = %q", gotBody)
	}
}

func TestSendBasicAuth(t *testing.T) {
	var user, pass string
	var ok bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok = r.BasicAuth()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New()
	cfg := json.RawMessage(`{"server_url":"` + srv.URL + `","topic":"a","username":"u","password":"p"}`)
	if err := n.Send(t.Context(), cfg, domain.NotificationEvent{Kind: domain.EventKindTest, Title: "hi"}); err != nil {
		t.Fatalf("Send error = %v", err)
	}
	if !ok || user != "u" || pass != "p" {
		t.Fatalf("basic auth = %q/%q ok=%v", user, pass, ok)
	}
}

// The topic is the path, and on a public server knowing it is all it takes to
// read and publish — so it must not appear in the provider's errors.
func TestErrorsDoNotRepeatTheTopic(t *testing.T) {
	const topic = "s3cr3t-topic-name"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	for _, server := range []string{srv.URL, "http://127.0.0.1:1"} {
		cfg := json.RawMessage(`{"server_url":"` + server + `","topic":"` + topic + `"}`)
		err := New().Send(t.Context(), cfg, domain.NotificationEvent{Kind: domain.EventKindTest})
		if err == nil {
			t.Fatalf("Send via %s succeeded", server)
		}
		if strings.Contains(err.Error(), topic) {
			t.Errorf("error repeats the topic: %v", err)
		}
	}
}

// A monitor name is free text, and the title travels as an HTTP header. A
// newline or other control character in it made net/http refuse to build the
// request, so every alert for that monitor failed — retried, and failed again.
func TestControlCharactersInTheTitleDoNotBreakDelivery(t *testing.T) {
	var gotTitle, gotTags string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTitle = r.Header.Get("X-Title")
		gotTags = r.Header.Get("X-Tags")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := json.RawMessage(`{"server_url":"` + srv.URL + `","topic":"a","tags":["ops\nteam"]}`)
	ev := domain.NotificationEvent{Kind: domain.EventKindDown, Title: "API\r\nprod\x00 is DOWN"}
	if err := New().Send(t.Context(), cfg, ev); err != nil {
		t.Fatalf("Send error = %v", err)
	}
	if gotTitle != "API prod is DOWN" {
		t.Errorf("X-Title = %q, want the control characters replaced", gotTitle)
	}
	if strings.ContainsAny(gotTags, "\r\n") || !strings.Contains(gotTags, "ops team") {
		t.Errorf("X-Tags = %q", gotTags)
	}
}

func TestSendNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	n := New()
	cfg := json.RawMessage(`{"server_url":"` + srv.URL + `","topic":"a"}`)
	if err := n.Send(t.Context(), cfg, domain.NotificationEvent{Kind: domain.EventKindTest}); err == nil {
		t.Fatal("expected error on 403 response")
	}
}
