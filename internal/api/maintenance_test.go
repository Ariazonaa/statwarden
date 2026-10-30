package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// alwaysOnRecurrence covers every instant: all seven weekdays, from midnight,
// for a full day. Used where a test needs a window that is in effect *now*
// without pinning the test to a wall clock.
func alwaysOnRecurrence() json.RawMessage {
	return json.RawMessage(`{"weekdays":[0,1,2,3,4,5,6],"start":"00:00","duration_minutes":1440}`)
}

// TestMaintenanceValidation pins the write-time contract: a payload the API
// accepts is one the evaluator can actually evaluate. Everything else is a 400
// naming the field, rather than a row that is stored and then silently ignored
// forever.
func TestMaintenanceValidation(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	start := time.Now().Add(time.Hour).UTC()
	end := start.Add(time.Hour)

	cases := []struct {
		name string
		body maintenanceRequest
		want int
	}{
		{
			name: "single window",
			body: maintenanceRequest{Title: "db upgrade", StartsAt: &start, EndsAt: &end},
			want: http.StatusCreated,
		},
		{
			name: "recurring window",
			body: maintenanceRequest{
				Title: "nightly", Strategy: "recurring", Timezone: "Europe/Berlin",
				Recurrence: json.RawMessage(`{"weekdays":[1,3],"start":"02:00","duration_minutes":120}`),
			},
			want: http.StatusCreated,
		},
		{
			name: "missing title",
			body: maintenanceRequest{StartsAt: &start, EndsAt: &end},
			want: http.StatusBadRequest,
		},
		{
			name: "single without starts_at",
			body: maintenanceRequest{Title: "no start", EndsAt: &end},
			want: http.StatusBadRequest,
		},
		{
			name: "single without ends_at",
			body: maintenanceRequest{Title: "no end", StartsAt: &start},
			want: http.StatusBadRequest,
		},
		{
			name: "single ends before it starts",
			body: maintenanceRequest{Title: "backwards", StartsAt: &end, EndsAt: &start},
			want: http.StatusBadRequest,
		},
		{
			name: "unknown strategy",
			body: maintenanceRequest{Title: "weird", Strategy: "sometimes", StartsAt: &start, EndsAt: &end},
			want: http.StatusBadRequest,
		},
		{
			name: "unknown timezone",
			body: maintenanceRequest{
				Title: "bad tz", Timezone: "Mars/Olympus_Mons", StartsAt: &start, EndsAt: &end,
			},
			want: http.StatusBadRequest,
		},
		{
			name: "recurring without recurrence",
			body: maintenanceRequest{Title: "empty", Strategy: "recurring"},
			want: http.StatusBadRequest,
		},
		{
			name: "recurrence with no weekdays",
			body: maintenanceRequest{
				Title: "no days", Strategy: "recurring",
				Recurrence: json.RawMessage(`{"weekdays":[],"start":"02:00","duration_minutes":60}`),
			},
			want: http.StatusBadRequest,
		},
		{
			name: "recurrence with out-of-range weekday",
			body: maintenanceRequest{
				Title: "day 9", Strategy: "recurring",
				Recurrence: json.RawMessage(`{"weekdays":[9],"start":"02:00","duration_minutes":60}`),
			},
			want: http.StatusBadRequest,
		},
		{
			name: "recurrence with malformed start",
			body: maintenanceRequest{
				Title: "2am", Strategy: "recurring",
				Recurrence: json.RawMessage(`{"weekdays":[1],"start":"2:00","duration_minutes":60}`),
			},
			want: http.StatusBadRequest,
		},
		{
			name: "recurrence with zero duration",
			body: maintenanceRequest{
				Title: "instant", Strategy: "recurring",
				Recurrence: json.RawMessage(`{"weekdays":[1],"start":"02:00","duration_minutes":0}`),
			},
			want: http.StatusBadRequest,
		},
		{
			// A typo'd field would otherwise be dropped, storing a window that
			// never fires and never explains why.
			name: "recurrence with unknown field",
			body: maintenanceRequest{
				Title: "typo", Strategy: "recurring",
				Recurrence: json.RawMessage(`{"weekdays":[1],"start":"02:00","duration":60}`),
			},
			want: http.StatusBadRequest,
		},
		{
			name: "recurrence is not an object",
			body: maintenanceRequest{
				Title: "cron", Strategy: "recurring",
				Recurrence: json.RawMessage(`"0 2 * * 1"`),
			},
			want: http.StatusBadRequest,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.requestWith(http.MethodPost, "/api/v1/maintenance", tc.body, admin)
			if rec.Code != tc.want {
				t.Fatalf("create = %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == http.StatusBadRequest && rec.Body.Len() == 0 {
				t.Fatal("400 carried no problem detail")
			}
		})
	}
}

// TestMaintenanceUpdateValidated proves the update path is guarded too: a
// window that was valid on create cannot be edited into one the evaluator would
// have to skip.
func TestMaintenanceUpdateValidated(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	rec := h.requestWith(http.MethodPost, "/api/v1/maintenance", maintenanceRequest{
		Title: "nightly", Strategy: "recurring", Recurrence: alwaysOnRecurrence(),
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, body %s", rec.Code, rec.Body.String())
	}
	var mw MaintenanceWindow
	mustJSON(t, rec, &mw)

	rec = h.requestWith(http.MethodPut, "/api/v1/maintenance/"+itoa64(mw.ID), maintenanceRequest{
		Title: "nightly", Strategy: "recurring",
		Recurrence: json.RawMessage(`{"weekdays":[1],"start":"25:00","duration_minutes":60}`),
	}, admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("update with bad start = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestPublicPageMaintenanceMatchesEvaluator is the "one evaluator" guarantee:
// the public page must call a window in effect exactly when the engine would.
//
// The discriminating case is "recurring, not right now": the old ends_at test
// could not tell it apart from one that is running, so the page banner said
// maintenance while the engine went on alerting.
func TestPublicPageMaintenanceMatchesEvaluator(t *testing.T) {
	published := true

	t.Run("in effect now colours the banner", func(t *testing.T) {
		h := newStatusHarness(t)
		admin := h.setupAdmin()
		mon := h.createMonitorFake(admin)
		h.createPage(admin, statusPageRequest{
			Slug: "now", Title: "Now", Published: &published, MonitorIDs: []int64{mon},
		})
		h.createWindow(admin, maintenanceRequest{
			Title: "rolling restart", Strategy: "recurring",
			Recurrence: alwaysOnRecurrence(), MonitorIDs: []int64{mon},
		})

		view := h.publicView("now")
		if len(view.Maintenance) != 1 {
			t.Fatalf("maintenance = %+v, want the running window", view.Maintenance)
		}
		if view.OverallStatus != OverallMaintenance {
			t.Fatalf("overall = %q, want %q", view.OverallStatus, OverallMaintenance)
		}
	})

	t.Run("recurring but not right now is listed without colouring the banner", func(t *testing.T) {
		h := newStatusHarness(t)
		admin := h.setupAdmin()
		mon := h.createMonitorFake(admin)
		h.createPage(admin, statusPageRequest{
			Slug: "later", Title: "Later", Published: &published, MonitorIDs: []int64{mon},
		})
		// Two hours from now for one hour: announced, but not in effect. The
		// occurrence on the previous weekday ended long ago, so no lookback
		// case can make this cover the present instant either.
		start := time.Now().UTC().Add(2 * time.Hour).Format("15:04")
		h.createWindow(admin, maintenanceRequest{
			Title: "later tonight", Strategy: "recurring", MonitorIDs: []int64{mon},
			Recurrence: json.RawMessage(
				`{"weekdays":[0,1,2,3,4,5,6],"start":"` + start + `","duration_minutes":60}`),
		})

		view := h.publicView("later")
		if len(view.Maintenance) != 1 {
			t.Fatalf("maintenance = %+v, want the upcoming window announced", view.Maintenance)
		}
		if view.OverallStatus != OverallOperational {
			t.Fatalf("overall = %q, want %q: an upcoming window is not an outage",
				view.OverallStatus, OverallOperational)
		}
	})

	t.Run("window scoped to other monitors is not shown", func(t *testing.T) {
		h := newStatusHarness(t)
		admin := h.setupAdmin()
		mine := h.createMonitorFake(admin)
		theirs := h.createMonitorFake(admin)
		h.createPage(admin, statusPageRequest{
			Slug: "scoped", Title: "Scoped", Published: &published, MonitorIDs: []int64{mine},
		})
		h.createWindow(admin, maintenanceRequest{
			Title: "someone else's", Strategy: "recurring",
			Recurrence: alwaysOnRecurrence(), MonitorIDs: []int64{theirs},
		})

		view := h.publicView("scoped")
		if len(view.Maintenance) != 0 {
			t.Fatalf("maintenance = %+v, want none: the window is scoped elsewhere", view.Maintenance)
		}
		if view.OverallStatus != OverallOperational {
			t.Fatalf("overall = %q, want %q", view.OverallStatus, OverallOperational)
		}
	})

	t.Run("finished window is dropped", func(t *testing.T) {
		h := newStatusHarness(t)
		admin := h.setupAdmin()
		mon := h.createMonitorFake(admin)
		h.createPage(admin, statusPageRequest{
			Slug: "past", Title: "Past", Published: &published, MonitorIDs: []int64{mon},
		})
		start := time.Now().UTC().Add(-2 * time.Hour)
		end := start.Add(time.Hour)
		h.createWindow(admin, maintenanceRequest{
			Title: "yesterday's", StartsAt: &start, EndsAt: &end, MonitorIDs: []int64{mon},
		})

		view := h.publicView("past")
		if len(view.Maintenance) != 0 {
			t.Fatalf("maintenance = %+v, want none: the window has ended", view.Maintenance)
		}
	})
}

// TestMaintenanceMutationRefreshesEvaluator proves the invalidation seam: the
// snapshot the engine and the status page read is rebuilt by the handler, so a
// window takes effect and stops taking effect on the write, not a refresh
// interval later.
func TestMaintenanceMutationRefreshesEvaluator(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	now := time.Now()

	if h.maint.Snapshot().InMaintenance(1, now) {
		t.Fatal("monitor is in maintenance before any window exists")
	}

	mw := h.createWindow(admin, maintenanceRequest{
		Title: "rolling restart", Strategy: "recurring",
		Recurrence: alwaysOnRecurrence(), MonitorIDs: []int64{1},
	})
	if !h.maint.Snapshot().InMaintenance(1, now) {
		t.Fatal("window created but the evaluator does not see it")
	}
	if h.maint.Snapshot().InMaintenance(2, now) {
		t.Fatal("window scoped to monitor 1 suppresses monitor 2")
	}

	// Deactivating is a plain update; it must take effect immediately.
	inactive := false
	rec := h.requestWith(http.MethodPut, "/api/v1/maintenance/"+itoa64(mw.ID), maintenanceRequest{
		Title: "rolling restart", Strategy: "recurring", Active: &inactive,
		Recurrence: alwaysOnRecurrence(), MonitorIDs: []int64{1},
	}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d, body %s", rec.Code, rec.Body.String())
	}
	if h.maint.Snapshot().InMaintenance(1, now) {
		t.Fatal("window deactivated but the evaluator still suppresses")
	}

	// And so must deletion, from a re-activated window.
	mw = h.createWindow(admin, maintenanceRequest{
		Title: "second", Strategy: "recurring",
		Recurrence: alwaysOnRecurrence(), MonitorIDs: []int64{1},
	})
	if !h.maint.Snapshot().InMaintenance(1, now) {
		t.Fatal("second window created but the evaluator does not see it")
	}
	rec = h.requestWith(http.MethodDelete, "/api/v1/maintenance/"+itoa64(mw.ID), nil, admin)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, body %s", rec.Code, rec.Body.String())
	}
	if h.maint.Snapshot().InMaintenance(1, now) {
		t.Fatal("window deleted but the evaluator still suppresses")
	}
}

// createWindow creates a maintenance window via the API and returns it.
func (h *harness) createWindow(sess *session, body maintenanceRequest) MaintenanceWindow {
	h.t.Helper()
	rec := h.requestWith(http.MethodPost, "/api/v1/maintenance", body, sess)
	if rec.Code != http.StatusCreated {
		h.t.Fatalf("create maintenance = %d, body %s", rec.Code, rec.Body.String())
	}
	var mw MaintenanceWindow
	mustJSON(h.t, rec, &mw)
	return mw
}

// publicView fetches a public status page and decodes it.
func (h *harness) publicView(slug string) PublicPageView {
	h.t.Helper()
	rec := h.getPublic("/api/v1/public/status/"+slug, "")
	if rec.Code != http.StatusOK {
		h.t.Fatalf("public GET = %d, body %s", rec.Code, rec.Body.String())
	}
	var view PublicPageView
	mustJSON(h.t, rec, &view)
	return view
}
