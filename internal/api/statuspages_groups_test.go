package api

import (
	"net/http"
	"testing"
)

// TestStatusPageRejectsDuplicateGroupNames pins the rule that makes the layout
// round-trip well-defined. A monitor is attached to a group BY NAME on the wire
// (statusPageMonitorInput.GroupName) and both layout writers resolve it through
// a name->id map, so two groups sharing a name silently collapsed every monitor
// naming it into whichever was written last and left the other rendering empty.
// The payload cannot even express which of the two was meant, so it is rejected
// rather than guessed at.
func TestStatusPageRejectsDuplicateGroupNames(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	published := true

	dup := statusPageRequest{
		Slug: "dup-groups", Title: "Dup", Published: &published,
		Groups: []statusPageGroupInput{{Name: "Core", SortOrder: 0}, {Name: "Core", SortOrder: 1}},
	}
	rec := h.requestWith(http.MethodPost, "/api/v1/status-pages", dup, admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create with duplicate group names = %d, want 400; body %s", rec.Code, rec.Body.String())
	}

	// An empty name is refused for the same reason: nothing can reference it, so
	// a group carrying one could never hold a monitor.
	empty := statusPageRequest{
		Slug: "empty-group", Title: "Empty", Published: &published,
		Groups: []statusPageGroupInput{{Name: "", SortOrder: 0}},
	}
	if rec := h.requestWith(http.MethodPost, "/api/v1/status-pages", empty, admin); rec.Code != http.StatusBadRequest {
		t.Fatalf("create with an empty group name = %d, want 400; body %s", rec.Code, rec.Body.String())
	}

	// The same rule has to hold on update, or a page could be edited into the
	// broken shape the create path refuses.
	ok := statusPageRequest{
		Slug: "ok-groups", Title: "OK", Published: &published,
		Groups: []statusPageGroupInput{{Name: "Core", SortOrder: 0}},
	}
	rec = h.requestWith(http.MethodPost, "/api/v1/status-pages", ok, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, body %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID int64 `json:"id"`
	}
	mustJSON(t, rec, &created)

	dup.Slug, dup.Title = "ok-groups", "OK"
	rec = h.requestWith(http.MethodPut, "/api/v1/status-pages/"+itoa64(created.ID), dup, admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("update with duplicate group names = %d, want 400; body %s", rec.Code, rec.Body.String())
	}
}
