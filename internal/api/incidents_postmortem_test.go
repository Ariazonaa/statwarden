package api

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// pmFixture is a status page with one incident on it, plus the admin session.
type pmFixture struct {
	h     *harness
	admin *session
	page  StatusPage
	inc   Incident
	// clock backs the server's now(). Tests that care about a timestamp advance
	// it, so an assertion about WHEN something happened is not competing with the
	// wall clock's millisecond noise.
	clock *time.Time
}

func newPostmortemFixture(t *testing.T) *pmFixture {
	t.Helper()
	clock := time.Date(2026, 3, 14, 9, 10, 0, 0, time.UTC)
	h := newHarnessTweaked(t, func(o *Options) {
		o.Now = func() time.Time { return clock }
	})
	// The STORE's clock too, not only the server's.
	//
	// MemStore.now defaults to time.Now, so f.advance() moved the handler's clock
	// and left every incident's StartedAt on the wall clock, milliseconds apart.
	// Ties then fell through to Go's randomised map iteration inside
	// ListIncidents, and the ordering-dependent tests here passed about 12 runs
	// in 20 — green often enough to look solid, powerless often enough to miss a
	// real regression.
	h.store.now = func() time.Time { return clock }
	admin := h.setupAdmin()

	var page StatusPage
	rec := h.requestWith(http.MethodPost, "/api/v1/status-pages", map[string]any{
		"slug": "public", "title": "Public", "published": true,
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status page = %d, body %s", rec.Code, rec.Body.String())
	}
	mustJSON(t, rec, &page)

	var inc Incident
	rec = h.requestWith(http.MethodPost, "/api/v1/status-pages/"+id64(page.ID)+"/incidents", map[string]any{
		"title": "Database unreachable", "body": "Looking into it.", "severity": "major",
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create incident = %d, body %s", rec.Code, rec.Body.String())
	}
	mustJSON(t, rec, &inc)

	return &pmFixture{h: h, admin: admin, page: page, inc: inc, clock: &clock}
}

// advance moves the server clock forward.
func (f *pmFixture) advance(d time.Duration) { *f.clock = f.clock.Add(d) }

// id64 renders an id for a URL path.
func id64(v int64) string { return strconv.FormatInt(v, 10) }

func (f *pmFixture) path() string {
	return "/api/v1/status-pages/" + id64(f.page.ID) + "/incidents/" + id64(f.inc.ID) + "/postmortem"
}

func (f *pmFixture) resolve(t *testing.T) {
	t.Helper()
	rec := f.h.requestWith(http.MethodPost,
		"/api/v1/status-pages/"+id64(f.page.ID)+"/incidents/"+id64(f.inc.ID)+"/updates",
		map[string]any{"status": "resolved", "body": "Failover completed."}, f.admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("resolve = %d, body %s", rec.Code, rec.Body.String())
	}
}

// A draft is the ordinary intermediate state: written, reviewed, published
// later. It must never be one JSON field away from the open internet.
func TestADraftPostmortemIsNotPublished(t *testing.T) {
	f := newPostmortemFixture(t)
	f.resolve(t)

	rec := f.h.requestWith(http.MethodPut, f.path(), map[string]any{
		"body": "Root cause: the primary lost its disk. INTERNAL: runbook at wiki/db-failover.",
	}, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("save draft = %d, body %s", rec.Code, rec.Body.String())
	}
	var saved Incident
	mustJSON(t, rec, &saved)
	if saved.Postmortem == "" {
		t.Fatal("the draft was not stored")
	}
	if saved.PostmortemPublishedAt != nil {
		t.Fatal("a draft must not carry a publication timestamp")
	}

	// The authenticated read sees it — that is the editor reloading its own work.
	rec = f.h.requestWith(http.MethodGet, "/api/v1/status-pages/"+id64(f.page.ID)+"/incidents/"+id64(f.inc.ID), nil, f.admin)
	if !strings.Contains(rec.Body.String(), "runbook") {
		t.Error("the editor cannot read back its own draft")
	}

	// The public page must not.
	rec = f.h.request(http.MethodGet, "/api/v1/public/status/public", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("public page = %d, body %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "runbook") {
		t.Fatalf("an unpublished postmortem leaked to anonymous visitors:\n%s", rec.Body.String())
	}
}

func TestPublishingAPostmortemShowsItPublicly(t *testing.T) {
	f := newPostmortemFixture(t)
	f.resolve(t)

	rec := f.h.requestWith(http.MethodPut, f.path(), map[string]any{
		"body": "We lost the primary database node.", "published": true,
	}, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("publish = %d, body %s", rec.Code, rec.Body.String())
	}
	var published Incident
	mustJSON(t, rec, &published)
	if published.PostmortemPublishedAt == nil {
		t.Fatal("publishing left no timestamp, so the page cannot say when it was posted")
	}

	rec = f.h.request(http.MethodGet, "/api/v1/public/status/public", nil)
	if !strings.Contains(rec.Body.String(), "We lost the primary database node.") {
		t.Fatalf("a published postmortem is missing from the public page:\n%s", rec.Body.String())
	}
}

// A retrospective on an ongoing incident is not a retrospective. Drafting one
// early is fine — that is often when the notes get written — but it cannot go
// public while the outage is still open.
func TestPublishingRequiresAResolvedIncident(t *testing.T) {
	f := newPostmortemFixture(t)

	rec := f.h.requestWith(http.MethodPut, f.path(), map[string]any{
		"body": "Still guessing.", "published": true,
	}, f.admin)
	if rec.Code != http.StatusConflict {
		t.Fatalf("publish on an open incident = %d, want 409", rec.Code)
	}
	// The draft path is still open on the same incident.
	rec = f.h.requestWith(http.MethodPut, f.path(), map[string]any{"body": "Still guessing."}, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("draft on an open incident = %d, want 200", rec.Code)
	}
}

func TestPublishingAnEmptyPostmortemIsRejected(t *testing.T) {
	f := newPostmortemFixture(t)
	f.resolve(t)
	rec := f.h.requestWith(http.MethodPut, f.path(), map[string]any{"body": "   ", "published": true}, f.admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("publishing whitespace = %d, want 400 — it would render an empty section", rec.Code)
	}
}

// Re-publishing after a typo fix must not move the write-up's date to today:
// readers use it to tell a fresh retrospective from one posted months later.
func TestRepublishingKeepsTheOriginalTimestamp(t *testing.T) {
	f := newPostmortemFixture(t)
	f.resolve(t)

	var first Incident
	rec := f.h.requestWith(http.MethodPut, f.path(), map[string]any{"body": "v1", "published": true}, f.admin)
	mustJSON(t, rec, &first)

	// Two days later, on the server's own clock. A handler that re-stamped the
	// publication would now answer a timestamp 48 hours off, not one a few
	// milliseconds off — the difference between a test that catches the bug and
	// one that catches it only because time.Now() has nanosecond resolution.
	f.advance(48 * time.Hour)

	var second Incident
	rec = f.h.requestWith(http.MethodPut, f.path(), map[string]any{"body": "v1, with the typo fixed", "published": true}, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-publish = %d, body %s", rec.Code, rec.Body.String())
	}
	mustJSON(t, rec, &second)
	if !second.PostmortemPublishedAt.Equal(*first.PostmortemPublishedAt) {
		t.Fatalf("publication moved from %s to %s on an edit",
			first.PostmortemPublishedAt, second.PostmortemPublishedAt)
	}
}

func TestUnpublishingKeepsTheText(t *testing.T) {
	f := newPostmortemFixture(t)
	f.resolve(t)
	if rec := f.h.requestWith(http.MethodPut, f.path(), map[string]any{"body": "v1", "published": true}, f.admin); rec.Code != http.StatusOK {
		t.Fatalf("publish = %d", rec.Code)
	}

	var back Incident
	rec := f.h.requestWith(http.MethodPut, f.path(), map[string]any{"body": "v1", "published": false}, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("unpublish = %d, body %s", rec.Code, rec.Body.String())
	}
	mustJSON(t, rec, &back)
	if back.Postmortem != "v1" {
		t.Errorf("unpublishing destroyed the text: %q", back.Postmortem)
	}
	if back.PostmortemPublishedAt != nil {
		t.Error("unpublishing left the publication timestamp behind")
	}
	rec = f.h.request(http.MethodGet, "/api/v1/public/status/public", nil)
	if strings.Contains(rec.Body.String(), "v1") {
		t.Error("an unpublished postmortem is still on the public page")
	}
}

// The whole reason this is its own endpoint: an editor that loaded the incident
// before the retrospective existed must not erase it by saving a title change.
func TestUpdatingAnIncidentDoesNotClobberItsPostmortem(t *testing.T) {
	f := newPostmortemFixture(t)
	f.resolve(t)
	if rec := f.h.requestWith(http.MethodPut, f.path(), map[string]any{"body": "the write-up", "published": true}, f.admin); rec.Code != http.StatusOK {
		t.Fatalf("publish = %d", rec.Code)
	}

	// A full-replace PUT of the incident, exactly as a stale editor would send it:
	// no postmortem field anywhere in the body.
	rec := f.h.requestWith(http.MethodPut, "/api/v1/status-pages/"+id64(f.page.ID)+"/incidents/"+id64(f.inc.ID), map[string]any{
		"title": "Database unreachable (renamed)", "body": "Looking into it.",
		"severity": "major", "status": "resolved",
	}, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("update incident = %d, body %s", rec.Code, rec.Body.String())
	}
	var updated Incident
	mustJSON(t, rec, &updated)
	if updated.Postmortem != "the write-up" {
		t.Fatalf("a title change erased the postmortem: %q", updated.Postmortem)
	}
	if updated.PostmortemPublishedAt == nil {
		t.Fatal("a title change unpublished the postmortem")
	}
}

func TestPostmortemRejectsAnOversizedBody(t *testing.T) {
	f := newPostmortemFixture(t)
	f.resolve(t)
	rec := f.h.requestWith(http.MethodPut, f.path(), map[string]any{
		"body": strings.Repeat("x", maxPostmortemBytes+1),
	}, f.admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized body = %d, want 400 — it rides on every public page render", rec.Code)
	}
}

// The postmortem endpoint carries both ids and must honour both, like every
// other incident route: asking about page 2 must not act on page 1's incident.
func TestPostmortemHonoursTheStatusPageInThePath(t *testing.T) {
	f := newPostmortemFixture(t)
	rec := f.h.requestWith(http.MethodPost, "/api/v1/status-pages", map[string]any{
		"slug": "other", "title": "Other",
	}, f.admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("second page = %d", rec.Code)
	}
	var other StatusPage
	mustJSON(t, rec, &other)

	rec = f.h.requestWith(http.MethodPut,
		"/api/v1/status-pages/"+id64(other.ID)+"/incidents/"+id64(f.inc.ID)+"/postmortem",
		map[string]any{"body": "wrong page"}, f.admin)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-page write = %d, want 404", rec.Code)
	}
}

func TestPostmortemRequiresAWriter(t *testing.T) {
	f := newPostmortemFixture(t)
	viewer := f.h.createUser(f.admin, "viewer@example.com", "viewer")
	rec := f.h.requestWith(http.MethodPut, f.path(), map[string]any{"body": "nope"}, viewer)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer write = %d, want 403", rec.Code)
	}
}

// An omitted body means "leave the text alone", not "erase it".
//
// {"published": false} is the natural way to write an unpublish: it names the
// one thing being changed. With Body as a plain string the zero value made that
// request indistinguishable from {"body": "", "published": false}, so taking a
// retrospective down deleted it, and answered 200 OK while doing so.
func TestOmittingTheBodyDoesNotEraseThePostmortem(t *testing.T) {
	f := newPostmortemFixture(t)
	f.resolve(t)
	const text = "## Root cause\n\nA failover that had never been exercised."
	if rec := f.h.requestWith(http.MethodPut, f.path(), map[string]any{"body": text, "published": true}, f.admin); rec.Code != http.StatusOK {
		t.Fatalf("publish = %d", rec.Code)
	}

	rec := f.h.requestWith(http.MethodPut, f.path(), map[string]any{"published": false}, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("unpublish = %d, body %s", rec.Code, rec.Body.String())
	}
	var back Incident
	mustJSON(t, rec, &back)
	if back.Postmortem != text {
		t.Errorf("the retrospective is now %q, want it untouched", back.Postmortem)
	}
	if back.PostmortemPublishedAt != nil {
		t.Error("it is still published")
	}

	// And the reverse: publish again without resending the text.
	rec = f.h.requestWith(http.MethodPut, f.path(), map[string]any{"published": true}, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("republish = %d, body %s", rec.Code, rec.Body.String())
	}
	mustJSON(t, rec, &back)
	if back.Postmortem != text || back.PostmortemPublishedAt == nil {
		t.Errorf("republish without a body gave %q published=%v", back.Postmortem, back.PostmortemPublishedAt)
	}
}

// Clearing it is still possible; it just has to be said explicitly.
func TestAnExplicitEmptyBodyStillClearsThePostmortem(t *testing.T) {
	f := newPostmortemFixture(t)
	if rec := f.h.requestWith(http.MethodPut, f.path(), map[string]any{"body": "draft"}, f.admin); rec.Code != http.StatusOK {
		t.Fatalf("draft = %d", rec.Code)
	}
	rec := f.h.requestWith(http.MethodPut, f.path(), map[string]any{"body": "", "published": false}, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear = %d, body %s", rec.Code, rec.Body.String())
	}
	var back Incident
	mustJSON(t, rec, &back)
	if back.Postmortem != "" {
		t.Errorf("an explicit empty body left %q behind", back.Postmortem)
	}
}

// Publishing with an omitted body still refuses an empty retrospective: the
// check now runs against what is STORED, which is what would go on the page.
func TestPublishingWithNoStoredTextIsStillRejected(t *testing.T) {
	f := newPostmortemFixture(t)
	f.resolve(t)
	rec := f.h.requestWith(http.MethodPut, f.path(), map[string]any{"published": true}, f.admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("publishing nothing = %d, want 400: body %s", rec.Code, rec.Body.String())
	}
}

// A published retrospective stays reachable once it falls out of the ten most
// recent incidents.
//
// It is a document somebody deliberately put on the page, and usually the thing
// they link to afterwards. Dropping it because ten unrelated incidents have
// happened since made that link dead with no redirect and no note — the write-up
// still stored, still marked published, simply not rendered anywhere.
func TestAPublishedPostmortemSurvivesTenNewerIncidents(t *testing.T) {
	f := newPostmortemFixture(t)
	f.resolve(t)
	const text = "## Root cause\n\nThe one everybody links to."
	if rec := f.h.requestWith(http.MethodPut, f.path(), map[string]any{"body": text, "published": true}, f.admin); rec.Code != http.StatusOK {
		t.Fatalf("publish = %d", rec.Code)
	}
	oldest := f.inc.ID

	// Twelve newer resolved incidents, none with a retrospective.
	for i := range 12 {
		f.advance(time.Hour)
		rec := f.h.requestWith(http.MethodPost,
			"/api/v1/status-pages/"+id64(f.page.ID)+"/incidents", map[string]any{
				"title": "noise " + strconv.Itoa(i), "body": "x", "severity": "minor",
			}, f.admin)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create incident %d = %d, body %s", i, rec.Code, rec.Body.String())
		}
		var inc Incident
		mustJSON(t, rec, &inc)
		rec = f.h.requestWith(http.MethodPost,
			"/api/v1/status-pages/"+id64(f.page.ID)+"/incidents/"+id64(inc.ID)+"/updates",
			map[string]any{"status": "resolved", "body": "done"}, f.admin)
		if rec.Code != http.StatusCreated {
			t.Fatalf("resolve incident %d = %d", i, rec.Code)
		}
	}

	rec := f.h.request(http.MethodGet, "/api/v1/public/status/public", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("public page = %d", rec.Code)
	}
	var view PublicPageView
	mustJSON(t, rec, &view)

	var found *Incident
	for i := range view.RecentIncidents {
		if view.RecentIncidents[i].ID == oldest {
			found = &view.RecentIncidents[i]
		}
	}
	if found == nil {
		t.Fatalf("the incident with the published postmortem is gone from a page carrying %d incidents",
			len(view.RecentIncidents))
	}
	if found.Postmortem != text {
		t.Errorf("its retrospective came back as %q", found.Postmortem)
	}
	// And the newer, retrospective-less ones are still capped.
	if len(view.RecentIncidents) > 25 {
		t.Errorf("the page carries %d resolved incidents; the ceiling is 25", len(view.RecentIncidents))
	}
}

// A DRAFT retrospective must never go down the public WebSocket topic.
//
// status-page:{slug} is subscribed to by anonymous visitors of the page.
// announceIncident applies publicCopy on the way out precisely so no caller can
// forget — five call sites pass whatever incident they happen to be holding —
// and this is the assertion that says the guard is still there. It could not be
// made before: the test double discarded the payload, so a leak and correct
// behaviour recorded identically.
func TestADraftPostmortemIsNotBroadcastToThePublicTopic(t *testing.T) {
	pub := &fakeWSPub{}
	clock := time.Date(2026, 3, 14, 9, 10, 0, 0, time.UTC)
	h := newHarnessTweaked(t, func(o *Options) {
		o.WSPublisher = pub
		o.Now = func() time.Time { return clock }
	})
	admin := h.setupAdmin()

	var page StatusPage
	rec := h.requestWith(http.MethodPost, "/api/v1/status-pages", map[string]any{
		"slug": "public", "title": "Public", "published": true,
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status page = %d, body %s", rec.Code, rec.Body.String())
	}
	mustJSON(t, rec, &page)

	var inc Incident
	rec = h.requestWith(http.MethodPost, "/api/v1/status-pages/"+id64(page.ID)+"/incidents",
		map[string]any{"title": "Outage", "body": "Looking into it.", "severity": "major"}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create incident = %d", rec.Code)
	}
	mustJSON(t, rec, &inc)

	const secret = "we leaked a customer list to the logs for nine minutes"
	pmPath := "/api/v1/status-pages/" + id64(page.ID) + "/incidents/" + id64(inc.ID) + "/postmortem"
	if rec := h.requestWith(http.MethodPut, pmPath, map[string]any{"body": secret}, admin); rec.Code != http.StatusOK {
		t.Fatalf("save draft = %d, body %s", rec.Code, rec.Body.String())
	}

	assertNoLeak := func(when string) {
		t.Helper()
		for _, payload := range pub.sent(t) {
			if strings.Contains(payload, secret) {
				t.Fatalf("a draft retrospective went out on the public topic (%s):\n%s", when, payload)
			}
		}
	}
	assertNoLeak("after saving the draft")

	// Now an UNRELATED change to the same incident, which broadcasts it whole.
	// This is where the guard actually earns its place: announceIncident has five
	// callers, and every one of them passes the incident it happens to be
	// holding — including this one, which has just re-read the row with the draft
	// on it.
	rec = h.requestWith(http.MethodPost,
		"/api/v1/status-pages/"+id64(page.ID)+"/incidents/"+id64(inc.ID)+"/updates",
		map[string]any{"status": "monitoring", "body": "Watching it."}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("post update = %d, body %s", rec.Code, rec.Body.String())
	}
	assertNoLeak("after an unrelated update to the same incident")

	// Publishing it is the point at which it may be broadcast — and it must be,
	// or a page open in a browser keeps showing an incident with no write-up.
	rec = h.requestWith(http.MethodPost,
		"/api/v1/status-pages/"+id64(page.ID)+"/incidents/"+id64(inc.ID)+"/updates",
		map[string]any{"status": "resolved", "body": "Fixed."}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("resolve = %d", rec.Code)
	}
	if rec := h.requestWith(http.MethodPut, pmPath, map[string]any{"published": true}, admin); rec.Code != http.StatusOK {
		t.Fatalf("publish = %d, body %s", rec.Code, rec.Body.String())
	}
	leaked := false
	for _, payload := range pub.sent(t) {
		if strings.Contains(payload, secret) {
			leaked = true
		}
	}
	if !leaked {
		t.Error("a PUBLISHED retrospective was never broadcast; an open page keeps showing the incident without it")
	}
}

// A draft save is not a public event, so it must not wake every anonymous
// visitor of the page.
func TestSavingADraftDoesNotPokeThePublicTopic(t *testing.T) {
	pub := &fakeWSPub{}
	h := newHarnessTweaked(t, func(o *Options) { o.WSPublisher = pub })
	admin := h.setupAdmin()

	var page StatusPage
	rec := h.requestWith(http.MethodPost, "/api/v1/status-pages", map[string]any{
		"slug": "public", "title": "Public", "published": true,
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status page = %d", rec.Code)
	}
	mustJSON(t, rec, &page)
	var inc Incident
	rec = h.requestWith(http.MethodPost, "/api/v1/status-pages/"+id64(page.ID)+"/incidents",
		map[string]any{"title": "Outage", "body": "x", "severity": "minor"}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create incident = %d", rec.Code)
	}
	mustJSON(t, rec, &inc)

	before := pub.count()
	pmPath := "/api/v1/status-pages/" + id64(page.ID) + "/incidents/" + id64(inc.ID) + "/postmortem"
	for _, body := range []string{"d", "dr", "dra", "draf", "draft"} {
		if rec := h.requestWith(http.MethodPut, pmPath, map[string]any{"body": body}, admin); rec.Code != http.StatusOK {
			t.Fatalf("save %q = %d", body, rec.Code)
		}
	}
	if got := pub.count() - before; got != 0 {
		t.Errorf("five draft saves produced %d public broadcasts; publicCopy strips the draft, "+
			"so every one of them told anonymous visitors that nothing had changed", got)
	}
}

// A body-only edit must not take a published retrospective off the page.
//
// Body was made a pointer so an omitted field means "leave it alone"; Published
// was left a plain bool, so the endpoint had mixed semantics and the natural
// partial edit — PUT {"body": "typo fixed"} — decoded published:false and
// withdrew the document, with 200 OK and an ordinary-looking audit line.
func TestABodyOnlyEditDoesNotUnpublish(t *testing.T) {
	f := newPostmortemFixture(t)
	f.resolve(t)
	if rec := f.h.requestWith(http.MethodPut, f.path(), map[string]any{"body": "v1", "published": true}, f.admin); rec.Code != http.StatusOK {
		t.Fatalf("publish = %d", rec.Code)
	}

	rec := f.h.requestWith(http.MethodPut, f.path(), map[string]any{"body": "v1, typo fixed"}, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("body-only edit = %d, body %s", rec.Code, rec.Body.String())
	}
	var back Incident
	mustJSON(t, rec, &back)
	if back.Postmortem != "v1, typo fixed" {
		t.Errorf("the edit did not land: %q", back.Postmortem)
	}
	if back.PostmortemPublishedAt == nil {
		t.Error("a body-only edit withdrew the retrospective from the public page")
	}
}

// Editing an ALREADY PUBLISHED retrospective must keep working after the
// incident reopens.
//
// The "must be resolved" gate was unconditional, so it also blocked the edit —
// and in that state the UI disables Publish, leaving "Unpublish" as the only
// save. Correcting a typo meant taking the document down.
func TestEditingAPublishedPostmortemSurvivesTheIncidentReopening(t *testing.T) {
	f := newPostmortemFixture(t)
	f.resolve(t)
	if rec := f.h.requestWith(http.MethodPut, f.path(), map[string]any{"body": "v1", "published": true}, f.admin); rec.Code != http.StatusOK {
		t.Fatalf("publish = %d", rec.Code)
	}

	// It recurs.
	rec := f.h.requestWith(http.MethodPost,
		"/api/v1/status-pages/"+id64(f.page.ID)+"/incidents/"+id64(f.inc.ID)+"/updates",
		map[string]any{"status": "investigating", "body": "it is back"}, f.admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("reopen = %d, body %s", rec.Code, rec.Body.String())
	}

	// Reopening must also clear the resolution time, or the post-mortem template
	// prints "Resolved" and a Duration for an outage that is still running.
	var reopened Incident
	rec = f.h.requestWith(http.MethodGet,
		"/api/v1/status-pages/"+id64(f.page.ID)+"/incidents", nil, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	var listed struct {
		Items []Incident `json:"items"`
	}
	mustJSON(t, rec, &listed)
	for _, in := range listed.Items {
		if in.ID == f.inc.ID {
			reopened = in
		}
	}
	if reopened.ResolvedAt != nil {
		t.Errorf("a reopened incident still carries resolved_at = %v", reopened.ResolvedAt)
	}

	// And the edit still goes through.
	rec = f.h.requestWith(http.MethodPut, f.path(),
		map[string]any{"body": "v1 plus: this recurred at 11:00", "published": true}, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("editing a published retrospective on a reopened incident = %d, body %s",
			rec.Code, rec.Body.String())
	}
	var back Incident
	mustJSON(t, rec, &back)
	if back.PostmortemPublishedAt == nil {
		t.Error("the edit unpublished it")
	}
}

// Publishing something that was never resolved is still refused.
func TestPublishingAnUnresolvedIncidentIsStillRefused(t *testing.T) {
	f := newPostmortemFixture(t)
	if rec := f.h.requestWith(http.MethodPut, f.path(), map[string]any{"body": "early notes"}, f.admin); rec.Code != http.StatusOK {
		t.Fatalf("draft = %d", rec.Code)
	}
	rec := f.h.requestWith(http.MethodPut, f.path(), map[string]any{"published": true}, f.admin)
	if rec.Code != http.StatusConflict {
		t.Fatalf("publishing an unresolved incident = %d, want 409: %s", rec.Code, rec.Body.String())
	}
}
