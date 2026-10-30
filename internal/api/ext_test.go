package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/importexport"
	"github.com/Ariazonaa/statwarden/internal/monitor/push"
	monreg "github.com/Ariazonaa/statwarden/internal/monitor/registry"
	notifyreg "github.com/Ariazonaa/statwarden/internal/notify/registry"
	"github.com/Ariazonaa/statwarden/internal/report"
)

// namedChecker is a registrable checker whose type name is configurable, so
// import tests can create monitors of arbitrary types.
type namedChecker string

func (n namedChecker) Type() string                 { return string(n) }
func (namedChecker) Validate(json.RawMessage) error { return nil }
func (namedChecker) Check(context.Context, json.RawMessage) (domain.CheckResult, error) {
	return domain.CheckResult{Status: domain.StatusUp}, nil
}

// fakeReporter is a canned Reporter for handler tests.
type fakeReporter struct {
	uptime   report.UptimeResult
	report   report.FullReport
	flakiest []report.Flakiness
	certs    []report.CertRow
	// seriesCalls counts UptimeSeries invocations so a test can pin that the
	// public page asks for its bars ONCE per monitor rather than once per bar.
	seriesCalls int
	// uptimeCalls counts Service.Uptime invocations, and uptimeSpans records the
	// window each one asked for. Service.Uptime walks its range a DAY AT A TIME
	// hunting rollup gaps, which is why parseReportRange refuses a window wider
	// than ~400 days — so a caller reaching it with a decade-wide range is a
	// full-scan on whatever surface it sits on, and the availability archive is a
	// public one.
	uptimeCalls int
	uptimeSpans []report.Range
}

func (f *fakeReporter) Uptime(_ context.Context, _ int64, r report.Range) (report.UptimeResult, error) {
	f.uptimeCalls++
	f.uptimeSpans = append(f.uptimeSpans, r)
	return f.uptime, nil
}

func (f *fakeReporter) UptimeSeries(_ context.Context, _ int64, buckets []report.Range) ([]report.UptimeResult, error) {
	f.seriesCalls++
	out := make([]report.UptimeResult, len(buckets))
	for i, b := range buckets {
		out[i] = f.uptime
		out[i].From, out[i].To = b.From, b.To
	}
	return out, nil
}
func (f *fakeReporter) Report(context.Context, int64, report.Range) (report.FullReport, error) {
	return f.report, nil
}
func (f *fakeReporter) Flakiest(context.Context, report.Range, int) ([]report.Flakiness, error) {
	return f.flakiest, nil
}
func (f *fakeReporter) CertOverview(context.Context) ([]report.CertRow, error) { return f.certs, nil }

// fakePush is a canned PushRecorder for handler tests.
type fakePush struct {
	monitor domain.Monitor
	err     error
	calls   []string
}

func (f *fakePush) Record(_ context.Context, token string, _ push.Beat) (domain.Monitor, error) {
	f.calls = append(f.calls, token)
	if f.err != nil {
		return domain.Monitor{}, f.err
	}
	return f.monitor, nil
}

// newExtHarness builds a harness wired with the given reporter/push fakes and a
// registry that knows the common monitor types.
func newExtHarness(t *testing.T, rep Reporter, pr PushRecorder) *harness {
	t.Helper()
	store := NewMemStore()
	eng := newFakeEngine()
	mr := monreg.New()
	mr.Register(fakeChecker{})
	for _, name := range []string{"http", "tcp", "ping", "push"} {
		mr.Register(namedChecker(name))
	}
	nr := notifyreg.New()
	nr.Register(fakeNotifier{})
	nr.Register(namedNotifier("slack"))

	h := New(Options{
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:           store,
		Engine:          eng,
		MonitorRegistry: mr,
		NotifyRegistry:  nr,
		Reporter:        rep,
		PushRecorder:    pr,
	})
	return &harness{t: t, handler: h, store: store, engine: eng}
}

// namedNotifier is a registrable notifier with a configurable type name.
type namedNotifier string

func (n namedNotifier) Type() string                 { return string(n) }
func (namedNotifier) Validate(json.RawMessage) error { return nil }
func (namedNotifier) Send(context.Context, json.RawMessage, domain.NotificationEvent) error {
	return nil
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

// createMonitorFake creates a monitor of the test "fake" type and returns its id.
func (h *harness) createMonitorFake(sess *session) int64 {
	h.t.Helper()
	return h.createMonitorNamed(sess, "m", "fake")
}

// createMonitorNamed creates a monitor of the given name/type and returns its id.
func (h *harness) createMonitorNamed(sess *session, name, typ string) int64 {
	h.t.Helper()
	rec := h.requestWith(http.MethodPost, "/api/v1/monitors", monitorRequest{
		Name: name, Type: typ, Interval: 60, Config: json.RawMessage(`{}`),
	}, sess)
	if rec.Code != http.StatusCreated {
		h.t.Fatalf("create monitor %q = %d, body %s", name, rec.Code, rec.Body.String())
	}
	var v monitorView
	mustJSON(h.t, rec, &v)
	return v.ID
}

// requestWithRaw sends a raw string body with the session's cookies/CSRF.
func (h *harness) requestWithRaw(method, target, body string, sess *session) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if sess != nil {
		for _, c := range sess.cookies {
			req.AddCookie(c)
		}
		if sess.csrf != "" {
			req.Header.Set(csrfHeader, sess.csrf)
		}
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func TestPushEndpoint(t *testing.T) {
	pr := &fakePush{monitor: domain.Monitor{ID: 5, Status: domain.StatusUp}}
	h := newExtHarness(t, nil, pr)

	rec := h.request(http.MethodPost, "/api/v1/push/tok123?status=up&ping=42&msg=ok", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("push = %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		OK        bool   `json:"ok"`
		MonitorID int64  `json:"monitor_id"`
		Status    string `json:"status"`
	}
	mustJSON(t, rec, &body)
	if !body.OK || body.MonitorID != 5 || body.Status != "up" {
		t.Fatalf("push body = %+v", body)
	}
	if len(pr.calls) != 1 || pr.calls[0] != "tok123" {
		t.Fatalf("recorder calls = %v", pr.calls)
	}
}

func TestPushEndpointUnknownToken(t *testing.T) {
	pr := &fakePush{err: push.ErrNotFound}
	h := newExtHarness(t, nil, pr)
	rec := h.request(http.MethodPost, "/api/v1/push/nope", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown token push = %d, want 404", rec.Code)
	}
}

func TestPushEndpointUnavailable(t *testing.T) {
	h := newExtHarness(t, nil, nil) // no push recorder
	rec := h.request(http.MethodPost, "/api/v1/push/tok", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("push without recorder = %d, want 503", rec.Code)
	}
}

func TestRegeneratePushToken(t *testing.T) {
	h := newExtHarness(t, nil, nil)
	admin := h.setupAdmin()
	// Create a push monitor.
	rec := h.requestWith(http.MethodPost, "/api/v1/monitors", monitorRequest{
		Name: "cron", Type: "push", Interval: 60, Config: json.RawMessage(`{}`),
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create monitor = %d, body %s", rec.Code, rec.Body.String())
	}
	var created monitorView
	mustJSON(t, rec, &created)

	rec = h.requestWith(http.MethodPost, "/api/v1/monitors/"+itoa64(created.ID)+"/push-token", nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("regenerate = %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		PushToken string `json:"push_token"`
		PushURL   string `json:"push_url"`
	}
	mustJSON(t, rec, &body)
	if len(body.PushToken) != 32 || !strings.Contains(body.PushURL, body.PushToken) {
		t.Fatalf("regenerate body = %+v", body)
	}
	// Hash-at-rest: the row stores the token's hash, never the plaintext, so a
	// stolen dump yields no working heartbeat credential. The response revealed
	// the plaintext once; the stored value must be its hash.
	m, _ := h.store.GetMonitor(context.Background(), created.ID)
	if m.PushToken == body.PushToken {
		t.Fatalf("push token persisted in plaintext: %q", m.PushToken)
	}
	if m.PushToken != push.HashToken(body.PushToken) {
		t.Fatalf("stored push token is not the hash of the revealed token: %q vs hash(%q)", m.PushToken, body.PushToken)
	}
}

func TestMonitorUptimeEndpoint(t *testing.T) {
	rep := &fakeReporter{uptime: report.UptimeResult{Up: 99, Down: 1, Total: 100, Uptime: 0.99}}
	h := newExtHarness(t, rep, nil)
	admin := h.setupAdmin()
	id := h.createMonitorFake(admin)

	rec := h.requestWith(http.MethodGet, "/api/v1/monitors/"+itoa64(id)+"/uptime?range=7d", nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("uptime = %d, body %s", rec.Code, rec.Body.String())
	}
	var got report.UptimeResult
	mustJSON(t, rec, &got)
	if got.Uptime != 0.99 {
		t.Fatalf("uptime = %+v", got)
	}
}

func TestFlakiestAndCertEndpoints(t *testing.T) {
	rep := &fakeReporter{
		flakiest: []report.Flakiness{{MonitorID: 2, MonitorName: "flaky", Transitions: 9}},
		certs:    []report.CertRow{{MonitorID: 1, MonitorName: "web", DaysRemaining: 3}},
	}
	h := newExtHarness(t, rep, nil)
	admin := h.setupAdmin()

	rec := h.requestWith(http.MethodGet, "/api/v1/report/flakiest", nil, admin)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "flaky") {
		t.Fatalf("flakiest = %d, body %s", rec.Code, rec.Body.String())
	}
	rec = h.requestWith(http.MethodGet, "/api/v1/report/certificates", nil, admin)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "web") {
		t.Fatalf("certificates = %d, body %s", rec.Code, rec.Body.String())
	}
}

func TestSLACSVEndpoint(t *testing.T) {
	rep := &fakeReporter{report: report.FullReport{Uptime: report.UptimeResult{Up: 10, Total: 10, Uptime: 1}}}
	h := newExtHarness(t, rep, nil)
	admin := h.setupAdmin()
	h.createMonitorFake(admin)

	rec := h.requestWith(http.MethodGet, "/api/v1/report/sla.csv", nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("sla.csv = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("content-type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "monitor_id,monitor_name") {
		t.Fatalf("csv body: %s", rec.Body.String())
	}
}

func TestSLAPDFEndpoint(t *testing.T) {
	rep := &fakeReporter{report: report.FullReport{Uptime: report.UptimeResult{Up: 10, Total: 10, Uptime: 1}}}
	h := newExtHarness(t, rep, nil)
	admin := h.setupAdmin()
	h.createMonitorFake(admin)

	rec := h.requestWith(http.MethodGet, "/api/v1/report/sla.pdf", nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("sla.pdf = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/pdf" {
		t.Fatalf("content-type = %q, want application/pdf", ct)
	}
	if !strings.HasPrefix(rec.Body.String(), "%PDF-") {
		t.Fatalf("body is not a PDF: %q", rec.Body.String()[:min(8, rec.Body.Len())])
	}
}

// mintBadge mints a monitor's badge token and returns it. Badges are addressed
// by this opaque token rather than the monitor id, so the public endpoints
// cannot be walked to enumerate monitors.
func (h *harness) mintBadge(sess *session, id int64) string {
	h.t.Helper()
	rec := h.requestWith(http.MethodPost, "/api/v1/monitors/"+itoa64(id)+"/badge", nil, sess)
	if rec.Code != http.StatusOK {
		h.t.Fatalf("mint badge = %d, body %s", rec.Code, rec.Body.String())
	}
	var out struct {
		BadgeToken string `json:"badge_token"`
	}
	mustJSON(h.t, rec, &out)
	if out.BadgeToken == "" {
		h.t.Fatal("mint badge returned no token")
	}
	return out.BadgeToken
}

func TestStatusBadgeEndpoint(t *testing.T) {
	h := newExtHarness(t, nil, nil)
	admin := h.setupAdmin()
	id := h.createMonitorFake(admin)
	token := h.mintBadge(admin, id)

	rec := h.request(http.MethodGet, "/api/v1/badge/"+token+"/status.svg", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status badge = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/svg+xml") {
		t.Fatalf("content-type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "<svg") {
		t.Fatalf("badge body: %s", rec.Body.String())
	}
}

func TestUptimeBadgeEndpoint(t *testing.T) {
	rep := &fakeReporter{uptime: report.UptimeResult{Uptime: 0.995, Total: 100}}
	h := newExtHarness(t, rep, nil)
	admin := h.setupAdmin()
	id := h.createMonitorFake(admin)
	token := h.mintBadge(admin, id)

	rec := h.request(http.MethodGet, "/api/v1/badge/"+token+"/uptime.svg", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "99.50%") {
		t.Fatalf("uptime badge = %d, body %s", rec.Code, rec.Body.String())
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	src := newExtHarness(t, nil, nil)
	admin := src.setupAdmin()
	// Seed a tag and two monitors.
	if rec := src.requestWith(http.MethodPost, "/api/v1/tags", map[string]string{"name": "prod", "color": "#fff"}, admin); rec.Code != http.StatusCreated {
		t.Fatalf("create tag = %d", rec.Code)
	}
	src.createMonitorNamed(admin, "web", "http")
	src.createMonitorNamed(admin, "ssh", "tcp")

	rec := src.requestWith(http.MethodGet, "/api/v1/export", nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("export = %d, body %s", rec.Code, rec.Body.String())
	}
	var bundle importexport.Bundle
	mustJSON(t, rec, &bundle)
	if len(bundle.Monitors) != 2 || len(bundle.Tags) != 1 {
		t.Fatalf("exported bundle = %+v", bundle)
	}

	// Import into a fresh instance.
	dst := newExtHarness(t, nil, nil)
	dstAdmin := dst.setupAdmin()
	rec = dst.requestWith(http.MethodPost, "/api/v1/import", bundle, dstAdmin)
	if rec.Code != http.StatusOK {
		t.Fatalf("import = %d, body %s", rec.Code, rec.Body.String())
	}
	var res importResult
	mustJSON(t, rec, &res)
	if res.Monitors != 2 || res.Tags != 1 {
		t.Fatalf("import result = %+v", res)
	}
	// Verify the monitors actually exist in the destination store.
	monitors, total, _ := dst.store.ListMonitors(context.Background(), MonitorFilter{}, Page{Limit: 50})
	if total != 2 || len(monitors) != 2 {
		t.Fatalf("destination monitors = %d", total)
	}
}

func TestImportKumaEndpoint(t *testing.T) {
	h := newExtHarness(t, nil, nil)
	admin := h.setupAdmin()
	kuma := `{"monitorList":[
		{"name":"Web","type":"http","url":"https://x.test","interval":60},
		{"name":"Weird","type":"gamedig","interval":60}
	]}`
	rec := h.requestWithRaw(http.MethodPost, "/api/v1/import/uptime-kuma", kuma, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("kuma import = %d, body %s", rec.Code, rec.Body.String())
	}
	var res importResult
	mustJSON(t, rec, &res)
	if res.Monitors != 1 {
		t.Fatalf("kuma import monitors = %d, want 1", res.Monitors)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("expected a warning about the unsupported gamedig monitor")
	}
}

func TestReportRequiresAuth(t *testing.T) {
	h := newExtHarness(t, &fakeReporter{}, nil)
	// No session -> 401.
	rec := h.request(http.MethodGet, "/api/v1/report/flakiest", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated flakiest = %d, want 401", rec.Code)
	}
}

// --- report window validation --------------------------------------------------

// TestReportRangeIsValidated pins the bounds on an explicit ?from=/?to= window.
//
// It used to be taken verbatim: an unparseable bound fell through to the default
// (hiding a caller's bug behind plausible numbers), from could sit after to, and
// an unbounded span let any authenticated user turn one request into a
// full-table scan — Flakiest and the SLA report run it once PER MONITOR, and
// Service.Uptime walks the range a day at a time looking for rollup gaps.
func TestReportRangeIsValidated(t *testing.T) {
	rep := &fakeReporter{uptime: report.UptimeResult{Up: 1, Total: 1, Uptime: 1}}
	h := newExtHarness(t, rep, nil)
	admin := h.setupAdmin()
	id := h.createMonitorFake(admin)
	base := "/api/v1/monitors/" + itoa64(id) + "/uptime"

	for _, tc := range []struct {
		name  string
		query string
	}{
		{"unparseable from", "?from=yesterday"},
		{"unparseable to", "?from=2026-01-01T00:00:00Z&to=tomorrow"},
		{"inverted window", "?from=2026-02-01T00:00:00Z&to=2026-01-01T00:00:00Z"},
		{"zero-length window", "?from=2026-01-01T00:00:00Z&to=2026-01-01T00:00:00Z"},
		{"span beyond the cap", "?from=1000-01-01T00:00:00Z&to=2026-01-01T00:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.requestWith(http.MethodGet, base+tc.query, nil, admin)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s = %d, want 400; body %s", tc.query, rec.Code, rec.Body.String())
			}
		})
	}

	// A well-formed window is still accepted.
	rec := h.requestWith(http.MethodGet, base+"?from=2026-01-01T00:00:00Z&to=2026-01-08T00:00:00Z", nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid window = %d, body %s", rec.Code, rec.Body.String())
	}
}

// A ?to= in the future is clamped to now rather than rejected: it is what a
// naive "end of today" date picker sends, and leaving it would make an outage
// still open at the end of the window report as lasting into the future.
func TestReportRangeClampsFutureTo(t *testing.T) {
	rep := &fakeReporter{uptime: report.UptimeResult{Up: 1, Total: 1, Uptime: 1}}
	h := newExtHarness(t, rep, nil)
	admin := h.setupAdmin()
	id := h.createMonitorFake(admin)

	future := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
	past := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	rec := h.requestWith(http.MethodGet,
		"/api/v1/monitors/"+itoa64(id)+"/uptime?from="+past+"&to="+future, nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("future to = %d, want 200 (clamped); body %s", rec.Code, rec.Body.String())
	}
}
