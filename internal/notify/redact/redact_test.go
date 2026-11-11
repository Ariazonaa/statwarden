package redact

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// A bot token lives in the request PATH for Telegram, a webhook secret in the
// path for Slack and Discord, an ntfy topic is the path. Every one of them is
// carried, verbatim, by the *url.Error net/http returns for a failed request.
const token = "123456:AAH-secret-token"

func TestErrorStripsTheRequestURLOfATransportError(t *testing.T) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"http://127.0.0.1:1/bot"+token+"/sendMessage?key="+token, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, derr := http.DefaultClient.Do(req)
	if derr == nil {
		t.Fatal("expected a dial error")
	}
	wrapped := fmt.Errorf("telegram: sendMessage: %w", derr)
	if !strings.Contains(wrapped.Error(), token) {
		t.Fatalf("precondition: the raw error should carry the token: %v", wrapped)
	}

	got := Error(wrapped)
	if strings.Contains(got.Error(), token) {
		t.Fatalf("redacted error still carries the token: %v", got)
	}
	if !strings.Contains(got.Error(), "http://127.0.0.1:1") {
		t.Fatalf("redacted error lost the host, which is the useful part: %v", got)
	}
	if !strings.HasPrefix(got.Error(), "telegram: sendMessage: ") {
		t.Fatalf("redacted error lost the provider's context: %v", got)
	}
	// The chain survives, so a caller can still ask what kind of failure it was.
	var uerr *url.Error
	if !errors.As(got, &uerr) {
		t.Fatal("redaction broke errors.As on the original *url.Error")
	}
}

func TestErrorStripsAURLThatFailedToParse(t *testing.T) {
	// A control character in a token makes NewRequest fail with a *url.Error of
	// its own, Op "parse", quoting the whole URL.
	_, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"https://api.example.test/bot"+token+"\x7f/send", nil)
	if err == nil {
		t.Fatal("expected a parse error")
	}
	got := Error(fmt.Errorf("telegram: build request: %w", err))
	if strings.Contains(got.Error(), token) {
		t.Fatalf("redacted error still carries the token: %v", got)
	}
}

func TestErrorStripsExtraURLsTheProviderFormattedItself(t *testing.T) {
	hook := "https://hooks.example.test/services/T000/B000/" + token
	err := fmt.Errorf("webhook: POST %s: unexpected status 500", hook)
	got := Error(err, hook, "", "not a url")
	if strings.Contains(got.Error(), token) {
		t.Fatalf("redacted error still carries the token: %v", got)
	}
	if !strings.Contains(got.Error(), "https://hooks.example.test") {
		t.Fatalf("redacted error lost the host: %v", got)
	}
}

func TestErrorLeavesAnErrorWithoutURLsAlone(t *testing.T) {
	err := errors.New("slack: unexpected status 404 Not Found")
	if got := Error(err); got != err {
		t.Fatalf("Error returned a new value for an error with nothing to redact: %v", got)
	}
	if Error(nil) != nil {
		t.Fatal("Error(nil) must stay nil")
	}
}

func TestURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://api.example.test/bot" + token + "/sendMessage", "https://api.example.test/[redacted]"},
		{"https://user:pw@hooks.example.test/x?y=1", "https://hooks.example.test/[redacted]"},
		{"https://ntfy.example.test", "https://ntfy.example.test"},
		{"https://ntfy.example.test/", "https://ntfy.example.test"},
		{"https://ntfy.example.test:8443/topic", "https://ntfy.example.test:8443/[redacted]"},
		{"::not a url", "[redacted URL]"},
	}
	for _, tt := range tests {
		if got := URL(tt.in); got != tt.want {
			t.Errorf("URL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
