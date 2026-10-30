package metrics

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeSource struct {
	snap Snapshot
	err  error
}

func (f fakeSource) Snapshot(context.Context) (Snapshot, error) { return f.snap, f.err }

func scrape(t *testing.T, h http.Handler, req *http.Request) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func sampleSource() fakeSource {
	return fakeSource{snap: Snapshot{
		Monitors: []MonitorSample{
			{ID: 1, Name: "web", Type: "http", Status: "up", LatencyMs: 42},
			{ID: 2, Name: "db", Type: "tcp", Status: "down", LatencyMs: -1},
		},
		StatusCounts: map[string]int{"up": 1, "down": 1},
		Workers:      8,
	}}
}

func TestHandlerExportsMetrics(t *testing.T) {
	h := Handler(sampleSource(), "")
	code, body := scrape(t, h, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if code != http.StatusOK {
		t.Fatalf("code = %d", code)
	}
	for _, want := range []string{
		`statwarden_monitor_up{monitor_id="1",monitor_name="web",type="http"} 1`,
		`statwarden_monitor_up{monitor_id="2",monitor_name="db",type="tcp"} 0`,
		`statwarden_monitor_latency_ms{monitor_id="1",monitor_name="web",type="http"} 42`,
		`statwarden_monitors_total{status="up"} 1`,
		`statwarden_engine_workers 8`,
		`statwarden_scrape_error 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q\n---\n%s", want, body)
		}
	}
	// Monitor 2 has unknown latency -> not exported.
	if strings.Contains(body, `statwarden_monitor_latency_ms{monitor_id="2"`) {
		t.Error("unknown latency should not be exported")
	}
}

func TestHandlerScrapeError(t *testing.T) {
	h := Handler(fakeSource{err: errors.New("boom")}, "")
	_, body := scrape(t, h, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(body, "statwarden_scrape_error 1") {
		t.Fatalf("expected scrape_error 1, got:\n%s", body)
	}
}

func TestHandlerTokenGate(t *testing.T) {
	h := Handler(sampleSource(), "secret")

	// No token -> 401.
	if code, _ := scrape(t, h, httptest.NewRequest(http.MethodGet, "/metrics", nil)); code != http.StatusUnauthorized {
		t.Fatalf("no token code = %d, want 401", code)
	}
	// Query param.
	if code, _ := scrape(t, h, httptest.NewRequest(http.MethodGet, "/metrics?token=secret", nil)); code != http.StatusOK {
		t.Fatalf("query token code = %d, want 200", code)
	}
	// Bearer header.
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer secret")
	if code, _ := scrape(t, h, req); code != http.StatusOK {
		t.Fatalf("bearer token code = %d, want 200", code)
	}
	// Wrong token.
	if code, _ := scrape(t, h, httptest.NewRequest(http.MethodGet, "/metrics?token=nope", nil)); code != http.StatusUnauthorized {
		t.Fatalf("wrong token code = %d, want 401", code)
	}
}

// TestHandlerQueryTokenIsDeprecatedButStillWorks pins the backwards-compatible
// half of moving the scrape secret out of the URL.
//
// A ?token= in the URL is copied into reverse-proxy access logs and into
// Prometheus's own target metadata, so several systems end up holding the
// scrape secret. Existing scrape configurations keep working — breaking
// monitoring to improve monitoring is a bad trade — but the operator is told,
// ONCE. A scrape runs every few seconds, so warning on each one would drown the
// log rather than inform anyone.
func TestHandlerQueryTokenIsDeprecatedButStillWorks(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := Handler(sampleSource(), "secret")
	for i := range 3 {
		if code, _ := scrape(t, h, httptest.NewRequest(http.MethodGet, "/metrics?token=secret", nil)); code != http.StatusOK {
			t.Fatalf("scrape %d = %d, want 200 (the query token must keep working)", i, code)
		}
	}
	if n := strings.Count(buf.String(), "deprecated"); n != 1 {
		t.Fatalf("deprecation warnings = %d across three scrapes, want exactly 1: %s", n, buf.String())
	}

	// The supported header path must stay silent.
	buf.Reset()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("X-Metrics-Token", "secret")
	if code, _ := scrape(t, h, req); code != http.StatusOK {
		t.Fatalf("X-Metrics-Token scrape = %d, want 200", code)
	}
	if strings.Contains(buf.String(), "deprecated") {
		t.Fatalf("the supported header path logged a deprecation: %s", buf.String())
	}
}

// The deprecation headers ride on EVERY query-token scrape, unlike the log
// warning, which fires once per process.
//
// The asymmetry is the point: the log line is for the operator of this instance,
// the headers are for the caller — and the caller is a Prometheus server whose
// configuration lives somewhere else entirely and which will never read these
// logs. A machine client can act on Sunset; it cannot act on a log line it cannot
// see.
func TestQueryTokenScrapeCarriesDeprecationHeaders(t *testing.T) {
	h := Handler(sampleSource(), "secret")

	for i := range 3 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics?token=secret", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("scrape %d = %d, want 200", i, rec.Code)
		}
		if got := rec.Header().Get("Deprecation"); got != "true" {
			t.Fatalf("scrape %d: Deprecation = %q, want true on every response", i, got)
		}
		if _, err := http.ParseTime(rec.Header().Get("Sunset")); err != nil {
			t.Fatalf("scrape %d: Sunset = %q is unparsable: %v",
				i, rec.Header().Get("Sunset"), err)
		}
	}

	// The supported path advertises no deprecation at all.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("X-Metrics-Token", "secret")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("header scrape = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Deprecation"); got != "" {
		t.Fatalf("the header path advertised Deprecation = %q; only the query "+
			"parameter is deprecated", got)
	}
	if got := rec.Header().Get("Sunset"); got != "" {
		t.Fatalf("the header path advertised Sunset = %q", got)
	}
}
