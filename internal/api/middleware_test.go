package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	monreg "github.com/Ariazonaa/statwarden/internal/monitor/registry"
	notifyreg "github.com/Ariazonaa/statwarden/internal/notify/registry"
)

func TestRedactLogPath(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// The push token is masked, the route stays legible.
		{"/api/v1/push/deadbeefdeadbeefdeadbeefdeadbeef", "/api/v1/push/<redacted>"},
		{"/api/v1/push/x", "/api/v1/push/<redacted>"},
		// The bare route with no token is left alone (nothing to leak).
		{"/api/v1/push/", "/api/v1/push/"},
		{"/api/v1/push", "/api/v1/push"},
		// Unrelated paths pass through untouched.
		{"/api/v1/monitors", "/api/v1/monitors"},
		{"/healthz", "/healthz"},
		{"/api/v1/pushover/x", "/api/v1/pushover/x"},
	}
	for _, c := range cases {
		if got := redactLogPath(c.in); got != c.want {
			t.Errorf("redactLogPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestRequestLoggerRedactsPushToken proves the redaction is actually wired into
// the request logger: a push heartbeat's token never reaches the log, while the
// route (with the token masked) still does. Fails if requestLogger logs the raw
// r.URL.Path.
func TestRequestLoggerRedactsPushToken(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	store := NewMemStore()
	mr := monreg.New()
	mr.Register(fakeChecker{})
	nr := notifyreg.New()
	nr.Register(fakeNotifier{})
	h := New(Options{
		Logger:          logger,
		Store:           store,
		MonitorRegistry: mr,
		NotifyRegistry:  nr,
		Maintenance:     newMaintenanceProvider(store, logger),
		Mailer:          &mailCapture{},
		PublicBaseURL:   "https://statwarden.example",
	})

	const token = "supersecretpushtoken1234567890ab"
	req := httptest.NewRequest(http.MethodPost, "/api/v1/push/"+token, nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	logged := buf.String()
	if strings.Contains(logged, token) {
		t.Fatalf("request log leaked the push token:\n%s", logged)
	}
	if !strings.Contains(logged, pushPathPrefix+"<redacted>") {
		t.Fatalf("request log did not record the redacted push path:\n%s", logged)
	}
}
