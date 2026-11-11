package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/notify/registry"
)

// secretToken stands in for a bot token or webhook secret that a provider puts
// in its request path.
const secretToken = "123456:AAH-very-secret"

// urlErrNotifier fails the way a real provider does when the endpoint is
// unreachable: net/http hands back a *url.Error quoting the full request URL.
type urlErrNotifier struct{ typ string }

func (n urlErrNotifier) Type() string                 { return n.typ }
func (urlErrNotifier) Validate(json.RawMessage) error { return nil }
func (urlErrNotifier) Send(ctx context.Context, _ json.RawMessage, _ domain.NotificationEvent) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://127.0.0.1:1/bot"+secretToken+"/sendMessage", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		return errors.New("expected the request to fail")
	}
	return fmt.Errorf("chat: sendMessage: %w", err)
}

// formattingNotifier puts the configured URL into its own message, which no
// *url.Error points at.
type formattingNotifier struct{}

func (formattingNotifier) Type() string                   { return "formatting" }
func (formattingNotifier) Validate(json.RawMessage) error { return nil }
func (formattingNotifier) Send(_ context.Context, cfg json.RawMessage, _ domain.NotificationEvent) error {
	var c struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(cfg, &c)
	return fmt.Errorf("hook: POST %s: unexpected status 500", c.URL)
}

// syncBuffer is a log sink the dispatcher's goroutines can share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// A secret in a provider's request URL must not reach any of the three places a
// delivery error is written: the log, the delivery audit log, and the error a
// test send hands back to the API.
func TestDeliveryErrorsNeverCarryTheRequestURL(t *testing.T) {
	cases := []struct {
		name     string
		notifier registry.Notifier
		config   string
	}{
		{"transport error", urlErrNotifier{typ: "chat"}, `{}`},
		{"provider formatted the URL", formattingNotifier{},
			`{"url":"https://hooks.example.test/services/` + secretToken + `"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := &syncBuffer{}
			h := newHarness(t, Options{Logger: slog.New(slog.NewTextHandler(logs, nil))})
			h.reg.Register(tc.notifier)
			n := Notification{ID: 1, Name: "ops", Type: tc.notifier.Type(),
				Config: json.RawMessage(tc.config), Active: true}

			// The test-send path.
			err := h.d.TestSend(context.Background(), n)
			if err == nil {
				t.Fatal("TestSend succeeded against a failing provider")
			}
			if strings.Contains(err.Error(), secretToken) {
				t.Errorf("TestSend error carries the secret: %v", err)
			}

			// The alert path, which goes through the fan-out and its own log line.
			h.store.SetAssignments(7, Assignment{Notification: n})
			h.store.SetMonitorSettings(7, MonitorSettings{Name: "API"})
			h.d.HandleStateChange(downEvent(7, "API"))
			h.drain(t)

			deliveries := h.store.Deliveries()
			if len(deliveries) < 2 {
				t.Fatalf("deliveries = %d, want the test send and the alert recorded", len(deliveries))
			}
			for _, d := range deliveries {
				if d.Status != DeliveryFailed {
					t.Errorf("delivery %+v recorded as %s, want failed", d, d.Status)
				}
				if d.Error == "" {
					t.Error("a failed delivery was recorded without an error")
				}
				if strings.Contains(d.Error, secretToken) {
					t.Errorf("notification_deliveries.error carries the secret: %q", d.Error)
				}
			}
			if out := logs.String(); strings.Contains(out, secretToken) {
				t.Errorf("the log carries the secret:\n%s", out)
			}
		})
	}
}

func TestErrorClass(t *testing.T) {
	timeoutCtx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	<-timeoutCtx.Done()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://127.0.0.1:1/", nil)
	_, dialErr := http.DefaultClient.Do(req)

	tests := []struct {
		name string
		err  error
		want string
	}{
		{"deadline", fmt.Errorf("x: %w", timeoutCtx.Err()), ErrorClassTimeout},
		{"unreachable", dialErr, ErrorClassNetwork},
		{"unknown provider", fmt.Errorf("ops: %w", ErrUnknownProvider), ErrorClassConfig},
		{"rejected", errors.New("slack: unexpected status 403 Forbidden"), ErrorClassRejected},
	}
	for _, tt := range tests {
		if got := ErrorClass(tt.err); got != tt.want {
			t.Errorf("%s: ErrorClass(%v) = %q, want %q", tt.name, tt.err, got, tt.want)
		}
	}
}
