package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/domain"
)

// A webhook's custom headers are how the receiving system authenticates the
// call. net/http replays anything it does not recognise as sensitive to
// whatever host answers a 3xx, and a webhook URL is operator-supplied — often
// pointing at a service that redirects — so the token went to the redirect
// target.
func TestCustomHeadersAreStrippedOnACrossOriginRedirect(t *testing.T) {
	var got http.Header
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/hook", http.StatusFound)
	}))
	defer origin.Close()

	cfg, err := json.Marshal(map[string]any{
		"url": origin.URL,
		"headers": map[string]string{
			"X-Api-Key":     "secret-key",
			"Authorization": "Bearer secret-token",
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := New().Send(context.Background(), cfg, domain.NotificationEvent{
		Kind: domain.EventKindDown, MonitorName: "api",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	for _, name := range []string{"X-Api-Key", "Authorization"} {
		if v := got.Get(name); v != "" {
			t.Errorf("%s reached the redirect target as %q", name, v)
		}
	}
}

// The guard must not break the ordinary case: a receiver that redirects to its
// own canonical path still has to be authenticated.
func TestCustomHeadersSurviveASameOriginRedirect(t *testing.T) {
	var got string
	mux := http.NewServeMux()
	mux.HandleFunc("/hook", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/hook/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/hook/", func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Api-Key")
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg, err := json.Marshal(map[string]any{
		"url":     srv.URL + "/hook",
		"headers": map[string]string{"X-Api-Key": "keep-me"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := New().Send(context.Background(), cfg, domain.NotificationEvent{
		Kind: domain.EventKindDown, MonitorName: "api",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got != "keep-me" {
		t.Fatalf("X-Api-Key on a same-origin redirect = %q, want it kept", got)
	}
}
