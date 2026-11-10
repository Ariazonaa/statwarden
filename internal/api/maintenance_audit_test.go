package api

import (
	"net/http"
	"testing"
	"time"
)

// TestMaintenanceUpdateIsAudited pins that editing a window leaves an audit
// entry, as creating and deleting one already did. Moving a window or widening
// its scope silences alerts for the monitors it covers, which is exactly the
// kind of change the log exists to attribute.
func TestMaintenanceUpdateIsAudited(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	start := time.Now().Add(time.Hour).UTC()
	end := start.Add(time.Hour)
	rec := h.requestWith(http.MethodPost, "/api/v1/maintenance",
		maintenanceRequest{Title: "db upgrade", StartsAt: &start, EndsAt: &end}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, body %s", rec.Code, rec.Body.String())
	}
	var win MaintenanceWindow
	mustJSON(t, rec, &win)

	rec = h.requestWith(http.MethodPut, "/api/v1/maintenance/"+itoa64(win.ID),
		maintenanceRequest{Title: "db upgrade (moved)", StartsAt: &start, EndsAt: &end}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d, body %s", rec.Code, rec.Body.String())
	}

	entries, _, err := h.store.ListAudit(t.Context(), Page{Limit: 100})
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	for _, e := range entries {
		if e.Action == "maintenance.update" && e.EntityID != nil && *e.EntityID == win.ID {
			if e.UserID == nil || *e.UserID != admin.userID {
				t.Errorf("maintenance.update attributed to %v, want user %d", e.UserID, admin.userID)
			}
			return
		}
	}
	t.Fatalf("no maintenance.update entry for window %d among %d audit rows", win.ID, len(entries))
}
