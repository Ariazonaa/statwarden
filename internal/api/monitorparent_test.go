package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// setParent issues the full-replace update that repoints a monitor's parent and
// returns the recorder. The update DTO is full-replace, so the other fields are
// resent unchanged — this mirrors what the form does.
func (h *harness) setParent(sess *session, m monitorView, parentID *int64) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.requestWith(http.MethodPut, fmt.Sprintf("/api/v1/monitors/%d", m.ID), monitorRequest{
		Name:     m.Name,
		Type:     m.Type,
		Interval: m.Interval,
		ParentID: parentID,
	}, sess)
}

// TestMonitorRejectsSelfParent covers the degenerate cycle. The engine walks
// parent_id to decide whether to suppress a monitor's alerts while its parent
// is down; a monitor that is its own parent is a monitor that can suppress its
// own alerts.
func TestMonitorRejectsSelfParent(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	m := h.createMonitor(admin, monitorRequest{Name: "web", Type: "fake", Interval: 30})

	rec := h.setParent(admin, m, &m.ID)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("self-parent = %d, want 400", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "parent_id") {
		t.Fatalf("problem detail %q does not name parent_id", body)
	}

	// The rejection must not have been applied.
	rec = h.requestWith(http.MethodGet, fmt.Sprintf("/api/v1/monitors/%d", m.ID), nil, admin)
	var after monitorView
	mustJSON(t, rec, &after)
	if after.ParentID != nil {
		t.Fatalf("parent_id after rejected write = %v, want nil", *after.ParentID)
	}
}

// TestMonitorRejectsParentCycle builds A -> B -> C and then tries to close the
// loop by pointing A at C. Each individual edge is legitimate; only the last
// one forms the cycle, which is exactly the case a per-request "is this my own
// id" check misses and a chain walk catches.
func TestMonitorRejectsParentCycle(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	a := h.createMonitor(admin, monitorRequest{Name: "a", Type: "fake", Interval: 30})
	b := h.createMonitor(admin, monitorRequest{Name: "b", Type: "fake", Interval: 30})
	c := h.createMonitor(admin, monitorRequest{Name: "c", Type: "fake", Interval: 30})

	// B depends on A, C depends on B. Both are legal.
	if rec := h.setParent(admin, b, &a.ID); rec.Code != http.StatusOK {
		t.Fatalf("b.parent = a → %d, body %s", rec.Code, rec.Body.String())
	}
	if rec := h.setParent(admin, c, &b.ID); rec.Code != http.StatusOK {
		t.Fatalf("c.parent = b → %d, body %s", rec.Code, rec.Body.String())
	}

	// A depending on C would close A -> B -> C -> A.
	rec := h.setParent(admin, a, &c.ID)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("three-monitor cycle = %d, want 400", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "parent_id") {
		t.Fatalf("problem detail %q does not name parent_id", body)
	}

	// The chain must be intact and acyclic afterwards.
	get := func(id int64) monitorView {
		t.Helper()
		rec := h.requestWith(http.MethodGet, fmt.Sprintf("/api/v1/monitors/%d", id), nil, admin)
		var m monitorView
		mustJSON(t, rec, &m)
		return m
	}
	if p := get(a.ID).ParentID; p != nil {
		t.Fatalf("a.parent after rejected write = %d, want nil", *p)
	}
	if p := get(b.ID).ParentID; p == nil || *p != a.ID {
		t.Fatalf("b.parent = %v, want %d", p, a.ID)
	}
	if p := get(c.ID).ParentID; p == nil || *p != b.ID {
		t.Fatalf("c.parent = %v, want %d", p, b.ID)
	}
}

// TestMonitorRejectsUnknownParent keeps a dangling parent a 400 naming the
// field rather than a foreign-key 500.
func TestMonitorRejectsUnknownParent(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	missing := int64(9999)
	rec := h.requestWith(http.MethodPost, "/api/v1/monitors",
		monitorRequest{Name: "web", Type: "fake", Interval: 30, ParentID: &missing}, admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown parent_id = %d, want 400", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "parent_id") {
		t.Fatalf("problem detail %q does not name parent_id", body)
	}
}

// TestMonitorAcceptsValidParentChain guards the other direction: the cycle
// check must not reject legitimate nesting, or it would be trivially "passing"
// by refusing every parent.
func TestMonitorAcceptsValidParentChain(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	a := h.createMonitor(admin, monitorRequest{Name: "a", Type: "fake", Interval: 30})
	b := h.createMonitor(admin, monitorRequest{Name: "b", Type: "fake", Interval: 30})
	if rec := h.setParent(admin, b, &a.ID); rec.Code != http.StatusOK {
		t.Fatalf("legitimate parent rejected: %d, body %s", rec.Code, rec.Body.String())
	}

	// And a parent set at creation time works too.
	c := h.createMonitor(admin, monitorRequest{Name: "c", Type: "fake", Interval: 30, ParentID: &b.ID})
	if c.ParentID == nil || *c.ParentID != b.ID {
		t.Fatalf("created c.parent = %v, want %d", c.ParentID, b.ID)
	}
}
