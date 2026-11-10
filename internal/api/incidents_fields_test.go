package api

import (
	"net/http"
	"testing"
)

// TestIncidentCreatedResolvedCarriesResolvedAt pins that an incident recorded
// after the fact — created directly as resolved — gets a resolution time. Only
// the edit and timeline paths stamped resolved_at, so such an incident sat in
// the public "recent incidents" list with no end time and the post-mortem
// template computed no duration for it.
func TestIncidentCreatedResolvedCarriesResolvedAt(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	page := h.createPage(admin, statusPageRequest{Slug: "inc", Title: "Inc"})

	rec := h.requestWith(http.MethodPost, "/api/v1/status-pages/"+itoa64(page.ID)+"/incidents",
		incidentRequest{Title: "yesterday's outage", Status: IncidentResolved}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, body %s", rec.Code, rec.Body.String())
	}
	var in Incident
	mustJSON(t, rec, &in)
	if in.ResolvedAt == nil {
		t.Fatal("incident created as resolved has no resolved_at")
	}

	// An open incident still has none.
	rec = h.requestWith(http.MethodPost, "/api/v1/status-pages/"+itoa64(page.ID)+"/incidents",
		incidentRequest{Title: "ongoing"}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create open = %d, body %s", rec.Code, rec.Body.String())
	}
	var open Incident
	mustJSON(t, rec, &open)
	if open.ResolvedAt != nil {
		t.Errorf("open incident carries resolved_at %v", open.ResolvedAt)
	}
}

// TestIncidentUpdateWithoutTitleKeepsIt pins that a PUT which leaves the title
// out does not blank it. Severity and status already fell back to the stored
// value when omitted; the title was copied verbatim, so a status-only PUT left a
// nameless incident on the public page — a title the create path refuses.
func TestIncidentUpdateWithoutTitleKeepsIt(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	page := h.createPage(admin, statusPageRequest{Slug: "inc", Title: "Inc"})
	rec := h.requestWith(http.MethodPost, "/api/v1/status-pages/"+itoa64(page.ID)+"/incidents",
		incidentRequest{Title: "database latency"}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, body %s", rec.Code, rec.Body.String())
	}
	var in Incident
	mustJSON(t, rec, &in)

	rec = h.requestWith(http.MethodPut,
		"/api/v1/status-pages/"+itoa64(page.ID)+"/incidents/"+itoa64(in.ID),
		incidentRequest{Status: IncidentMonitoring}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d, body %s", rec.Code, rec.Body.String())
	}
	var got Incident
	mustJSON(t, rec, &got)
	if got.Title != "database latency" {
		t.Errorf("title = %q after a PUT without one, want it kept", got.Title)
	}
	if got.Status != IncidentMonitoring {
		t.Errorf("status = %q, the rest of the PUT was not applied", got.Status)
	}
}
