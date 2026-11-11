package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/netguard"
)

// Notification channels are configured by the same writers as monitors, so a
// webhook URL is as good a probe into the private network as a monitor target.
func TestNotifierClientHonoursTheEgressGuard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	netguard.SetEnabled(true)
	t.Cleanup(func() { netguard.SetEnabled(false) })

	resp, err := NotifierClient(5 * time.Second).Get(srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("a loopback webhook was reached with the guard on")
	}
	if !strings.Contains(err.Error(), "BLOCK_PRIVATE_TARGETS") {
		t.Fatalf("refusal does not name the guard: %v", err)
	}

	netguard.SetEnabled(false)
	resp, err = NotifierClient(5 * time.Second).Get(srv.URL)
	if err != nil {
		t.Fatalf("with the guard off the same request failed: %v", err)
	}
	_ = resp.Body.Close()
}
