package webpush

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/notify/registry"
)

const validSub = `{"endpoint":"https://push.example.com/abc","keys":{"p256dh":"BPk","auth":"c2VjcmV0"}}`

func TestSelfRegisters(t *testing.T) {
	if _, ok := registry.Get("webpush"); !ok {
		t.Fatal("webpush provider did not self-register")
	}
}

func TestValidate(t *testing.T) {
	n := New()
	tests := []struct {
		name    string
		cfg     string
		wantErr bool
	}{
		{"ok", `{"vapid_public_key":"pub","vapid_private_key":"priv","subject":"mailto:a@b.c","subscription":` + validSub + `}`, false},
		{"ok https subject", `{"vapid_public_key":"pub","vapid_private_key":"priv","subject":"https://a.b","subscription":` + validSub + `}`, false},
		{"missing public", `{"vapid_private_key":"priv","subject":"mailto:a@b","subscription":` + validSub + `}`, true},
		{"missing private", `{"vapid_public_key":"pub","subject":"mailto:a@b","subscription":` + validSub + `}`, true},
		{"missing subject", `{"vapid_public_key":"pub","vapid_private_key":"priv","subscription":` + validSub + `}`, true},
		{"bad subject", `{"vapid_public_key":"pub","vapid_private_key":"priv","subject":"ftp://x","subscription":` + validSub + `}`, true},
		{"missing subscription", `{"vapid_public_key":"pub","vapid_private_key":"priv","subject":"mailto:a@b"}`, true},
		{"no endpoint", `{"vapid_public_key":"pub","vapid_private_key":"priv","subject":"mailto:a@b","subscription":{"keys":{"p256dh":"x","auth":"y"}}}`, true},
		{"no keys", `{"vapid_public_key":"pub","vapid_private_key":"priv","subject":"mailto:a@b","subscription":{"endpoint":"https://p"}}`, true},
		{"broken json", `{"vapid_public_key":`, true},
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
	var gotPayload message
	var gotSub *webpush.Subscription
	var gotOpts *webpush.Options
	n := New()
	// Inject a capture in place of the real VAPID-signed push.
	n.send = func(_ context.Context, payload []byte, sub *webpush.Subscription, opts *webpush.Options) (*http.Response, error) {
		_ = json.Unmarshal(payload, &gotPayload)
		gotSub = sub
		gotOpts = opts
		return &http.Response{StatusCode: http.StatusCreated, Body: io.NopCloser(bytes.NewReader(nil))}, nil
	}

	cfg := json.RawMessage(`{"vapid_public_key":"pubkey","vapid_private_key":"privkey","subject":"mailto:ops@example.com","subscription":` + validSub + `}`)
	ev := domain.NotificationEvent{
		Kind: domain.EventKindDown, MonitorID: 42, MonitorName: "API",
		MonitorURL: "https://api.example.com", Status: domain.StatusDown,
		Title: "API is DOWN", Body: "connection refused",
	}
	if err := n.Send(t.Context(), cfg, ev); err != nil {
		t.Fatalf("Send error = %v", err)
	}
	if gotPayload.Title != "API is DOWN" || gotPayload.Body != "connection refused" {
		t.Fatalf("payload not mapped: %+v", gotPayload)
	}
	if gotPayload.URL != "https://api.example.com" {
		t.Fatalf("url = %q", gotPayload.URL)
	}
	if gotPayload.Tag != "statwarden-42" {
		t.Fatalf("tag = %q, want statwarden-42", gotPayload.Tag)
	}
	if gotSub == nil || gotSub.Endpoint != "https://push.example.com/abc" {
		t.Fatalf("subscription not passed through: %+v", gotSub)
	}
	if gotOpts == nil || gotOpts.VAPIDPublicKey != "pubkey" || gotOpts.VAPIDPrivateKey != "privkey" || gotOpts.Subscriber != "mailto:ops@example.com" {
		t.Fatalf("options not mapped: %+v", gotOpts)
	}
}

func TestSendNon2xx(t *testing.T) {
	n := New()
	n.send = func(_ context.Context, _ []byte, _ *webpush.Subscription, _ *webpush.Options) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusGone, Status: "410 Gone", Body: io.NopCloser(bytes.NewReader(nil))}, nil
	}
	cfg := json.RawMessage(`{"vapid_public_key":"pub","vapid_private_key":"priv","subject":"mailto:a@b","subscription":` + validSub + `}`)
	if err := n.Send(t.Context(), cfg, domain.NotificationEvent{Kind: domain.EventKindTest, Title: "t"}); err == nil {
		t.Fatal("expected error on 410 response")
	}
}
