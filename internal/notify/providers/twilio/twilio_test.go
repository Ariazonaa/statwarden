package twilio

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/notify/registry"
)

func TestSelfRegisters(t *testing.T) {
	if _, ok := registry.Get("twilio"); !ok {
		t.Fatal("twilio provider did not self-register")
	}
}

func TestValidate(t *testing.T) {
	n := New()
	tests := []struct {
		name    string
		cfg     string
		wantErr bool
	}{
		{"ok", `{"account_sid":"AC1","auth_token":"tok","from":"+1","to":["+2"]}`, false},
		{"missing sid", `{"auth_token":"t","from":"+1","to":["+2"]}`, true},
		{"missing token", `{"account_sid":"AC1","from":"+1","to":["+2"]}`, true},
		{"missing from", `{"account_sid":"AC1","auth_token":"t","to":["+2"]}`, true},
		{"no recipients", `{"account_sid":"AC1","auth_token":"t","from":"+1","to":[]}`, true},
		{"empty recipient", `{"account_sid":"AC1","auth_token":"t","from":"+1","to":["  "]}`, true},
		{"broken json", `{"account_sid":`, true},
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
	var mu sync.Mutex
	var gotMethod, gotPath, gotContentType, gotFrom, gotBody, gotUser, gotPass string
	var gotTos []string
	var authOK bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		gotUser, gotPass, authOK = r.BasicAuth()
		body, _ := io.ReadAll(r.Body)
		vals, _ := url.ParseQuery(string(body))
		gotFrom = vals.Get("From")
		gotBody = vals.Get("Body")
		gotTos = append(gotTos, vals.Get("To"))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"sid":"SM1","status":"queued"}`))
	}))
	defer srv.Close()

	n := New()
	n.baseURL = srv.URL // override public API host for the test
	cfg := json.RawMessage(`{"account_sid":"AC123","auth_token":"secrettok","from":"+15550001111","to":["+15550002222","+15550003333"]}`)
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
	if gotPath != "/2010-04-01/Accounts/AC123/Messages.json" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotContentType != "application/x-www-form-urlencoded" {
		t.Fatalf("content-type = %q", gotContentType)
	}
	if !authOK || gotUser != "AC123" || gotPass != "secrettok" {
		t.Fatalf("basic auth = %q:%q ok=%v", gotUser, gotPass, authOK)
	}
	if gotFrom != "+15550001111" {
		t.Fatalf("from = %q", gotFrom)
	}
	if !strings.Contains(gotBody, "API is DOWN") {
		t.Fatalf("body missing title: %q", gotBody)
	}
	sort.Strings(gotTos)
	if len(gotTos) != 2 || gotTos[0] != "+15550002222" || gotTos[1] != "+15550003333" {
		t.Fatalf("recipients = %v, want both numbers", gotTos)
	}
}

func TestSendNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"invalid number","code":21211}`))
	}))
	defer srv.Close()

	n := New()
	n.baseURL = srv.URL
	cfg := json.RawMessage(`{"account_sid":"AC1","auth_token":"t","from":"+1","to":["+2"]}`)
	err := n.Send(t.Context(), cfg, domain.NotificationEvent{Kind: domain.EventKindTest, Title: "t"})
	if err == nil {
		t.Fatal("expected error on 400 response")
	}
	if !strings.Contains(err.Error(), "invalid number") {
		t.Fatalf("error should surface API message: %v", err)
	}
}
