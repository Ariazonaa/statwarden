package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// createMonitorGroup posts a group and returns it, failing the test on a
// non-201.
func (h *harness) createMonitorGroup(sess *session, name string, sortOrder int) MonitorGroup {
	h.t.Helper()
	rec := h.requestWith(http.MethodPost, "/api/v1/monitor-groups",
		monitorGroupRequest{Name: name, SortOrder: sortOrder}, sess)
	if rec.Code != http.StatusCreated {
		h.t.Fatalf("create group = %d, body %s", rec.Code, rec.Body.String())
	}
	var g MonitorGroup
	mustJSON(h.t, rec, &g)
	return g
}

// createMonitor posts a monitor and returns its view, failing on a non-201.
func (h *harness) createMonitor(sess *session, req monitorRequest) monitorView {
	h.t.Helper()
	rec := h.requestWith(http.MethodPost, "/api/v1/monitors", req, sess)
	if rec.Code != http.StatusCreated {
		h.t.Fatalf("create monitor %q = %d, body %s", req.Name, rec.Code, rec.Body.String())
	}
	var m monitorView
	mustJSON(h.t, rec, &m)
	return m
}

func TestMonitorGroupCRUD(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	g := h.createMonitorGroup(admin, "Production", 1)
	if g.ID == 0 || g.Name != "Production" || g.SortOrder != 1 {
		t.Fatalf("unexpected group: %+v", g)
	}

	// List returns it.
	rec := h.requestWith(http.MethodGet, "/api/v1/monitor-groups", nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	var groups []MonitorGroup
	mustJSON(t, rec, &groups)
	if len(groups) != 1 || groups[0].ID != g.ID {
		t.Fatalf("list = %+v, want the one created group", groups)
	}

	// Update renames and repositions.
	rec = h.requestWith(http.MethodPut, fmt.Sprintf("/api/v1/monitor-groups/%d", g.ID),
		monitorGroupRequest{Name: "Staging", SortOrder: 5}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d, body %s", rec.Code, rec.Body.String())
	}
	var updated MonitorGroup
	mustJSON(t, rec, &updated)
	if updated.Name != "Staging" || updated.SortOrder != 5 {
		t.Fatalf("update = %+v, want name Staging sort 5", updated)
	}

	// A duplicate name is a conflict, not a second group with the same name.
	rec = h.requestWith(http.MethodPost, "/api/v1/monitor-groups",
		monitorGroupRequest{Name: "Staging"}, admin)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate name = %d, want 409", rec.Code)
	}

	// A nameless group is rejected.
	rec = h.requestWith(http.MethodPost, "/api/v1/monitor-groups", monitorGroupRequest{}, admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty name = %d, want 400", rec.Code)
	}

	// Delete, then it is gone from the list and unknown to update.
	rec = h.requestWith(http.MethodDelete, fmt.Sprintf("/api/v1/monitor-groups/%d", g.ID), nil, admin)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	rec = h.requestWith(http.MethodGet, "/api/v1/monitor-groups", nil, admin)
	mustJSON(t, rec, &groups)
	if len(groups) != 0 {
		t.Fatalf("list after delete = %+v, want empty", groups)
	}
	rec = h.requestWith(http.MethodPut, fmt.Sprintf("/api/v1/monitor-groups/%d", g.ID),
		monitorGroupRequest{Name: "Zombie"}, admin)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("update deleted group = %d, want 404", rec.Code)
	}
}

// TestMonitorSurvivesGroupDeletion pins the ON DELETE SET NULL contract:
// deleting a folder unfiles its monitors and must never delete them. A
// CASCADE (or a MemStore that forgot to clear the pointer) would silently
// destroy monitoring coverage from what looks like a tidy-up action.
func TestMonitorSurvivesGroupDeletion(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	g := h.createMonitorGroup(admin, "Production", 0)
	m := h.createMonitor(admin, monitorRequest{Name: "web", Type: "fake", Interval: 30, GroupID: &g.ID})
	if m.GroupID == nil || *m.GroupID != g.ID {
		t.Fatalf("created monitor group_id = %v, want %d", m.GroupID, g.ID)
	}

	rec := h.requestWith(http.MethodDelete, fmt.Sprintf("/api/v1/monitor-groups/%d", g.ID), nil, admin)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete group = %d", rec.Code)
	}

	rec = h.requestWith(http.MethodGet, fmt.Sprintf("/api/v1/monitors/%d", m.ID), nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("monitor after group delete = %d, want it to survive", rec.Code)
	}
	var after monitorView
	mustJSON(t, rec, &after)
	if after.GroupID != nil {
		t.Fatalf("monitor group_id after group delete = %v, want nil", *after.GroupID)
	}
	if after.Name != "web" {
		t.Fatalf("monitor name = %q, want it otherwise untouched", after.Name)
	}
}

// TestMonitorRejectsUnknownGroup keeps an unknown folder a 400 naming the
// field rather than a foreign-key 500.
func TestMonitorRejectsUnknownGroup(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	missing := int64(9999)
	rec := h.requestWith(http.MethodPost, "/api/v1/monitors",
		monitorRequest{Name: "web", Type: "fake", Interval: 30, GroupID: &missing}, admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown group_id = %d, want 400", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "group_id") {
		t.Fatalf("problem detail %q does not name group_id", body)
	}
}

func TestMonitorListFiltersByGroup(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	g := h.createMonitorGroup(admin, "Production", 0)
	filed := h.createMonitor(admin, monitorRequest{Name: "filed", Type: "fake", Interval: 30, GroupID: &g.ID})
	unfiled := h.createMonitor(admin, monitorRequest{Name: "unfiled", Type: "fake", Interval: 30})

	ids := func(target string) []int64 {
		t.Helper()
		rec := h.requestWith(http.MethodGet, target, nil, admin)
		if rec.Code != http.StatusOK {
			t.Fatalf("list %s = %d", target, rec.Code)
		}
		var env struct {
			Items []monitorView `json:"items"`
		}
		mustJSON(t, rec, &env)
		out := make([]int64, 0, len(env.Items))
		for _, m := range env.Items {
			out = append(out, m.ID)
		}
		return out
	}

	if got := ids(fmt.Sprintf("/api/v1/monitors?group_id=%d", g.ID)); len(got) != 1 || got[0] != filed.ID {
		t.Fatalf("group filter = %v, want [%d]", got, filed.ID)
	}
	// "none" is the only way to ask for the unfiled monitors; a numeric id
	// cannot express it.
	if got := ids("/api/v1/monitors?group_id=none"); len(got) != 1 || got[0] != unfiled.ID {
		t.Fatalf("unfiled filter = %v, want [%d]", got, unfiled.ID)
	}
}
