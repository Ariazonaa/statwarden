package api

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"
)

// TestPublicMaintenanceView pins what an anonymous visitor learns about a
// maintenance window: its scope stated outright, only the monitors of THIS
// page, and for a recurring series the concrete occurrence rather than a
// series that is "scheduled" forever with no time attached.
func TestPublicMaintenanceView(t *testing.T) {
	published := true

	t.Run("monitor ids are limited to the page", func(t *testing.T) {
		h := newStatusHarness(t)
		admin := h.setupAdmin()
		mine := h.createMonitorFake(admin)
		theirs := h.createMonitorFake(admin)
		h.createPage(admin, statusPageRequest{
			Slug: "mine", Title: "Mine", Published: &published, MonitorIDs: []int64{mine},
		})
		h.createWindow(admin, maintenanceRequest{
			Title: "shared rack", Strategy: "recurring",
			Recurrence: alwaysOnRecurrence(), MonitorIDs: []int64{mine, theirs},
		})

		view := h.publicView("mine")
		if len(view.Maintenance) != 1 {
			t.Fatalf("maintenance = %+v, want one window", view.Maintenance)
		}
		w := view.Maintenance[0]
		if len(w.MonitorIDs) != 1 || w.MonitorIDs[0] != mine {
			t.Errorf("monitor_ids = %v, want only this page's monitor %d (the other is not public here)", w.MonitorIDs, mine)
		}
		if w.AppliesToAll {
			t.Error("a scoped window is reported as fleet-wide")
		}
	})

	t.Run("a fleet-wide window says so", func(t *testing.T) {
		h := newStatusHarness(t)
		admin := h.setupAdmin()
		mon := h.createMonitorFake(admin)
		h.createPage(admin, statusPageRequest{
			Slug: "all", Title: "All", Published: &published, MonitorIDs: []int64{mon},
		})
		h.createWindow(admin, maintenanceRequest{
			Title: "datacentre move", Strategy: "recurring", Recurrence: alwaysOnRecurrence(),
		})

		view := h.publicView("all")
		if len(view.Maintenance) != 1 {
			t.Fatalf("maintenance = %+v, want one window", view.Maintenance)
		}
		if w := view.Maintenance[0]; !w.AppliesToAll || len(w.MonitorIDs) != 0 {
			t.Errorf("applies_to_all=%v monitor_ids=%v, want true and []", w.AppliesToAll, w.MonitorIDs)
		}
	})

	t.Run("a recurring series is announced by its next occurrence", func(t *testing.T) {
		h := newStatusHarness(t)
		admin := h.setupAdmin()
		mon := h.createMonitorFake(admin)
		h.createPage(admin, statusPageRequest{
			Slug: "nightly", Title: "Nightly", Published: &published, MonitorIDs: []int64{mon},
		})
		// Daily, starting two hours from now (to the minute), for an hour.
		next := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Minute)
		h.createWindow(admin, maintenanceRequest{
			Title: "backup", Strategy: "recurring", MonitorIDs: []int64{mon},
			Recurrence: json.RawMessage(
				`{"weekdays":[0,1,2,3,4,5,6],"start":"` + next.Format("15:04") + `","duration_minutes":60}`),
		})

		view := h.publicView("nightly")
		if len(view.Maintenance) != 1 {
			t.Fatalf("maintenance = %+v, want the series announced", view.Maintenance)
		}
		w := view.Maintenance[0]
		if w.StartsAt == nil || w.EndsAt == nil {
			t.Fatalf("starts_at=%v ends_at=%v, want the occurrence's bounds", w.StartsAt, w.EndsAt)
		}
		if !w.StartsAt.Equal(next) || !w.EndsAt.Equal(next.Add(time.Hour)) {
			t.Errorf("occurrence = %v–%v, want %v–%v", w.StartsAt, w.EndsAt, next, next.Add(time.Hour))
		}
	})

	t.Run("a series with no occurrence left before its end is dropped", func(t *testing.T) {
		h := newStatusHarness(t)
		admin := h.setupAdmin()
		mon := h.createMonitorFake(admin)
		h.createPage(admin, statusPageRequest{
			Slug: "ending", Title: "Ending", Published: &published, MonitorIDs: []int64{mon},
		})
		// Only on the weekday three days from now, but the series ends tomorrow.
		day := time.Now().UTC().AddDate(0, 0, 3).Weekday()
		end := time.Now().UTC().Add(24 * time.Hour)
		h.createWindow(admin, maintenanceRequest{
			Title: "last ever", Strategy: "recurring", MonitorIDs: []int64{mon}, EndsAt: &end,
			Recurrence: json.RawMessage(
				`{"weekdays":[` + strconv.Itoa(int(day)) + `],"start":"02:00","duration_minutes":60}`),
		})

		if view := h.publicView("ending"); len(view.Maintenance) != 0 {
			t.Fatalf("maintenance = %+v, want none: the series has nothing left to announce", view.Maintenance)
		}
	})
}
