package api

import (
	"net/http"
	"testing"
	"time"
)

// The expiry column was dead for as long as it existed: auth_middleware checked
// api_keys.expires_at on every single request and PgStore.CreateAPIKey wrote it,
// but createAPIKeyRequest had no field for it, so the handler always passed nil
// and every key ever minted was a permanent bearer credential. A check with no
// writer is indistinguishable from no check at all.
//
// These tests are written so that they fail against that state: the first one
// asserts the key stops working, which it never did.

// harnessAtClock builds a harness whose clock the caller can move. Everything
// downstream of it — session expiry, sliding TTL, key expiry — reads the same
// function, so advancing it is the only honest way to test "later".
func harnessAtClock(t *testing.T, clock *time.Time) *harness {
	t.Helper()
	return newHarnessTweaked(t, func(o *Options) {
		o.Now = func() time.Time { return *clock }
	})
}

// mintKeyReq creates an API key from sess and returns its secret plus the decoded
// api_key object.
func mintKeyReq(t *testing.T, h *harness, sess *session, req createAPIKeyRequest) (string, APIKey) {
	t.Helper()
	rec := h.requestWith(http.MethodPost, "/api/v1/me/api-keys", req, sess)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create key = %d, want 201; body %s", rec.Code, rec.Body.String())
	}
	var out struct {
		APIKey APIKey `json:"api_key"`
		Secret string `json:"secret"`
	}
	mustJSON(t, rec, &out)
	return out.Secret, out.APIKey
}

func TestAPIKeyExpiresAfterTheRequestedDays(t *testing.T) {
	clock := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	h := harnessAtClock(t, &clock)
	admin := h.setupAdmin()

	days := 30
	secret, key := mintKeyReq(t, h, admin, createAPIKeyRequest{
		Name:          "ci",
		Scopes:        []string{ScopeRead},
		ExpiresInDays: &days,
		stepUp:        stepUp{Password: "supersecret"},
	})

	// The response has to carry the expiry, not just the database: the UI shows
	// it, and a key whose lifetime is invisible is one nobody rotates.
	if key.ExpiresAt == nil {
		t.Fatal("created key has no expires_at; the request's expires_in_days was dropped")
	}
	if want := clock.AddDate(0, 0, days); !key.ExpiresAt.Equal(want) {
		t.Fatalf("expires_at = %s, want %s", key.ExpiresAt, want)
	}

	// One day before: still a working credential.
	clock = clock.AddDate(0, 0, days-1)
	if rec := h.requestKey(http.MethodGet, "/api/v1/monitors", secret, nil); rec.Code != http.StatusOK {
		t.Fatalf("key one day before expiry = %d, want 200; body %s", rec.Code, rec.Body.String())
	}

	// One day after: rejected. This is the assertion the missing writer made
	// unreachable — before the fix the key answers 200 here, forever.
	clock = clock.AddDate(0, 0, 2)
	if rec := h.requestKey(http.MethodGet, "/api/v1/monitors", secret, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired key = %d, want 401; body %s", rec.Code, rec.Body.String())
	}
}

// Omitting the field has to keep meaning "never", or shipping the feature
// silently revokes every integration that was minted without it.
func TestAPIKeyWithoutExpiryKeepsWorking(t *testing.T) {
	clock := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	h := harnessAtClock(t, &clock)
	admin := h.setupAdmin()

	secret, key := mintKeyReq(t, h, admin, createAPIKeyRequest{
		Name:   "forever",
		Scopes: []string{ScopeRead},
		stepUp: stepUp{Password: "supersecret"},
	})
	if key.ExpiresAt != nil {
		t.Fatalf("expires_at = %s, want nil for a request that did not ask for one", key.ExpiresAt)
	}

	clock = clock.AddDate(20, 0, 0)
	if rec := h.requestKey(http.MethodGet, "/api/v1/monitors", secret, nil); rec.Code != http.StatusOK {
		t.Fatalf("key with no expiry after 20 years = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
}

func TestAPIKeyExpiryRejectsOutOfRangeDays(t *testing.T) {
	clock := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	h := harnessAtClock(t, &clock)
	admin := h.setupAdmin()

	// 0 is rejected rather than treated as "never": a client that sends it
	// meant something, and silently reinterpreting it as an unlimited key is
	// the wrong way to resolve the ambiguity. "Never" is expressed by leaving
	// the field out, which TestAPIKeyWithoutExpiryKeepsWorking pins.
	for _, days := range []int{0, -1, apiKeyMaxExpiryDays + 1} {
		d := days
		rec := h.requestWith(http.MethodPost, "/api/v1/me/api-keys", createAPIKeyRequest{
			Name:          "bad",
			Scopes:        []string{ScopeRead},
			ExpiresInDays: &d,
			stepUp:        stepUp{Password: "supersecret"},
		}, admin)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expires_in_days=%d = %d, want 400; body %s", days, rec.Code, rec.Body.String())
		}
	}

	// The boundaries themselves are accepted.
	for _, days := range []int{1, apiKeyMaxExpiryDays} {
		d := days
		if _, key := mintKeyReq(t, h, admin, createAPIKeyRequest{
			Name:          "edge",
			Scopes:        []string{ScopeRead},
			ExpiresInDays: &d,
			stepUp:        stepUp{Password: "supersecret"},
		}); key.ExpiresAt == nil {
			t.Fatalf("expires_in_days=%d produced no expiry", days)
		}
	}
}
