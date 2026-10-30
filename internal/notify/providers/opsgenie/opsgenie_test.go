package opsgenie

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
	if _, ok := registry.Get("opsgenie"); !ok {
		t.Fatal("opsgenie provider did not self-register")
	}
}

func TestValidate(t *testing.T) {
	n := New()
	tests := []struct {
		name    string
		cfg     string
		wantErr bool
	}{
		{"ok", `{"api_key":"abc"}`, false},
		{"ok eu", `{"api_key":"abc","region":"eu"}`, false},
		{"missing key", `{}`, true},
		{"bad region", `{"api_key":"abc","region":"asia"}`, true},
		{"broken json", `{"api_key":`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := n.Validate(json.RawMessage(tt.cfg)); (err != nil) != tt.wantErr {
				t.Fatalf("Validate(%s) error = %v, wantErr %v", tt.cfg, err, tt.wantErr)
			}
		})
	}
}

func TestSendCreate(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotBody createAlertRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"result":"Request will be processed"}`))
	}))
	defer srv.Close()

	n := New()
	n.baseURL = srv.URL
	cfg := json.RawMessage(`{"api_key":"secret-key"}`)
	ev := domain.NotificationEvent{
		Kind: domain.EventKindDown, MonitorID: 7, MonitorName: "API",
		MonitorURL: "https://api.example.com", Status: domain.StatusDown,
		Title: "API is DOWN", Body: "connection refused",
	}
	if err := n.Send(t.Context(), cfg, ev); err != nil {
		t.Fatalf("Send error = %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/v2/alerts" {
		t.Fatalf("path = %q, want /v2/alerts", gotPath)
	}
	if gotAuth != "GenieKey secret-key" {
		t.Fatalf("auth = %q, want GenieKey secret-key", gotAuth)
	}
	if gotBody.Alias != "statwarden-monitor-7" {
		t.Fatalf("alias = %q, want statwarden-monitor-7", gotBody.Alias)
	}
	if gotBody.Message != "API is DOWN" {
		t.Fatalf("message = %q", gotBody.Message)
	}
	if gotBody.Priority != "P1" {
		t.Fatalf("priority = %q, want P1", gotBody.Priority)
	}
}

func TestSendClose(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"result":"Request will be processed"}`))
	}))
	defer srv.Close()

	n := New()
	n.baseURL = srv.URL
	cfg := json.RawMessage(`{"api_key":"secret-key"}`)
	ev := domain.NotificationEvent{Kind: domain.EventKindUp, MonitorID: 7, Title: "API recovered"}
	if err := n.Send(t.Context(), cfg, ev); err != nil {
		t.Fatalf("Send error = %v", err)
	}
	if gotPath != "/v2/alerts/statwarden-monitor-7/close" {
		t.Fatalf("path = %q, want /v2/alerts/statwarden-monitor-7/close", gotPath)
	}
	if !strings.Contains(gotQuery, "identifierType=alias") {
		t.Fatalf("query = %q, want identifierType=alias", gotQuery)
	}
}

func TestRegionBaseURL(t *testing.T) {
	n := New()
	if got := n.base(Config{Region: "eu"}); got != baseURLEU {
		t.Fatalf("eu base = %q, want %q", got, baseURLEU)
	}
	if got := n.base(Config{Region: ""}); got != baseURLUS {
		t.Fatalf("default base = %q, want %q", got, baseURLUS)
	}
}

func TestSendNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Could not authenticate"}`))
	}))
	defer srv.Close()

	n := New()
	n.baseURL = srv.URL
	cfg := json.RawMessage(`{"api_key":"bad"}`)
	if err := n.Send(t.Context(), cfg, domain.NotificationEvent{Kind: domain.EventKindDown}); err == nil {
		t.Fatal("expected error on 401 response")
	}
}
