package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/notify"
	notifyreg "github.com/Ariazonaa/statwarden/internal/notify/registry"
)

// botToken is what a chat provider carries in its request path.
const botToken = "987654:AAF-test-send-secret"

// unreachableNotifier is registered under the harness's "fake" type and fails
// the way a real chat provider does when its API cannot be reached: with
// net/http's *url.Error, which quotes the request URL — token included.
type unreachableNotifier struct{}

func (unreachableNotifier) Type() string                   { return "fake" }
func (unreachableNotifier) Validate(json.RawMessage) error { return nil }
func (unreachableNotifier) Send(ctx context.Context, _ json.RawMessage, _ domain.NotificationEvent) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://127.0.0.1:1/bot"+botToken+"/sendMessage", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		return nil
	}
	return fmt.Errorf("chat: sendMessage: %w", err)
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// The test-send endpoint used to answer "test send failed: " + the provider's
// raw error, which for Telegram, Slack, Discord and every other provider with a
// secret in its URL was the secret. It now answers a failure class and a message
// with the URL reduced to its host — and the same holds for the log and the
// delivery audit log the dispatcher writes on the way.
func TestTestSendFailureNeverEchoesTheSecret(t *testing.T) {
	logs := &lockedBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))

	reg := notifyreg.New()
	reg.Register(unreachableNotifier{})
	deliveries := notify.NewMemStore()
	disp := notify.New(notify.Options{Logger: logger, Store: deliveries, Registry: reg})
	disp.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = disp.Stop(ctx)
	})

	h := newHarnessTweaked(t, func(o *Options) {
		o.Dispatcher = disp
		o.Logger = logger
	})
	admin := h.setupAdmin()

	rec := h.requestWith(http.MethodPost, "/api/v1/notifications",
		notificationRequest{Name: "chat", Type: "fake", Config: json.RawMessage(`{}`)}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create notification = %d, body %s", rec.Code, rec.Body.String())
	}
	var n Notification
	mustJSON(t, rec, &n)

	rec = h.requestWith(http.MethodPost, fmt.Sprintf("/api/v1/notifications/%d/test", n.ID), nil, admin)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("failed test send = %d, want 502; body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, botToken) {
		t.Errorf("the API response carries the bot token: %s", body)
	}
	var problem struct {
		Detail     string `json:"detail"`
		ErrorClass string `json:"error_class"`
	}
	mustJSON(t, rec, &problem)
	if problem.ErrorClass != notify.ErrorClassNetwork {
		t.Errorf("error_class = %q, want %q", problem.ErrorClass, notify.ErrorClassNetwork)
	}
	if !strings.Contains(problem.Detail, "127.0.0.1:1") {
		t.Errorf("detail lost the host, which is what tells the operator which endpoint "+
			"failed: %q", problem.Detail)
	}

	recorded := deliveries.Deliveries()
	if len(recorded) != 1 {
		t.Fatalf("deliveries = %d, want the test send recorded", len(recorded))
	}
	if strings.Contains(recorded[0].Error, botToken) {
		t.Errorf("notification_deliveries.error carries the bot token: %q", recorded[0].Error)
	}
	if strings.Contains(logs.String(), botToken) {
		t.Errorf("the log carries the bot token:\n%s", logs.String())
	}
}
