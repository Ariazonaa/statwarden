package webhook

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/netguard"
)

// A webhook channel is configured by the same writers as a monitor, so its URL
// is the same probe into the private network and gets the same guard.
func TestSendHonoursTheEgressGuard(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	netguard.SetEnabled(true)
	t.Cleanup(func() { netguard.SetEnabled(false) })

	err := New().Send(t.Context(), json.RawMessage(`{"url":"`+srv.URL+`"}`), domain.NotificationEvent{
		Kind: domain.EventKindDown, MonitorID: 1, MonitorName: "API", Status: domain.StatusDown, At: time.Now(),
	})
	if err == nil || hit {
		t.Fatalf("a loopback webhook was delivered with the guard on (err=%v, hit=%v)", err, hit)
	}
	if !strings.Contains(err.Error(), "BLOCK_PRIVATE_TARGETS") {
		t.Fatalf("refusal does not name the guard: %v", err)
	}
}
