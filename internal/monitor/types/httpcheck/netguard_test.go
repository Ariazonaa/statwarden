package httpcheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/netguard"
)

// The dial-time half of the egress guard. The engine's pre-flight looks at
// the configured URL only, so a public URL that redirects to a private one —
// or a name that resolves differently by the time the request goes out — is
// caught here, on the address the transport actually connects to.
func TestGuardRefusesTheDialEvenWithoutThePreflight(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	netguard.SetEnabled(true)
	t.Cleanup(func() { netguard.SetEnabled(false) })

	var ch Checker
	res, err := ch.Check(context.Background(), mustCfg(t, Config{URL: srv.URL}))
	msg := res.Message
	if err != nil {
		msg = err.Error()
	}
	if err == nil && res.Status != domain.StatusDown {
		t.Fatalf("a loopback target was reached with the guard on: %s", res.Status)
	}
	if !strings.Contains(msg, "BLOCK_PRIVATE_TARGETS") {
		t.Fatalf("refusal does not name the guard: %q", msg)
	}
}
