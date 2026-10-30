package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/monitor/push"
)

// TestRegenerateRejectsNonPushMonitor pins the lifecycle rule that a push token
// may only be minted for a push monitor: regenerating one for a non-push
// monitor is a 409, not a silent mint.
func TestRegenerateRejectsNonPushMonitor(t *testing.T) {
	h := newExtHarness(t, nil, nil)
	admin := h.setupAdmin()
	rec := h.requestWith(http.MethodPost, "/api/v1/monitors", monitorRequest{
		Name: "web", Type: "http", Interval: 30, Config: json.RawMessage(`{}`),
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create monitor = %d, body %s", rec.Code, rec.Body.String())
	}
	var created monitorView
	mustJSON(t, rec, &created)

	rec = h.requestWith(http.MethodPost, "/api/v1/monitors/"+itoa64(created.ID)+"/push-token", nil, admin)
	if rec.Code != http.StatusConflict {
		t.Fatalf("regenerate on non-push = %d, want 409; body %s", rec.Code, rec.Body.String())
	}
}

// TestRegenerateRejectsPausedMonitor pins that a paused push monitor cannot
// hand out a fresh secret via regenerate.
func TestRegenerateRejectsPausedMonitor(t *testing.T) {
	h := newExtHarness(t, nil, nil)
	admin := h.setupAdmin()
	inactive := false
	rec := h.requestWith(http.MethodPost, "/api/v1/monitors", monitorRequest{
		Name: "cron", Type: "push", Interval: 60, Config: json.RawMessage(`{}`), Active: &inactive,
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create monitor = %d, body %s", rec.Code, rec.Body.String())
	}
	var created monitorView
	mustJSON(t, rec, &created)

	rec = h.requestWith(http.MethodPost, "/api/v1/monitors/"+itoa64(created.ID)+"/push-token", nil, admin)
	if rec.Code != http.StatusConflict {
		t.Fatalf("regenerate on paused = %d, want 409; body %s", rec.Code, rec.Body.String())
	}
}

// TestReconcilePushToken exercises the create/update minting logic directly:
// a push monitor without a token is minted one, an existing token is kept, and
// any non-push type has its token revoked. It also pins the password-like
// storage: the value written is the token's hash, and only a freshly minted
// token is revealed in plaintext.
func TestReconcilePushToken(t *testing.T) {
	// Becomes/stays push without a token: mint one. The stored value is the
	// hash; the plaintext is returned separately for the one-time reveal.
	storeHash, plain, err := reconcilePushToken("push", nil)
	if err != nil {
		t.Fatalf("reconcile push/nil: %v", err)
	}
	if len(plain) != 32 {
		t.Fatalf("minted plaintext token = %q, want 32 hex chars", plain)
	}
	if storeHash == nil || *storeHash != push.HashToken(plain) {
		t.Fatalf("stored value is not the hash of the minted token: store=%v plain=%q", storeHash, plain)
	}
	if storeHash != nil && *storeHash == plain {
		t.Fatal("the raw token must never be the stored value")
	}

	// Stays push with a token: keep the existing hash, reveal nothing (the
	// plaintext was never stored and cannot be recovered).
	existing := push.HashToken("deadbeefdeadbeefdeadbeefdeadbeef")
	storeHash, plain, err = reconcilePushToken("push", &existing)
	if err != nil || storeHash == nil || *storeHash != existing {
		t.Fatalf("existing push hash not preserved: store=%v err=%v", storeHash, err)
	}
	if plain != "" {
		t.Fatalf("a kept token must not be revealed again, got %q", plain)
	}

	// Any non-push type revokes the token.
	storeHash, plain, err = reconcilePushToken("http", &existing)
	if err != nil {
		t.Fatalf("reconcile http: %v", err)
	}
	if storeHash != nil || plain != "" {
		t.Fatalf("non-push monitor must not carry a token, got store=%v plain=%q", storeHash, plain)
	}
}

// TestPushRateLimitIsPerTokenNotPerIP pins that many push monitors reporting
// from ONE egress address do not throttle each other.
//
// They used to share the 60/min public-page budget keyed by client IP alone, so
// past sixty beats a minute the rest were rejected — the heartbeat never reached
// the recorder, the watchdog saw nothing arrive and tripped healthy monitors
// DOWN. The rate limit manufactured the outage it had nothing to do with, which
// is the worst failure mode a monitoring system has.
func TestPushRateLimitIsPerTokenNotPerIP(t *testing.T) {
	h := newExtHarness(t, nil, &fakePush{})
	// Comfortably past the old per-IP ceiling, each beat carrying a DIFFERENT
	// monitor's token, all from the same client address.
	for i := 0; i < publicRateLimit*2; i++ {
		rec := h.request(http.MethodPost, "/api/v1/push/token-"+itoa(i), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("beat %d from a distinct token = %d, want 200; body %s",
				i, rec.Code, rec.Body.String())
		}
	}
}

// A single token flooding on its own is still braked — the per-token budget is
// the real abuse control, since a token identifies exactly one monitor.
func TestPushRateLimitStillBrakesASingleToken(t *testing.T) {
	h := newExtHarness(t, nil, &fakePush{})
	for i := 0; i < pushTokenRateLimit*2; i++ {
		if h.request(http.MethodPost, "/api/v1/push/one-token", nil).Code == http.StatusTooManyRequests {
			return
		}
	}
	t.Fatalf("a single token was never rate limited within %d requests", pushTokenRateLimit*2)
}
