package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	monreg "github.com/Ariazonaa/statwarden/internal/monitor/registry"
	notifyreg "github.com/Ariazonaa/statwarden/internal/notify/registry"
	"github.com/Ariazonaa/statwarden/internal/report"
)

// --- fakes ---

type sentMail struct{ to, subject, text, html string }

// mailCapture is a Mailer that records every message it is asked to send.
type mailCapture struct {
	mu   sync.Mutex
	sent []sentMail
}

func (m *mailCapture) Send(_ context.Context, to, subject, text, html string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, sentMail{to, subject, text, html})
	return nil
}

func (m *mailCapture) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}

// fakeWSPub records the public status-page broadcasts the handlers make.
//
// The PAYLOAD is recorded, not just the event name. status-page:{slug} is a
// public topic that anonymous visitors of the page subscribe to, and the whole
// reason announceIncident applies publicCopy on the way out is that an
// unpublished retrospective must not go down it. A double that discarded the
// payload could not tell whether that guard was still there — the leak and the
// correct behaviour produced identical recordings.
type fakeWSPub struct {
	mu       sync.Mutex
	events   []string
	payloads []any
}

func (f *fakeWSPub) PublishStatusPage(slug, event string, payload any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, slug+":"+event)
	f.payloads = append(f.payloads, payload)
}

func (f *fakeWSPub) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.events)
}

// sent returns every payload broadcast so far, as JSON — which is the form the
// browser actually receives, and therefore the form an assertion about a leak
// has to be made against.
func (f *fakeWSPub) sent(t *testing.T) []string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.payloads))
	for _, p := range f.payloads {
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshal broadcast payload: %v", err)
		}
		out = append(out, string(raw))
	}
	return out
}

// statusHarness bundles the API handler with the fakes the status-page tests
// assert on.
type statusHarness struct {
	*harness
	wsPub *fakeWSPub
}

func newStatusHarness(t *testing.T) *statusHarness {
	t.Helper()
	store := NewMemStore()
	eng := newFakeEngine()
	mr := monreg.New()
	mr.Register(fakeChecker{})
	nr := notifyreg.New()
	nr.Register(fakeNotifier{})
	rep := &fakeReporter{uptime: report.UptimeResult{Up: 100, Total: 100, Uptime: 1.0}}
	wsPub := &fakeWSPub{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	maint := newMaintenanceProvider(store, log)
	h := New(Options{
		Logger:          log,
		Store:           store,
		Engine:          eng,
		MonitorRegistry: mr,
		NotifyRegistry:  nr,
		Reporter:        rep,
		WSPublisher:     wsPub,
		Maintenance:     maint,
		PublicBaseURL:   "https://statwarden.test",
	})
	return &statusHarness{
		harness: &harness{t: t, handler: h, store: store, engine: eng, maint: maint},
		wsPub:   wsPub,
	}
}

// createPage creates a status page via the given session and returns it.
func (h *harness) createPage(sess *session, body statusPageRequest) StatusPage {
	h.t.Helper()
	rec := h.requestWith(http.MethodPost, "/api/v1/status-pages", body, sess)
	if rec.Code != http.StatusCreated {
		h.t.Fatalf("create page = %d, body %s", rec.Code, rec.Body.String())
	}
	var p StatusPage
	mustJSON(h.t, rec, &p)
	return p
}

// getPublic performs an unauthenticated GET on a public status-page route.
func (h *harness) getPublic(target, host string) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if host != "" {
		req.Host = host
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func TestPublicStatusPageRendering(t *testing.T) {
	h := newStatusHarness(t)
	admin := h.setupAdmin()
	m1 := h.createMonitorFake(admin)
	published := true
	p := h.createPage(admin, statusPageRequest{
		Slug: "acme", Title: "Acme Status", Published: &published, MonitorIDs: []int64{m1},
	})
	if p.Slug != "acme" {
		t.Fatalf("slug = %q", p.Slug)
	}

	rec := h.getPublic("/api/v1/public/status/acme", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("public GET = %d, body %s", rec.Code, rec.Body.String())
	}
	var view PublicPageView
	mustJSON(t, rec, &view)
	if view.Title != "Acme Status" {
		t.Fatalf("title = %q", view.Title)
	}
	if len(view.Ungrouped) != 1 {
		t.Fatalf("ungrouped monitors = %d", len(view.Ungrouped))
	}
	if got := len(view.Ungrouped[0].Heatmap); got != heatmapDays {
		t.Fatalf("heatmap days = %d, want %d", got, heatmapDays)
	}
	if view.OverallStatus != OverallOperational {
		t.Fatalf("overall = %q, want operational", view.OverallStatus)
	}
}

func TestPublicStatusPageUnpublished(t *testing.T) {
	h := newStatusHarness(t)
	admin := h.setupAdmin()
	no := false
	h.createPage(admin, statusPageRequest{Slug: "hidden", Title: "Hidden", Published: &no})
	rec := h.getPublic("/api/v1/public/status/hidden", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unpublished GET = %d, want 404", rec.Code)
	}
}

func TestPublicStatusPagePasswordProtected(t *testing.T) {
	h := newStatusHarness(t)
	admin := h.setupAdmin()
	pub := true
	h.createPage(admin, statusPageRequest{Slug: "secret", Title: "Secret", Published: &pub, Password: "hunter2"})

	rec := h.getPublic("/api/v1/public/status/secret", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-password GET = %d, want 401", rec.Code)
	}
	if rec.Header().Get("X-Password-Required") != "true" {
		t.Fatalf("missing X-Password-Required header")
	}
	rec = h.getPublic("/api/v1/public/status/secret?password=hunter2", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("password GET = %d, body %s", rec.Code, rec.Body.String())
	}
}

func TestStatusPageViewCounter(t *testing.T) {
	h := newStatusHarness(t)
	admin := h.setupAdmin()
	pub := true
	p := h.createPage(admin, statusPageRequest{Slug: "views", Title: "Views", Published: &pub})
	for i := 0; i < 3; i++ {
		h.getPublic("/api/v1/public/status/views", "")
	}
	reread, _ := h.store.GetStatusPage(context.Background(), p.ID)
	if reread.ViewCount != 3 {
		t.Fatalf("view count = %d, want 3", reread.ViewCount)
	}
}

func TestCustomDomainResolution(t *testing.T) {
	h := newStatusHarness(t)
	admin := h.setupAdmin()
	pub := true
	h.createPage(admin, statusPageRequest{
		Slug: "corp", Title: "Corp", Published: &pub, CustomDomain: "status.corp.example",
	})
	rec := h.getPublic("/api/v1/public/status", "status.corp.example:443")
	if rec.Code != http.StatusOK {
		t.Fatalf("domain GET = %d, body %s", rec.Code, rec.Body.String())
	}
	var view PublicPageView
	mustJSON(t, rec, &view)
	if view.Slug != "corp" {
		t.Fatalf("resolved slug = %q, want corp", view.Slug)
	}
	if rec := h.getPublic("/api/v1/public/status", "nope.example"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown host = %d, want 404", rec.Code)
	}
}

func TestIncidentLifecycle(t *testing.T) {
	h := newStatusHarness(t)
	admin := h.setupAdmin()
	pub := true
	p := h.createPage(admin, statusPageRequest{Slug: "life", Title: "Life", Published: &pub})

	rec := h.requestWith(http.MethodPost, "/api/v1/status-pages/"+itoa64(p.ID)+"/incidents", incidentRequest{
		Title: "API errors", Body: "We are investigating.", Severity: "major", Status: IncidentInvestigating,
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create incident = %d, body %s", rec.Code, rec.Body.String())
	}
	var inc Incident
	mustJSON(t, rec, &inc)
	if len(inc.Updates) != 1 {
		t.Fatalf("opening update count = %d, want 1", len(inc.Updates))
	}

	for _, st := range []string{IncidentIdentified, IncidentMonitoring, IncidentResolved} {
		rec := h.requestWith(http.MethodPost,
			"/api/v1/status-pages/"+itoa64(p.ID)+"/incidents/"+itoa64(inc.ID)+"/updates",
			incidentUpdateRequest{Status: st, Body: "moving to " + st}, admin)
		if rec.Code != http.StatusCreated {
			t.Fatalf("update to %s = %d, body %s", st, rec.Code, rec.Body.String())
		}
		mustJSON(t, rec, &inc)
	}
	if inc.Status != IncidentResolved {
		t.Fatalf("final status = %q", inc.Status)
	}
	if inc.ResolvedAt == nil {
		t.Fatalf("resolved_at not set")
	}
	if len(inc.Updates) != 4 {
		t.Fatalf("timeline length = %d, want 4", len(inc.Updates))
	}
	if h.wsPub.count() != 4 {
		t.Fatalf("ws events = %d, want 4", h.wsPub.count())
	}
}

// TestSubscriberStoreRoundTrip pins the subscriber Store methods that are kept
// dormant after the e-mail-subscription feature was archived (see
// kept out of the build), so a later reactivation lands on a tested store layer
// rather than an unexercised one. It drives create → verify → lookup → delete
// directly against the store.
func TestSubscriberStoreRoundTrip(t *testing.T) {
	h := newStatusHarness(t)
	admin := h.setupAdmin()
	pub := true
	p := h.createPage(admin, statusPageRequest{Slug: "subs-store", Title: "Subs", Published: &pub})
	ctx := context.Background()

	created, err := h.store.CreateSubscriber(ctx, Subscriber{
		StatusPageID:     p.ID,
		Email:            "user@example.com",
		VerifyToken:      "vtok",
		UnsubscribeToken: "utok",
	})
	if err != nil {
		t.Fatalf("CreateSubscriber: %v", err)
	}
	if created.ID == 0 || created.Verified {
		t.Fatalf("new subscriber = %+v, want an assigned id and unverified", created)
	}

	// Same page + address (case-insensitive) is a conflict, not a second row.
	if _, err := h.store.CreateSubscriber(ctx, Subscriber{StatusPageID: p.ID, Email: "USER@example.com"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate subscribe err = %v, want ErrConflict", err)
	}

	// Verify-token lookup resolves the pending row; before opt-in nothing is verified.
	if got, err := h.store.GetSubscriberByVerifyToken(ctx, "vtok"); err != nil || got.ID != created.ID {
		t.Fatalf("GetSubscriberByVerifyToken = %+v, %v", got, err)
	}
	if before, _ := h.store.ListSubscribers(ctx, p.ID, true); len(before) != 0 {
		t.Fatalf("verified before opt-in = %d, want 0", len(before))
	}

	verified, err := h.store.VerifySubscriber(ctx, created.ID)
	if err != nil || !verified.Verified || verified.VerifiedAt == nil {
		t.Fatalf("VerifySubscriber = %+v, %v", verified, err)
	}
	if after, _ := h.store.ListSubscribers(ctx, p.ID, true); len(after) != 1 {
		t.Fatalf("verified after opt-in = %d, want 1", len(after))
	}

	// Unsubscribe-token lookup still resolves after verification; delete clears it.
	sub, err := h.store.GetSubscriberByUnsubscribeToken(ctx, "utok")
	if err != nil || sub.ID != created.ID {
		t.Fatalf("GetSubscriberByUnsubscribeToken = %+v, %v", sub, err)
	}
	if err := h.store.DeleteSubscriber(ctx, sub.ID); err != nil {
		t.Fatalf("DeleteSubscriber: %v", err)
	}
	if remaining, _ := h.store.ListSubscribers(ctx, p.ID, false); len(remaining) != 0 {
		t.Fatalf("subscribers after delete = %d, want 0", len(remaining))
	}
}

func TestStatusPageGroupedLayout(t *testing.T) {
	h := newStatusHarness(t)
	admin := h.setupAdmin()
	m1 := h.createMonitorFake(admin)
	m2 := h.createMonitorNamed(admin, "second", "fake")
	pub := true
	show := true
	p := h.createPage(admin, statusPageRequest{
		Slug: "grp", Title: "Grouped", Published: &pub,
		Groups: []statusPageGroupInput{{Name: "API", SortOrder: 0}},
		Monitors: []statusPageMonitorInput{
			{MonitorID: m1, GroupName: "API", ShowUptime: &show},
			{MonitorID: m2, ShowUptime: &show},
		},
	})
	if len(p.Groups) != 1 {
		t.Fatalf("groups persisted = %d, want 1", len(p.Groups))
	}

	rec := h.getPublic("/api/v1/public/status/grp", "")
	var view PublicPageView
	mustJSON(t, rec, &view)
	if len(view.Groups) != 1 || view.Groups[0].Name != "API" {
		t.Fatalf("group view = %+v", view.Groups)
	}
	if len(view.Groups[0].Monitors) != 1 {
		t.Fatalf("API group monitors = %d, want 1", len(view.Groups[0].Monitors))
	}
	if len(view.Ungrouped) != 1 {
		t.Fatalf("ungrouped = %d, want 1", len(view.Ungrouped))
	}
}

func TestStatusPageLogoPathNotClientSettable(t *testing.T) {
	h := newStatusHarness(t)
	admin := h.setupAdmin()

	cases := []struct {
		name string
		sent string
	}{
		{"external url", "https://evil.example/x.png"},
		{"traversal path", "/uploads/logos/../../etc/passwd"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// createPage fails the test on anything but 201, which also pins that
			// logo_path is still accepted-and-ignored rather than removed — removing
			// the field would trip DisallowUnknownFields for existing clients.
			p := h.createPage(admin, statusPageRequest{
				Slug: "np" + itoa64(int64(i)), Title: "No Logo", LogoPath: tc.sent,
			})
			if p.LogoPath != "" {
				t.Fatalf("logo path = %q, want empty", p.LogoPath)
			}
			rec := h.requestWith(http.MethodGet, "/api/v1/status-pages/"+itoa64(p.ID), nil, admin)
			var got StatusPage
			mustJSON(t, rec, &got)
			if got.LogoPath != "" {
				t.Fatalf("stored logo path = %q, want empty", got.LogoPath)
			}
		})
	}
}

// TestStatusPageLogoSurvivesPageUpdate pins that logo_path is server-owned: a
// full-replace PUT neither clears it nor lets a client redirect it. This runs
// against MemStore only, so its preservation line and the removal of logo_path
// from the PgStore UPDATE must be reviewed together or the stores diverge.
func TestStatusPageLogoSurvivesPageUpdate(t *testing.T) {
	h := newLogoHarness(t)
	admin := h.setupAdmin()
	p := h.createPage(admin, statusPageRequest{Slug: "keep", Title: "Keep"})

	rec := h.uploadLogo(admin, p.ID, "logo.png", pngBytes(t, 64, 64))
	if rec.Code != http.StatusOK {
		t.Fatalf("upload = %d, body %s", rec.Code, rec.Body.String())
	}
	var uploaded StatusPage
	mustJSON(t, rec, &uploaded)

	target := "/api/v1/status-pages/" + itoa64(p.ID)

	// logo_path omitted entirely.
	rec = h.requestWith(http.MethodPut, target, statusPageRequest{Slug: "keep", Title: "Renamed"}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d, body %s", rec.Code, rec.Body.String())
	}
	var updated StatusPage
	mustJSON(t, rec, &updated)
	if updated.LogoPath != uploaded.LogoPath {
		t.Fatalf("logo path after omitting the key = %q, want %q", updated.LogoPath, uploaded.LogoPath)
	}

	// logo_path forged to point somewhere else.
	rec = h.requestWith(http.MethodPut, target, statusPageRequest{
		Slug: "keep", Title: "Renamed", LogoPath: "https://evil.example/x.png",
	}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d, body %s", rec.Code, rec.Body.String())
	}
	mustJSON(t, rec, &updated)
	if updated.LogoPath != uploaded.LogoPath {
		t.Fatalf("logo path after a forged key = %q, want %q", updated.LogoPath, uploaded.LogoPath)
	}
	if names := h.files(); len(names) != 1 {
		t.Fatalf("uploads dir = %v, want the uploaded file untouched", names)
	}
}

// TestStatusPageLogoPublicViewSanitizes seeds a row the way a pre-upgrade
// database looks — logo_path was free client text — and proves the public view
// neutralises it on read, without a data migration.
func TestStatusPageLogoPublicViewSanitizes(t *testing.T) {
	h := newStatusHarness(t)
	admin := h.setupAdmin()
	pub := true
	p := h.createPage(admin, statusPageRequest{Slug: "legacy", Title: "Legacy", Published: &pub})

	if _, _, err := h.store.SetStatusPageLogo(context.Background(), p.ID, "https://evil.example/x.png"); err != nil {
		t.Fatalf("seed legacy logo path: %v", err)
	}

	rec := h.getPublic("/api/v1/public/status/legacy", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("public GET = %d, body %s", rec.Code, rec.Body.String())
	}
	var view PublicPageView
	mustJSON(t, rec, &view)
	if view.LogoPath != "" {
		t.Fatalf("public logo path = %q, want empty", view.LogoPath)
	}
}

func TestStatusPageAccentColorRoundTrip(t *testing.T) {
	h := newStatusHarness(t)
	admin := h.setupAdmin()

	cases := []struct {
		name string
		sent string
		want string
	}{
		{"uppercase six digit", "#FF8800", "#ff8800"},
		{"shorthand", "#f80", "#ff8800"},
		{"omitted", "", ""},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pub := true
			slug := "ac" + itoa64(int64(i))
			p := h.createPage(admin, statusPageRequest{
				Slug: slug, Title: "Accent", Published: &pub, AccentColor: tc.sent,
			})
			if p.AccentColor != tc.want {
				t.Fatalf("accent colour = %q, want %q", p.AccentColor, tc.want)
			}

			rec := h.getPublic("/api/v1/public/status/"+slug, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("public GET = %d, body %s", rec.Code, rec.Body.String())
			}
			var view PublicPageView
			mustJSON(t, rec, &view)
			if view.AccentColor != tc.want {
				t.Fatalf("public accent colour = %q, want %q", view.AccentColor, tc.want)
			}
		})
	}
}

// TestStatusPageAccentColorRejected pins that a colour normalizeHexColor cannot
// parse is a validation error rather than a silent drop: the old behaviour
// answered 200 and kept the previously stored colour, so a client that sent a
// typo or an injection attempt was told it had succeeded.
func TestStatusPageAccentColorRejected(t *testing.T) {
	h := newStatusHarness(t)
	admin := h.setupAdmin()

	pub := true
	p := h.createPage(admin, statusPageRequest{
		Slug: "reject", Title: "Reject", Published: &pub, AccentColor: "#0ea5e9",
	})
	target := "/api/v1/status-pages/" + itoa64(p.ID)

	cases := []struct {
		name string
		sent string
	}{
		{"named colour", "red"},
		{"css injection", "red; background:url(x)"},
		{"non hex digits", "#GGGGGG"},
		{"truncated", "#0ea5e"},
		{"scheme", "javascript:alert(1)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.requestWith(http.MethodPut, target, statusPageRequest{
				Slug: "reject", Title: "Reject", Published: &pub, AccentColor: tc.sent,
			}, admin)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("update = %d, body %s", rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
				t.Fatalf("content type = %q, want problem+json", ct)
			}
			if body := rec.Body.String(); !strings.Contains(body, "accent_color") {
				t.Fatalf("problem detail %s does not name the field", body)
			}

			// The stored colour must be untouched by the rejected write.
			rec = h.requestWith(http.MethodGet, target, nil, admin)
			var got StatusPage
			mustJSON(t, rec, &got)
			if got.AccentColor != "#0ea5e9" {
				t.Fatalf("stored accent colour = %q, want %q", got.AccentColor, "#0ea5e9")
			}
		})
	}

	// Creating with the same input is rejected too, so the invalid value never
	// reaches the store by either route.
	rec := h.requestWith(http.MethodPost, "/api/v1/status-pages", statusPageRequest{
		Slug: "reject2", Title: "Reject", Published: &pub, AccentColor: "red; background:url(x)",
	}, admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create = %d, body %s", rec.Code, rec.Body.String())
	}
}

// TestStatusPageAccentColorCleared pins that an empty accent_color stays a
// clear rather than becoming a validation error.
func TestStatusPageAccentColorCleared(t *testing.T) {
	h := newStatusHarness(t)
	admin := h.setupAdmin()

	pub := true
	p := h.createPage(admin, statusPageRequest{
		Slug: "clear", Title: "Clear", Published: &pub, AccentColor: "#0ea5e9",
	})
	target := "/api/v1/status-pages/" + itoa64(p.ID)

	rec := h.requestWith(http.MethodPut, target, statusPageRequest{
		Slug: "clear", Title: "Clear", Published: &pub, AccentColor: "",
	}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d, body %s", rec.Code, rec.Body.String())
	}
	var updated StatusPage
	mustJSON(t, rec, &updated)
	if updated.AccentColor != "" {
		t.Fatalf("accent colour after clear = %q, want empty", updated.AccentColor)
	}
}

// TestPublicPageCollectionsAreNeverNull pins that every collection on the
// public payload is an array. encoding/json renders a nil slice as null and the
// public page reads .length on all five without guarding, so a page with no
// groups and no incidents rendered a completely blank screen in the browser
// while the API still answered 200.
func TestPublicPageCollectionsAreNeverNull(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	published := true
	rec := h.requestWith(http.MethodPost, "/api/v1/status-pages", statusPageRequest{
		Slug: "bare", Title: "Bare", Published: &published,
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create page = %d, body %s", rec.Code, rec.Body.String())
	}

	rec = h.request(http.MethodGet, "/api/v1/public/status/bare", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("public page = %d, body %s", rec.Code, rec.Body.String())
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, field := range []string{"groups", "ungrouped", "active_incidents", "recent_incidents", "maintenance"} {
		got, ok := raw[field]
		if !ok {
			t.Errorf("%s missing from the public payload", field)
			continue
		}
		if string(got) == "null" {
			t.Errorf("%s = null, want an array", field)
		}
	}
}

// TestPublicPageIdentifiersAreUnique pins that the public payload can be keyed
// safely. MONITOR names are not unique — monitors.name has no unique index — so
// a page holding two components with the same name used to make Svelte throw
// each_key_duplicate and render a blank page while the API still answered 200.
//
// Group names are a separate story: they are the join key a monitor is attached
// by, so duplicates are now rejected on save (see
// TestStatusPageRejectsDuplicateGroupNames). This test therefore uses distinct
// group names and duplicate MONITOR names, which is the case that remains real.
func TestPublicPageIdentifiersAreUnique(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	var ids []int64
	for _, name := range []string{"API", "API", "Web"} {
		rec := h.requestWith(http.MethodPost, "/api/v1/monitors",
			monitorRequest{Name: name, Type: "fake", Interval: 30}, admin)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create monitor = %d, body %s", rec.Code, rec.Body.String())
		}
		var m struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatalf("decode monitor: %v", err)
		}
		ids = append(ids, m.ID)
	}

	published := true
	rec := h.requestWith(http.MethodPost, "/api/v1/status-pages", statusPageRequest{
		Slug: "dup", Title: "Dup", Published: &published,
		MonitorIDs: ids,
		Groups:     []statusPageGroupInput{{Name: "Core", SortOrder: 0}, {Name: "Edge", SortOrder: 1}},
		Monitors: []statusPageMonitorInput{
			{MonitorID: ids[0], GroupName: "Core", SortOrder: 0},
			{MonitorID: ids[1], SortOrder: 1},
			{MonitorID: ids[2], SortOrder: 2},
		},
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create page = %d, body %s", rec.Code, rec.Body.String())
	}

	rec = h.request(http.MethodGet, "/api/v1/public/status/dup", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("public page = %d, body %s", rec.Code, rec.Body.String())
	}
	var view struct {
		Groups []struct {
			ID       int64 `json:"id"`
			Monitors []struct {
				ID int64 `json:"id"`
			} `json:"monitors"`
		} `json:"groups"`
		Ungrouped []struct {
			ID int64 `json:"id"`
		} `json:"ungrouped"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode public page: %v", err)
	}

	seenGroup := map[int64]bool{}
	for _, g := range view.Groups {
		if g.ID == 0 {
			t.Error("group has no id; the client cannot key on anything unique")
		}
		if seenGroup[g.ID] {
			t.Errorf("duplicate group id %d", g.ID)
		}
		seenGroup[g.ID] = true
	}

	seenMon := map[int64]bool{}
	for _, g := range view.Groups {
		for _, m := range g.Monitors {
			if m.ID == 0 || seenMon[m.ID] {
				t.Errorf("monitor id %d is zero or duplicated", m.ID)
			}
			seenMon[m.ID] = true
		}
	}
	for _, m := range view.Ungrouped {
		if m.ID == 0 || seenMon[m.ID] {
			t.Errorf("monitor id %d is zero or duplicated", m.ID)
		}
		seenMon[m.ID] = true
	}
	if len(seenMon) != len(ids) {
		t.Errorf("saw %d distinct monitors, want %d", len(seenMon), len(ids))
	}
}

// TestPublicPageSelectableRange pins the ?range= contract: each accepted token
// picks the documented bucket unit and bar count, an omitted or unknown range
// falls back to the 90-day default, and every bar carries its own ascending
// bucket boundary so the client can label any granularity.
func TestPublicPageSelectableRange(t *testing.T) {
	h := newStatusHarness(t)
	admin := h.setupAdmin()
	m1 := h.createMonitorFake(admin)
	pub := true
	h.createPage(admin, statusPageRequest{
		Slug: "range", Title: "Range", Published: &pub, MonitorIDs: []int64{m1},
	})

	cases := []struct {
		name   string
		query  string
		want   string
		bucket string
		bars   int
	}{
		{"default omitted", "", Range90d, bucketDay, 90},
		{"24h hourly", "?range=24h", Range24h, bucketHour, 24},
		{"7d hourly", "?range=7d", Range7d, bucketHour, 24 * 7},
		{"30d daily", "?range=30d", Range30d, bucketDay, 30},
		{"90d daily", "?range=90d", Range90d, bucketDay, 90},
		{"1y monthly", "?range=1y", Range1y, bucketMonth, 12},
		{"unknown falls back", "?range=bogus", Range90d, bucketDay, 90},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.getPublic("/api/v1/public/status/range"+tc.query, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("public GET = %d, body %s", rec.Code, rec.Body.String())
			}
			var view PublicPageView
			mustJSON(t, rec, &view)
			if view.Range != tc.want {
				t.Fatalf("range = %q, want %q", view.Range, tc.want)
			}
			if view.Bucket != tc.bucket {
				t.Fatalf("bucket = %q, want %q", view.Bucket, tc.bucket)
			}
			if len(view.Ungrouped) != 1 {
				t.Fatalf("ungrouped = %d, want 1", len(view.Ungrouped))
			}
			bars := view.Ungrouped[0].Heatmap
			if len(bars) != tc.bars {
				t.Fatalf("bars = %d, want %d", len(bars), tc.bars)
			}
			if bars[0].Start.IsZero() {
				t.Fatal("first bar has no start timestamp")
			}
			// Oldest-first, strictly ascending bucket boundaries.
			for i := 1; i < len(bars); i++ {
				if !bars[i-1].Start.Before(bars[i].Start) {
					t.Fatalf("bar %d start %v not before %v", i, bars[i-1].Start, bars[i].Start)
				}
			}
		})
	}
}

// TestPublicPageMaintenanceContract pins the visitor-facing maintenance shape:
// a window running now is reported with body (not description), the active
// flag, its RFC3339 bounds and the monitor ids it covers — exactly the fields
// the frontend contract fixes.
func TestPublicPageMaintenanceContract(t *testing.T) {
	h := newStatusHarness(t)
	admin := h.setupAdmin()
	mon := h.createMonitorFake(admin)
	pub := true
	h.createPage(admin, statusPageRequest{
		Slug: "maint", Title: "Maint", Published: &pub, MonitorIDs: []int64{mon},
	})

	// A single window in effect right now: started an hour ago, ends in an hour.
	start := time.Now().UTC().Add(-time.Hour)
	end := start.Add(2 * time.Hour)
	h.createWindow(admin, maintenanceRequest{
		Title: "DB upgrade", Description: "Rolling database upgrade.",
		StartsAt: &start, EndsAt: &end, MonitorIDs: []int64{mon},
	})

	rec := h.getPublic("/api/v1/public/status/maint", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("public GET = %d, body %s", rec.Code, rec.Body.String())
	}

	// Decode against the exact wire field names the frontend relies on.
	var wire struct {
		Maintenance []struct {
			ID         int64   `json:"id"`
			Title      string  `json:"title"`
			Body       string  `json:"body"`
			StartsAt   string  `json:"starts_at"`
			EndsAt     string  `json:"ends_at"`
			Active     bool    `json:"active"`
			MonitorIDs []int64 `json:"monitor_ids"`
		} `json:"maintenance"`
		OverallStatus string `json:"overall_status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(wire.Maintenance) != 1 {
		t.Fatalf("maintenance = %+v, want the running window", wire.Maintenance)
	}
	mw := wire.Maintenance[0]
	if mw.Title != "DB upgrade" {
		t.Fatalf("title = %q", mw.Title)
	}
	if mw.Body != "Rolling database upgrade." {
		t.Fatalf("body = %q, want the description mapped onto body", mw.Body)
	}
	if !mw.Active {
		t.Fatal("active = false, want true: the window is in effect now")
	}
	if _, err := time.Parse(time.RFC3339, mw.StartsAt); err != nil {
		t.Fatalf("starts_at %q not RFC3339: %v", mw.StartsAt, err)
	}
	if _, err := time.Parse(time.RFC3339, mw.EndsAt); err != nil {
		t.Fatalf("ends_at %q not RFC3339: %v", mw.EndsAt, err)
	}
	if len(mw.MonitorIDs) != 1 || mw.MonitorIDs[0] != mon {
		t.Fatalf("monitor_ids = %v, want [%d]", mw.MonitorIDs, mon)
	}
	if wire.OverallStatus != OverallMaintenance {
		t.Fatalf("overall = %q, want %q", wire.OverallStatus, OverallMaintenance)
	}
}

// TestCustomDomainUnlockCookieIsUsable pins the SCOPE of the page-unlock
// cookie for a page reached by custom domain. Those pages are served from
// /api/v1/public/status with no slug anywhere in the URL, but the cookie used to
// be scoped to /api/v1/public/status/{slug} — a path the browser never requests
// — so it was never sent back and the visitor was asked for the password on
// every single load.
func TestCustomDomainUnlockCookieIsUsable(t *testing.T) {
	h := newStatusHarness(t)
	admin := h.setupAdmin()
	pub := true
	h.createPage(admin, statusPageRequest{
		Slug: "corp", Title: "Corp", Published: &pub,
		CustomDomain: "status.corp.example", Password: "hunter2",
	})

	rec := h.getPublic("/api/v1/public/status?password=hunter2", "status.corp.example")
	if rec.Code != http.StatusOK {
		t.Fatalf("password GET = %d, body %s", rec.Code, rec.Body.String())
	}
	var unlock *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == pageUnlockCookie {
			unlock = c
		}
	}
	if unlock == nil {
		t.Fatal("no unlock cookie issued")
	}
	if unlock.Path != publicStatusPrefix {
		t.Fatalf("cookie path = %q, want %q so the domain-routed URL matches it",
			unlock.Path, publicStatusPrefix)
	}

	// The decisive assertion: the cookie ALONE, with no password, unlocks the
	// page on the next request.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/public/status", nil)
	req.Host = "status.corp.example"
	req.AddCookie(unlock)
	replay := httptest.NewRecorder()
	h.handler.ServeHTTP(replay, req)
	if replay.Code != http.StatusOK {
		t.Fatalf("cookie-only GET = %d, want 200; body %s", replay.Code, replay.Body.String())
	}
}

// The slug-addressed form keeps the narrower scope, so two protected pages on
// one host cannot clobber each other's cookie.
func TestSlugUnlockCookieStaysSlugScoped(t *testing.T) {
	h := newStatusHarness(t)
	admin := h.setupAdmin()
	pub := true
	h.createPage(admin, statusPageRequest{
		Slug: "secret", Title: "Secret", Published: &pub, Password: "hunter2",
	})

	rec := h.getPublic("/api/v1/public/status/secret?password=hunter2", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("password GET = %d, body %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == pageUnlockCookie {
			if want := publicStatusPrefix + "/secret"; c.Path != want {
				t.Fatalf("cookie path = %q, want %q", c.Path, want)
			}
			return
		}
	}
	t.Fatal("no unlock cookie issued")
}

// TestStatusPageSlugsForMonitor pins the lookup behind the live status-page
// poke: published pages that hold the monitor, and nothing else. An unpublished
// page answers 404 to the public endpoint, so poking it could only make
// anonymous subscribers re-fetch a 404.
func TestStatusPageSlugsForMonitor(t *testing.T) {
	h := newStatusHarness(t)
	admin := h.setupAdmin()
	shown := h.createMonitorFake(admin)
	other := h.createMonitorFake(admin)
	pub, priv := true, false
	h.createPage(admin, statusPageRequest{
		Slug: "shown", Title: "Shown", Published: &pub, MonitorIDs: []int64{shown},
	})
	h.createPage(admin, statusPageRequest{
		Slug: "unpublished", Title: "Hidden", Published: &priv, MonitorIDs: []int64{shown},
	})
	h.createPage(admin, statusPageRequest{
		Slug: "elsewhere", Title: "Elsewhere", Published: &pub, MonitorIDs: []int64{other},
	})

	got, err := h.store.StatusPageSlugsForMonitor(context.Background(), shown)
	if err != nil {
		t.Fatalf("StatusPageSlugsForMonitor: %v", err)
	}
	if len(got) != 1 || got[0] != "shown" {
		t.Fatalf("slugs = %v, want [shown]", got)
	}
}

// A password in the query string is answered with the deprecation headers, and
// the supported paths are not.
//
// The distinction matters because the party that has to change is usually not the
// operator reading this instance's logs — it is whoever pasted the link into an
// intranet page or a bookmark years ago. A Sunset header travels with the
// response to them.
func TestQueryPasswordCarriesDeprecationHeaders(t *testing.T) {
	h := newStatusHarness(t)
	admin := h.setupAdmin()
	pub := true
	h.createPage(admin, statusPageRequest{
		Slug: "sunset", Title: "Sunset", Published: &pub, Password: "hunter2",
	})

	rec := h.getPublic("/api/v1/public/status/sunset?password=hunter2", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("query-password GET = %d, want 200 — it must keep working", rec.Code)
	}
	if got := rec.Header().Get("Deprecation"); got != "true" {
		t.Fatalf("Deprecation = %q, want true", got)
	}
	if _, err := http.ParseTime(rec.Header().Get("Sunset")); err != nil {
		t.Fatalf("Sunset = %q is unparsable: %v", rec.Header().Get("Sunset"), err)
	}

	// The header path is the supported one and must advertise nothing.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/public/status/sunset", nil)
	req.Header.Set("X-Page-Password", "hunter2")
	viaHeader := httptest.NewRecorder()
	h.handler.ServeHTTP(viaHeader, req)
	if viaHeader.Code != http.StatusOK {
		t.Fatalf("X-Page-Password GET = %d, want 200", viaHeader.Code)
	}
	if got := viaHeader.Header().Get("Deprecation"); got != "" {
		t.Fatalf("the header path advertised Deprecation = %q", got)
	}

	// An unprotected page has no credential to deprecate.
	h.createPage(admin, statusPageRequest{Slug: "open", Title: "Open", Published: &pub})
	open := h.getPublic("/api/v1/public/status/open", "")
	if got := open.Header().Get("Sunset"); got != "" {
		t.Fatalf("an unprotected page advertised Sunset = %q", got)
	}
}

// An incident is addressed under the page it belongs to, and nowhere else.
//
// All four {incID} handlers carry both ids in the path and read only the
// incident's, so /status-pages/<other>/incidents/<id> acted on the incident
// regardless of which page it belonged to. Never a privilege boundary — a writer
// may edit every page — but the caller asked about one page and was answered
// about another, and the audit entry recorded the id from the URL.
func TestIncidentIsScopedToItsStatusPage(t *testing.T) {
	h := newStatusHarness(t)
	admin := h.setupAdmin()
	pub := true
	owner := h.createPage(admin, statusPageRequest{Slug: "owner", Title: "Owner", Published: &pub})
	other := h.createPage(admin, statusPageRequest{Slug: "other", Title: "Other", Published: &pub})

	rec := h.requestWith(http.MethodPost, "/api/v1/status-pages/"+itoa64(owner.ID)+"/incidents",
		incidentRequest{Title: "Owned", Body: "b", Severity: "minor", Status: IncidentInvestigating}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create incident = %d, body %s", rec.Code, rec.Body.String())
	}
	var inc Incident
	mustJSON(t, rec, &inc)

	wrong := "/api/v1/status-pages/" + itoa64(other.ID) + "/incidents/" + itoa64(inc.ID)
	right := "/api/v1/status-pages/" + itoa64(owner.ID) + "/incidents/" + itoa64(inc.ID)

	// Reading, editing, appending and deleting under the wrong page are all 404.
	if got := h.requestWith(http.MethodGet, wrong, nil, admin); got.Code != http.StatusNotFound {
		t.Errorf("GET under the wrong page = %d, want 404", got.Code)
	}
	if got := h.requestWith(http.MethodPut, wrong, incidentRequest{
		Title: "Hijacked", Severity: "minor", Status: IncidentInvestigating,
	}, admin); got.Code != http.StatusNotFound {
		t.Errorf("PUT under the wrong page = %d, want 404", got.Code)
	}
	if got := h.requestWith(http.MethodPost, wrong+"/updates", incidentUpdateRequest{
		Status: IncidentIdentified, Body: "from elsewhere",
	}, admin); got.Code != http.StatusNotFound {
		t.Errorf("POST updates under the wrong page = %d, want 404", got.Code)
	}
	if got := h.requestWith(http.MethodDelete, wrong, nil, admin); got.Code != http.StatusNotFound {
		t.Errorf("DELETE under the wrong page = %d, want 404", got.Code)
	}

	// The incident survived all of that, and its own page still reaches it.
	if got := h.requestWith(http.MethodGet, right, nil, admin); got.Code != http.StatusOK {
		t.Fatalf("GET under the owning page = %d, want 200", got.Code)
	}
	if got := h.requestWith(http.MethodDelete, right, nil, admin); got.Code != http.StatusNoContent {
		t.Fatalf("DELETE under the owning page = %d, want 204", got.Code)
	}
}

// Deleting an incident announces it, like every other change to one.
//
// Editing was fixed to announce; deleting was not. A removed incident therefore
// stayed on every open public status page until somebody reloaded — and it is the
// longest-lived stale state of the three, because nothing will ever touch that
// incident again to correct it.
func TestDeletingAnIncidentAnnouncesIt(t *testing.T) {
	h := newStatusHarness(t)
	admin := h.setupAdmin()
	pub := true
	p := h.createPage(admin, statusPageRequest{Slug: "gone", Title: "Gone", Published: &pub})

	rec := h.requestWith(http.MethodPost, "/api/v1/status-pages/"+itoa64(p.ID)+"/incidents",
		incidentRequest{Title: "Transient", Body: "b", Severity: "minor", Status: IncidentInvestigating}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create incident = %d", rec.Code)
	}
	var inc Incident
	mustJSON(t, rec, &inc)

	before := h.wsPub.count()
	got := h.requestWith(http.MethodDelete,
		"/api/v1/status-pages/"+itoa64(p.ID)+"/incidents/"+itoa64(inc.ID), nil, admin)
	if got.Code != http.StatusNoContent {
		t.Fatalf("delete incident = %d, body %s", got.Code, got.Body.String())
	}
	if h.wsPub.count() == before {
		t.Fatal("deleting an incident announced nothing; every open status page keeps " +
			"showing it until a manual reload")
	}
}

// walkNulls records the path of every JSON null in a decoded payload.
func walkNulls(path string, v any, out *[]string) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if val == nil {
				*out = append(*out, path+"."+k)
				continue
			}
			walkNulls(path+"."+k, val, out)
		}
	case []any:
		for _, val := range t {
			walkNulls(path+"[]", val, out)
		}
	}
}

// Nothing in the public payload is null — including the collections NESTED inside
// groups and incidents.
//
// An existing test pins the five top-level collections, and that is where the
// initialisation in buildPublicPageView stops. Two collections a level deeper were
// nil:
//
//   - groups[].monitors, for a group with nothing in it. The public page iterates it
//     with {#each group.monitors} and flatMaps it across every group, and the
//     TypeScript type declares `monitors: PublicMonitorView[]` with no nullable — so
//     one empty group did not degrade the page, it broke the render. Only against the
//     real server: the in-browser mock answers [].
//   - active_incidents[].monitor_ids, for an incident that names no specific monitor.
//
// This walks the WHOLE decoded payload rather than naming fields, so the next
// collection added a level down is covered without anyone remembering to add it.
func TestPublicPayloadHasNoNullsAnywhere(t *testing.T) {
	h := newStatusHarness(t)
	admin := h.setupAdmin()
	pub := true
	p := h.createPage(admin, statusPageRequest{
		Slug: "nonulls", Title: "No nulls", Published: &pub,
		// A group with no monitors in it, which is what used to produce the null.
		Groups: []statusPageGroupInput{{Name: "Unstaffed", SortOrder: 0}},
	})
	// An incident naming no specific monitor.
	rec := h.requestWith(http.MethodPost, "/api/v1/status-pages/"+itoa64(p.ID)+"/incidents",
		incidentRequest{Title: "Investigating", Body: "b", Severity: "minor",
			Status: IncidentInvestigating}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create incident = %d, body %s", rec.Code, rec.Body.String())
	}

	got := h.getPublic("/api/v1/public/status/nonulls", "")
	if got.Code != http.StatusOK {
		t.Fatalf("public GET = %d, body %s", got.Code, got.Body.String())
	}
	var decoded any
	if err := json.Unmarshal(got.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode public payload: %v", err)
	}
	var nulls []string
	walkNulls("", decoded, &nulls)
	for _, n := range nulls {
		t.Errorf("public payload is null at %s — the page reads these without guarding, "+
			"and the TypeScript types declare them non-nullable", n)
	}
}
