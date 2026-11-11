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
	"time"
	"unicode/utf8"

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

// recipientServer answers 400 for the numbers in bad and 201 for everyone
// else, and counts the messages each number was sent.
func recipientServer(t *testing.T, bad ...string) (*httptest.Server, func() map[string]int) {
	t.Helper()
	var mu sync.Mutex
	got := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		vals, _ := url.ParseQuery(string(body))
		to := vals.Get("To")
		for _, b := range bad {
			if to == b {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"message":"invalid number","code":21211}`))
				return
			}
		}
		mu.Lock()
		got[to]++
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)
	return srv, func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		out := map[string]int{}
		for k, v := range got {
			out[k] = v
		}
		return out
	}
}

// One bad number must not cost every recipient after it the alert.
//
// The loop returned on the first failure, so a typo in the first entry of the
// list silenced the whole on-call rota — while the channel merely showed as
// "failed", which looks like a provider problem rather than one bad entry.
func TestABadRecipientDoesNotStopTheOthers(t *testing.T) {
	srv, sent := recipientServer(t, "+15550000001")
	n := New()
	n.baseURL = srv.URL
	cfg := json.RawMessage(`{"account_sid":"AC1","auth_token":"t","from":"+1",` +
		`"to":["+15550000001","+15550000002","+15550000003"]}`)
	err := n.Send(t.Context(), cfg, domain.NotificationEvent{Kind: domain.EventKindDown, MonitorID: 1, Title: "API is DOWN"})
	if err == nil {
		t.Fatal("a failed recipient was reported as a successful delivery")
	}
	if !strings.Contains(err.Error(), "+15550000001") || !strings.Contains(err.Error(), "invalid number") {
		t.Errorf("error does not say which recipient failed and why: %v", err)
	}
	got := sent()
	if got["+15550000002"] != 1 || got["+15550000003"] != 1 {
		t.Fatalf("recipients after the bad one = %v, want each sent once", got)
	}
}

// A retry of the same alert must reach only the recipients that missed it.
//
// The dispatcher retries a failed Send (and the outbox retries it again later),
// and every retry used to text the whole list — so one permanently bad number
// meant everyone else got the same SMS up to a dozen times.
func TestARetryDoesNotTextTheSameRecipientTwice(t *testing.T) {
	srv, sent := recipientServer(t, "+15550000001")
	n := New()
	n.baseURL = srv.URL
	cfg := json.RawMessage(`{"account_sid":"AC1","auth_token":"t","from":"+1",` +
		`"to":["+15550000001","+15550000002"]}`)
	ev := domain.NotificationEvent{Kind: domain.EventKindDown, MonitorID: 1, Title: "API is DOWN",
		At: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	for range 3 {
		if err := n.Send(t.Context(), cfg, ev); err == nil {
			t.Fatal("the bad recipient was reported as delivered")
		}
	}
	if got := sent()["+15550000002"]; got != 1 {
		t.Fatalf("the good recipient was texted %d times across retries, want 1", got)
	}

	// A different alert is new news and does reach them again.
	ev.Kind, ev.Title = domain.EventKindUp, "API is UP"
	_ = n.Send(t.Context(), cfg, ev)
	if got := sent()["+15550000002"]; got != 2 {
		t.Fatalf("the next alert reached the good recipient %d times in total, want 2", got)
	}
}

// Truncation counts characters, not bytes. Cutting at a byte offset split a
// multi-byte character and sent invalid UTF-8, which the carrier renders as
// garbage at best.
func TestLongMessagesAreCutOnACharacterBoundary(t *testing.T) {
	msg := buildMessage(domain.NotificationEvent{Title: strings.Repeat("ü", maxSMSLen+10)})
	if !utf8.ValidString(msg) {
		t.Fatal("truncated message is not valid UTF-8")
	}
	if got := utf8.RuneCountInString(msg); got != maxSMSLen {
		t.Fatalf("truncated message is %d characters, want %d", got, maxSMSLen)
	}
	if !strings.HasSuffix(msg, "…") {
		t.Fatal("truncation is not marked")
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
