package smtp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/notify/registry"
)

func TestSelfRegisters(t *testing.T) {
	if _, ok := registry.Get("smtp"); !ok {
		t.Fatal("smtp provider did not self-register")
	}
}

func TestValidate(t *testing.T) {
	n := New()
	tests := []struct {
		name    string
		cfg     string
		wantErr bool
	}{
		{"ok", `{"host":"mail.example.com","from":"a@x.io","to":["b@x.io"]}`, false},
		{"ok explicit tls", `{"host":"mail.example.com","port":465,"from":"a@x.io","to":["b@x.io"],"encryption":"tls"}`, false},
		{"missing host", `{"from":"a@x.io","to":["b@x.io"]}`, true},
		{"missing from", `{"host":"mail.example.com","to":["b@x.io"]}`, true},
		{"bad from", `{"host":"mail.example.com","from":"not-an-addr","to":["b@x.io"]}`, true},
		{"no recipients", `{"host":"mail.example.com","from":"a@x.io","to":[]}`, true},
		{"bad recipient", `{"host":"mail.example.com","from":"a@x.io","to":["nope"]}`, true},
		{"bad port", `{"host":"mail.example.com","port":70000,"from":"a@x.io","to":["b@x.io"]}`, true},
		{"bad encryption", `{"host":"mail.example.com","from":"a@x.io","to":["b@x.io"],"encryption":"rot13"}`, true},
		{"broken json", `{"host":`, true},
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

func TestSendCapturesMessage(t *testing.T) {
	var (
		gotCfg Config
		gotMsg []byte
	)
	n := New()
	n.send = func(_ context.Context, c Config, msg []byte) error {
		gotCfg = c
		gotMsg = msg
		return nil
	}

	cfg := json.RawMessage(`{"host":"mail.example.com","username":"u","password":"p","from":"alerts@x.io","to":["ops@x.io","dev@x.io"]}`)
	ev := domain.NotificationEvent{
		Kind: domain.EventKindDown, MonitorName: "API", Status: domain.StatusDown,
		Title: "API is DOWN", Body: "connection refused\nretrying",
	}
	if err := n.Send(t.Context(), cfg, ev); err != nil {
		t.Fatalf("Send error = %v", err)
	}
	if gotCfg.Host != "mail.example.com" || gotCfg.Username != "u" {
		t.Fatalf("config not passed through: %+v", gotCfg)
	}
	msg := string(gotMsg)
	for _, want := range []string{
		"From: alerts@x.io\r\n",
		"To: ops@x.io, dev@x.io\r\n",
		"Subject: API is DOWN\r\n",
		"Content-Type: text/plain; charset=UTF-8\r\n",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message missing %q\n---\n%s", want, msg)
		}
	}
	// Body appears after the blank header/body separator with CRLF endings.
	if !strings.Contains(msg, "\r\n\r\nconnection refused\r\nretrying") {
		t.Fatalf("body not rendered with CRLF: %q", msg)
	}
}

func TestSendHeaderInjectionSanitized(t *testing.T) {
	var gotMsg []byte
	n := New()
	n.send = func(_ context.Context, _ Config, msg []byte) error {
		gotMsg = msg
		return nil
	}
	cfg := json.RawMessage(`{"host":"h","from":"a@x.io","to":["b@x.io"]}`)
	ev := domain.NotificationEvent{Title: "Evil\r\nBcc: attacker@x.io", Body: "hi"}
	if err := n.Send(t.Context(), cfg, ev); err != nil {
		t.Fatalf("Send error = %v", err)
	}
	if strings.Contains(string(gotMsg), "\r\nBcc:") {
		t.Fatalf("CRLF injection produced a Bcc header line: %q", gotMsg)
	}
	if !strings.Contains(string(gotMsg), "Subject: EvilBcc: attacker@x.io\r\n") {
		t.Fatalf("subject not collapsed onto one line: %q", gotMsg)
	}
}

func TestSendPropagatesError(t *testing.T) {
	n := New()
	n.send = func(_ context.Context, _ Config, _ []byte) error {
		return errors.New("connection refused")
	}
	cfg := json.RawMessage(`{"host":"h","from":"a@x.io","to":["b@x.io"]}`)
	if err := n.Send(t.Context(), cfg, domain.NotificationEvent{Kind: domain.EventKindTest}); err == nil {
		t.Fatal("expected error to propagate from send")
	}
}

func TestEffectivePort(t *testing.T) {
	if p := (Config{}).effectivePort(); p != 587 {
		t.Fatalf("default port = %d, want 587", p)
	}
	if p := (Config{Encryption: "tls"}).effectivePort(); p != 465 {
		t.Fatalf("tls default port = %d, want 465", p)
	}
	if p := (Config{Port: 2525}).effectivePort(); p != 2525 {
		t.Fatalf("explicit port = %d, want 2525", p)
	}
}
