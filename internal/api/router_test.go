package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Ariazonaa/statwarden/internal/version"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testDist() fstest.MapFS {
	return fstest.MapFS{
		"index.html":                     {Data: []byte("<!doctype html><title>Statwarden</title>")},
		"_app/immutable/entry/app.1a.js": {Data: []byte("console.log('app')")},
		"favicon.svg":                    {Data: []byte("<svg xmlns='http://www.w3.org/2000/svg'/>")},
	}
}

func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	return New(Options{Logger: testLogger(), Static: SPA(testDist())})
}

func do(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func TestHealthz(t *testing.T) {
	rec := do(t, newTestHandler(t), http.MethodGet, "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON body: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf(`status = %q, want "ok"`, body["status"])
	}
}

func TestReadyzWithoutPool(t *testing.T) {
	rec := do(t, newTestHandler(t), http.MethodGet, "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz without pool = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "database not configured") {
		t.Errorf("body %q does not explain the missing database", rec.Body.String())
	}
}

func TestVersionEndpoint(t *testing.T) {
	rec := do(t, newTestHandler(t), http.MethodGet, "/api/v1/version")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/version = %d, want 200", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON body: %v", err)
	}
	if body["version"] != version.Version {
		t.Errorf("version = %q, want %q", body["version"], version.Version)
	}
	for _, key := range []string{"commit", "build_date", "go_version"} {
		if body[key] == "" {
			t.Errorf("response is missing %q", key)
		}
	}
}

func TestUnknownAPIPathIsProblemJSONNotSPA(t *testing.T) {
	rec := do(t, newTestHandler(t), http.MethodGet, "/api/v1/nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /api/v1/nope = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
}

func TestSPAServesIndexAndFallback(t *testing.T) {
	h := newTestHandler(t)

	for _, target := range []string{"/", "/monitors/42"} {
		rec := do(t, h, http.MethodGet, target)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", target, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "Statwarden") {
			t.Errorf("GET %s did not serve the SPA shell", target)
		}
	}

	rec := do(t, h, http.MethodGet, "/_app/immutable/entry/app.1a.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("asset request = %d, want 200", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("asset Cache-Control = %q, want immutable", cc)
	}
}

func TestSPAWithoutBuildReturns503(t *testing.T) {
	h := New(Options{
		Logger: testLogger(),
		Static: SPA(fstest.MapFS{".gitkeep": {Data: []byte("")}}),
	})
	rec := do(t, h, http.MethodGet, "/")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET / with unbuilt frontend = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "frontend not built") {
		t.Errorf("body %q does not mention the unbuilt frontend", rec.Body.String())
	}
}

func TestPanicIsRecovered(t *testing.T) {
	h := New(Options{Logger: testLogger(), Static: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})})
	rec := do(t, h, http.MethodGet, "/anything")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("panicking handler = %d, want 500", rec.Code)
	}
}
