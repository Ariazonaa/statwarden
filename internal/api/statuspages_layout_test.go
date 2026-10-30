package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// layoutFixture is a saved status page with a known layout: group "A" holds m1
// then m2, group "B" holds m3, and nothing is ungrouped.
type layoutFixture struct {
	h          *harness
	admin      *session
	page       StatusPage
	m1, m2, m3 int64
}

func newLayoutFixture(t *testing.T) *layoutFixture {
	t.Helper()
	h := newHarness(t)
	admin := h.setupAdmin()
	m1 := h.createMonitorNamed(admin, "one", "fake")
	m2 := h.createMonitorNamed(admin, "two", "fake")
	m3 := h.createMonitorNamed(admin, "three", "fake")
	pub := true
	show := true
	p := h.createPage(admin, statusPageRequest{
		Slug: "layout", Title: "Layout", Published: &pub,
		Groups: []statusPageGroupInput{{Name: "A", SortOrder: 0}, {Name: "B", SortOrder: 1}},
		Monitors: []statusPageMonitorInput{
			{MonitorID: m1, GroupName: "A", ShowUptime: &show},
			{MonitorID: m2, GroupName: "A", ShowUptime: &show},
			{MonitorID: m3, GroupName: "B", ShowUptime: &show},
		},
	})
	return &layoutFixture{h: h, admin: admin, page: p, m1: m1, m2: m2, m3: m3}
}

// reorder PATCHes a layout ordering for the fixture's page as the given session.
func (f *layoutFixture) reorder(sess *session, body layoutReorderRequest) *httptest.ResponseRecorder {
	f.h.t.Helper()
	return f.h.requestWith(http.MethodPatch, "/api/v1/status-pages/"+itoa64(f.page.ID)+"/layout", body, sess)
}

// get re-reads a page through the admin API.
func (f *layoutFixture) get(id int64) StatusPage {
	f.h.t.Helper()
	rec := f.h.requestWith(http.MethodGet, "/api/v1/status-pages/"+itoa64(id), nil, f.admin)
	if rec.Code != http.StatusOK {
		f.h.t.Fatalf("get page %d = %d, body %s", id, rec.Code, rec.Body.String())
	}
	var p StatusPage
	mustJSON(f.h.t, rec, &p)
	return p
}

// groupID returns the server-assigned id of the named group.
func groupID(t *testing.T, p StatusPage, name string) int64 {
	t.Helper()
	for _, g := range p.Groups {
		if g.Name == name {
			return g.ID
		}
	}
	t.Fatalf("group %q not on page %d: %+v", name, p.ID, p.Groups)
	return 0
}

// entryOrder flattens a page's monitor entries into the ids they appear in.
func entryOrder(p StatusPage) []int64 {
	ids := make([]int64, 0, len(p.Monitors))
	for _, e := range p.Monitors {
		ids = append(ids, e.MonitorID)
	}
	return ids
}

// assertProblem checks a status code and that the body is RFC 7807.
func assertProblem(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d, body %s", rec.Code, want, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("content-type = %q, want application/problem+json", ct)
	}
}

func TestReorderStatusPageLayout(t *testing.T) {
	f := newLayoutFixture(t)
	gA := groupID(t, f.page, "A")
	gB := groupID(t, f.page, "B")

	// Swap the groups and reverse the monitors inside A.
	rec := f.reorder(f.admin, layoutReorderRequest{
		Groups: []layoutGroupOrderInput{
			{GroupID: gB, MonitorIDs: []int64{f.m3}},
			{GroupID: gA, MonitorIDs: []int64{f.m2, f.m1}},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("reorder = %d, body %s", rec.Code, rec.Body.String())
	}
	var got StatusPage
	mustJSON(t, rec, &got)

	if len(got.Groups) != 2 || got.Groups[0].Name != "B" || got.Groups[1].Name != "A" {
		t.Fatalf("group order = %+v, want B then A", got.Groups)
	}
	want := []int64{f.m3, f.m2, f.m1}
	if !reflect.DeepEqual(entryOrder(got), want) {
		t.Fatalf("monitor order = %v, want %v", entryOrder(got), want)
	}
	// A single running position across the whole layout, contiguous from 0.
	for i, e := range got.Monitors {
		if e.SortOrder != i {
			t.Fatalf("monitors[%d].sort_order = %d, want %d", i, e.SortOrder, i)
		}
	}
	// The flat projection must agree with the rich one.
	if !reflect.DeepEqual(got.MonitorIDs, want) {
		t.Fatalf("monitor_ids = %v, want %v", got.MonitorIDs, want)
	}
}

func TestReorderStatusPageLayoutBumpsUpdatedAt(t *testing.T) {
	f := newLayoutFixture(t)
	// Pin the clock forward: the wall clock's resolution is too coarse to prove
	// "strictly later" for two calls this close together.
	later := f.page.UpdatedAt.Add(time.Minute)
	f.h.store.now = func() time.Time { return later }

	rec := f.reorder(f.admin, layoutReorderRequest{
		Groups: []layoutGroupOrderInput{
			{GroupID: groupID(t, f.page, "B"), MonitorIDs: []int64{f.m3}},
			{GroupID: groupID(t, f.page, "A"), MonitorIDs: []int64{f.m1, f.m2}},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("reorder = %d, body %s", rec.Code, rec.Body.String())
	}
	var got StatusPage
	mustJSON(t, rec, &got)
	if !got.UpdatedAt.After(f.page.UpdatedAt) {
		t.Fatalf("updated_at = %s, want after %s", got.UpdatedAt, f.page.UpdatedAt)
	}
}

func TestReorderStatusPageLayoutRejectsIncompleteSet(t *testing.T) {
	f := newLayoutFixture(t)
	before := entryOrder(f.get(f.page.ID))

	// m3 is omitted entirely.
	rec := f.reorder(f.admin, layoutReorderRequest{
		Groups: []layoutGroupOrderInput{
			{GroupID: groupID(t, f.page, "A"), MonitorIDs: []int64{f.m2, f.m1}},
			{GroupID: groupID(t, f.page, "B")},
		},
	})
	assertProblem(t, rec, http.StatusConflict)

	after := f.get(f.page.ID)
	if !reflect.DeepEqual(entryOrder(after), before) {
		t.Fatalf("layout changed after a rejected reorder: %v, want %v", entryOrder(after), before)
	}
	for i, e := range after.Monitors {
		if e.SortOrder != i {
			t.Fatalf("monitors[%d].sort_order = %d, want the original %d", i, e.SortOrder, i)
		}
	}
}

func TestReorderStatusPageLayoutRejectsForeignIDs(t *testing.T) {
	f := newLayoutFixture(t)
	// A second page with its own group and its own monitor.
	other := f.h.createMonitorNamed(f.admin, "other", "fake")
	pub := true
	page2 := f.h.createPage(f.admin, statusPageRequest{
		Slug: "other", Title: "Other", Published: &pub,
		Groups:   []statusPageGroupInput{{Name: "C", SortOrder: 0}},
		Monitors: []statusPageMonitorInput{{MonitorID: other, GroupName: "C"}},
	})
	page2Group := groupID(t, page2, "C")
	page2Before := f.get(page2.ID)

	gA := groupID(t, f.page, "A")
	gB := groupID(t, f.page, "B")

	// Page 2's group id in page 1's payload, with the count still matching.
	rec := f.reorder(f.admin, layoutReorderRequest{
		Groups: []layoutGroupOrderInput{
			{GroupID: gA, MonitorIDs: []int64{f.m1, f.m2}},
			{GroupID: page2Group, MonitorIDs: []int64{f.m3}},
		},
	})
	assertProblem(t, rec, http.StatusConflict)

	// Page 2's monitor id in place of one of page 1's, count still matching.
	rec = f.reorder(f.admin, layoutReorderRequest{
		Groups: []layoutGroupOrderInput{
			{GroupID: gA, MonitorIDs: []int64{f.m1, f.m2}},
			{GroupID: gB, MonitorIDs: []int64{other}},
		},
	})
	assertProblem(t, rec, http.StatusConflict)

	// The store is the boundary, not the handler pre-check: the same payloads
	// must be refused when they reach it directly.
	ctx := context.Background()
	_, err := f.h.store.ReorderStatusPageLayout(ctx, f.page.ID, StatusPageLayoutOrder{
		Groups: []StatusPageGroupOrder{
			{GroupID: gA, MonitorIDs: []int64{f.m1, f.m2}},
			{GroupID: page2Group, MonitorIDs: []int64{f.m3}},
		},
	})
	if err != ErrConflict {
		t.Fatalf("store foreign group id = %v, want ErrConflict", err)
	}
	_, err = f.h.store.ReorderStatusPageLayout(ctx, f.page.ID, StatusPageLayoutOrder{
		Groups: []StatusPageGroupOrder{
			{GroupID: gA, MonitorIDs: []int64{f.m1, f.m2}},
			{GroupID: gB, MonitorIDs: []int64{other}},
		},
	})
	if err != ErrConflict {
		t.Fatalf("store foreign monitor id = %v, want ErrConflict", err)
	}

	page2After := f.get(page2.ID)
	if !reflect.DeepEqual(page2After.Groups, page2Before.Groups) ||
		!reflect.DeepEqual(page2After.Monitors, page2Before.Monitors) {
		t.Fatalf("page 2 layout was written across pages: %+v", page2After)
	}
}

func TestReorderStatusPageLayoutRejectsDuplicates(t *testing.T) {
	f := newLayoutFixture(t)
	gA := groupID(t, f.page, "A")
	gB := groupID(t, f.page, "B")

	// (a) The same monitor listed twice, once ungrouped and once in a group.
	dupMonitors := layoutReorderRequest{
		Ungrouped: []int64{f.m1},
		Groups: []layoutGroupOrderInput{
			{GroupID: gA, MonitorIDs: []int64{f.m1, f.m2}},
			{GroupID: gB, MonitorIDs: []int64{f.m3}},
		},
	}
	assertProblem(t, f.reorder(f.admin, dupMonitors), http.StatusBadRequest)

	// (b) The same group listed twice, with the group count still equal to the
	// page's real one — the payload count-equality alone would have accepted.
	dupGroups := layoutReorderRequest{
		Groups: []layoutGroupOrderInput{
			{GroupID: gA, MonitorIDs: []int64{f.m1, f.m2}},
			{GroupID: gA, MonitorIDs: []int64{f.m3}},
		},
	}
	assertProblem(t, f.reorder(f.admin, dupGroups), http.StatusBadRequest)

	// Both must also be refused by the store itself, bypassing the handler
	// pre-check entirely: distinctness is proven inside the transaction, so a
	// duplicate can never commit a layout that leaves a row with a stale order.
	ctx := context.Background()
	_, err := f.h.store.ReorderStatusPageLayout(ctx, f.page.ID, dupGroups.toOrder())
	if err != ErrConflict {
		t.Fatalf("store duplicate group id = %v, want ErrConflict", err)
	}
	_, err = f.h.store.ReorderStatusPageLayout(ctx, f.page.ID, dupMonitors.toOrder())
	if err != ErrConflict {
		t.Fatalf("store duplicate monitor id = %v, want ErrConflict", err)
	}

	after := f.get(f.page.ID)
	want := []int64{f.m1, f.m2, f.m3}
	if !reflect.DeepEqual(entryOrder(after), want) {
		t.Fatalf("layout changed after rejected duplicates: %v, want %v", entryOrder(after), want)
	}
}

func TestReorderStatusPageLayoutStaleAfterSave(t *testing.T) {
	f := newLayoutFixture(t)
	oldA := groupID(t, f.page, "A")
	oldB := groupID(t, f.page, "B")

	// A full save recreates the group rows, so the captured ids are now stale.
	pub := true
	rec := f.h.requestWith(http.MethodPut, "/api/v1/status-pages/"+itoa64(f.page.ID), statusPageRequest{
		Slug: "layout", Title: "Layout", Published: &pub,
		Groups: []statusPageGroupInput{{Name: "A", SortOrder: 0}, {Name: "B", SortOrder: 1}},
		Monitors: []statusPageMonitorInput{
			{MonitorID: f.m1, GroupName: "A"},
			{MonitorID: f.m2, GroupName: "A"},
			{MonitorID: f.m3, GroupName: "B"},
		},
	}, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("save page = %d, body %s", rec.Code, rec.Body.String())
	}
	var saved StatusPage
	mustJSON(t, rec, &saved)
	if groupID(t, saved, "A") == oldA {
		t.Fatalf("group ids survived a save; the optimistic-concurrency property is gone")
	}

	assertProblem(t, f.reorder(f.admin, layoutReorderRequest{
		Groups: []layoutGroupOrderInput{
			{GroupID: oldB, MonitorIDs: []int64{f.m3}},
			{GroupID: oldA, MonitorIDs: []int64{f.m1, f.m2}},
		},
	}), http.StatusConflict)
}

func TestReorderStatusPageLayoutMovesMonitorAcrossGroups(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	m1 := h.createMonitorNamed(admin, "one", "fake")
	m2 := h.createMonitorNamed(admin, "two", "fake")
	pub := true
	no := false
	show := true
	page := h.createPage(admin, statusPageRequest{
		Slug: "cross", Title: "Cross", Published: &pub,
		Groups: []statusPageGroupInput{{Name: "A", SortOrder: 0}, {Name: "B", SortOrder: 1}},
		Monitors: []statusPageMonitorInput{
			{MonitorID: m1, GroupName: "A", ShowUptime: &no},
			{MonitorID: m2, GroupName: "B", ShowUptime: &show},
		},
	})
	gA := groupID(t, page, "A")
	gB := groupID(t, page, "B")

	rec := h.requestWith(http.MethodPatch, "/api/v1/status-pages/"+itoa64(page.ID)+"/layout",
		layoutReorderRequest{
			Groups: []layoutGroupOrderInput{
				{GroupID: gA},
				{GroupID: gB, MonitorIDs: []int64{m2, m1}},
			},
		}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("reorder = %d, body %s", rec.Code, rec.Body.String())
	}
	var got StatusPage
	mustJSON(t, rec, &got)

	var moved *StatusPageMonitorEntry
	for i := range got.Monitors {
		if got.Monitors[i].MonitorID == m1 {
			moved = &got.Monitors[i]
		}
	}
	if moved == nil {
		t.Fatalf("m1 missing from the layout: %+v", got.Monitors)
	}
	if moved.GroupID == nil || *moved.GroupID != gB || moved.GroupName != "B" {
		t.Fatalf("moved entry = %+v, want group B (%d)", moved, gB)
	}
	// A reorder rewrites placement, never per-monitor flags.
	if moved.ShowUptime {
		t.Fatalf("show_uptime was reset by the reorder")
	}
}

func TestReorderStatusPageLayoutIdempotent(t *testing.T) {
	f := newLayoutFixture(t)
	before := f.get(f.page.ID)

	rec := f.reorder(f.admin, layoutReorderRequest{
		Groups: []layoutGroupOrderInput{
			{GroupID: groupID(t, before, "A"), MonitorIDs: []int64{f.m1, f.m2}},
			{GroupID: groupID(t, before, "B"), MonitorIDs: []int64{f.m3}},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("reorder = %d, body %s", rec.Code, rec.Body.String())
	}
	var got StatusPage
	mustJSON(t, rec, &got)
	if !reflect.DeepEqual(got.Groups, before.Groups) {
		t.Fatalf("groups changed: %+v, want %+v", got.Groups, before.Groups)
	}
	if !reflect.DeepEqual(got.Monitors, before.Monitors) {
		t.Fatalf("monitors changed: %+v, want %+v", got.Monitors, before.Monitors)
	}
}

func TestReorderStatusPageLayoutEmptyPage(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	pub := true
	page := h.createPage(admin, statusPageRequest{Slug: "bare", Title: "Bare", Published: &pub})

	rec := h.requestWithRaw(http.MethodPatch, "/api/v1/status-pages/"+itoa64(page.ID)+"/layout",
		`{"ungrouped":[],"groups":[]}`, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("empty reorder = %d, body %s", rec.Code, rec.Body.String())
	}
}

func TestReorderStatusPageLayoutRequiresWriter(t *testing.T) {
	f := newLayoutFixture(t)
	body := layoutReorderRequest{
		Groups: []layoutGroupOrderInput{
			{GroupID: groupID(t, f.page, "B"), MonitorIDs: []int64{f.m3}},
			{GroupID: groupID(t, f.page, "A"), MonitorIDs: []int64{f.m1, f.m2}},
		},
	}

	viewer := f.h.createUser(f.admin, "viewer@example.com", "viewer")
	assertProblem(t, f.reorder(viewer, body), http.StatusForbidden)

	writer := f.h.createUser(f.admin, "writer@example.com", string(roleWriter))
	if rec := f.reorder(writer, body); rec.Code != http.StatusOK {
		t.Fatalf("writer reorder = %d, body %s", rec.Code, rec.Body.String())
	}
}

func TestReorderStatusPageLayoutRequiresCSRF(t *testing.T) {
	f := newLayoutFixture(t)
	noCSRF := &session{cookies: f.admin.cookies}
	assertProblem(t, f.reorder(noCSRF, layoutReorderRequest{
		Groups: []layoutGroupOrderInput{
			{GroupID: groupID(t, f.page, "A"), MonitorIDs: []int64{f.m1, f.m2}},
			{GroupID: groupID(t, f.page, "B"), MonitorIDs: []int64{f.m3}},
		},
	}), http.StatusForbidden)
}

func TestReorderStatusPageLayoutUnknownPage(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	rec := h.requestWith(http.MethodPatch, "/api/v1/status-pages/999999/layout",
		layoutReorderRequest{}, admin)
	assertProblem(t, rec, http.StatusNotFound)
}

func TestReorderStatusPageLayoutRejectsUnknownFields(t *testing.T) {
	f := newLayoutFixture(t)
	rec := f.h.requestWithRaw(http.MethodPatch, "/api/v1/status-pages/"+itoa64(f.page.ID)+"/layout",
		`{"ungrouped":[],"groups":[],"bogus":1}`, f.admin)
	assertProblem(t, rec, http.StatusBadRequest)
}

func TestReorderStatusPageLayoutAudited(t *testing.T) {
	f := newLayoutFixture(t)
	rec := f.reorder(f.admin, layoutReorderRequest{
		Groups: []layoutGroupOrderInput{
			{GroupID: groupID(t, f.page, "B"), MonitorIDs: []int64{f.m3}},
			{GroupID: groupID(t, f.page, "A"), MonitorIDs: []int64{f.m1, f.m2}},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("reorder = %d, body %s", rec.Code, rec.Body.String())
	}

	entries, _, err := f.h.store.ListAudit(context.Background(), Page{Limit: 100})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	for _, e := range entries {
		if e.Action == "status_page.reorder" && e.EntityType == "status_page" &&
			e.EntityID != nil && *e.EntityID == f.page.ID {
			return
		}
	}
	t.Fatalf("no status_page.reorder audit entry for page %d in %+v", f.page.ID, entries)
}
