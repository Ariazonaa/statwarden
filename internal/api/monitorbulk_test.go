package api

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

// bulk posts a bulk action and returns the decoded response, failing on a
// non-200.
func (h *harness) bulk(sess *session, req bulkMonitorRequest) bulkMonitorResponse {
	h.t.Helper()
	rec := h.requestWith(http.MethodPost, "/api/v1/monitors/bulk", req, sess)
	if rec.Code != http.StatusOK {
		h.t.Fatalf("bulk %s = %d, body %s", req.Action, rec.Code, rec.Body.String())
	}
	var resp bulkMonitorResponse
	mustJSON(h.t, rec, &resp)
	return resp
}

// TestBulkPauseReachesEngine is the load-bearing one. Flipping the rows without
// telling the scheduler produces monitors that read as paused in the UI and
// keep checking and keep alerting — a bulk pause that does not reach the engine
// is worse than no bulk pause at all.
func TestBulkPauseReachesEngine(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	a := h.createMonitor(admin, monitorRequest{Name: "a", Type: "fake", Interval: 30})
	b := h.createMonitor(admin, monitorRequest{Name: "b", Type: "fake", Interval: 30})

	resp := h.bulk(admin, bulkMonitorRequest{Action: bulkActionPause, MonitorIDs: []int64{a.ID, b.ID}})
	if resp.Succeeded != 2 || resp.Failed != 0 {
		t.Fatalf("bulk pause = %d ok / %d failed, want 2/0 (%+v)", resp.Succeeded, resp.Failed, resp.Results)
	}

	// The engine was driven once per monitor, exactly as handlePauseMonitor does.
	h.engine.mu.Lock()
	paused := append([]int64(nil), h.engine.paused...)
	h.engine.mu.Unlock()
	if len(paused) != 2 {
		t.Fatalf("engine.Pause calls = %v, want one per monitor", paused)
	}
	got := map[int64]bool{paused[0]: true, paused[1]: true}
	if !got[a.ID] || !got[b.ID] {
		t.Fatalf("engine paused %v, want %d and %d", paused, a.ID, b.ID)
	}

	// And the rows agree with the scheduler.
	for _, id := range []int64{a.ID, b.ID} {
		m, err := h.store.GetMonitor(t.Context(), id)
		if err != nil {
			t.Fatalf("get monitor %d: %v", id, err)
		}
		if m.Active {
			t.Fatalf("monitor %d still active after bulk pause", id)
		}
	}
}

func TestBulkResumeReachesEngine(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	a := h.createMonitor(admin, monitorRequest{Name: "a", Type: "fake", Interval: 30})
	h.bulk(admin, bulkMonitorRequest{Action: bulkActionPause, MonitorIDs: []int64{a.ID}})

	resp := h.bulk(admin, bulkMonitorRequest{Action: bulkActionResume, MonitorIDs: []int64{a.ID}})
	if resp.Succeeded != 1 {
		t.Fatalf("bulk resume = %+v, want 1 success", resp)
	}
	h.engine.mu.Lock()
	resumed := len(h.engine.resumed)
	h.engine.mu.Unlock()
	if resumed != 1 {
		t.Fatalf("engine.Resume calls = %d, want 1", resumed)
	}
}

func TestBulkDeleteReachesEngine(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	a := h.createMonitor(admin, monitorRequest{Name: "a", Type: "fake", Interval: 30})
	resp := h.bulk(admin, bulkMonitorRequest{Action: bulkActionDelete, MonitorIDs: []int64{a.ID}})
	if resp.Succeeded != 1 {
		t.Fatalf("bulk delete = %+v, want 1 success", resp)
	}
	h.engine.mu.Lock()
	removed := append([]int64(nil), h.engine.removed...)
	h.engine.mu.Unlock()
	if len(removed) != 1 || removed[0] != a.ID {
		t.Fatalf("engine.Remove = %v, want [%d]", removed, a.ID)
	}
	if _, err := h.store.GetMonitor(t.Context(), a.ID); err == nil {
		t.Fatal("monitor still present after bulk delete")
	}
}

// TestBulkWithUnknownIDReportsPerID is the partial-failure contract: one stale
// id (a monitor deleted between the list render and the click) must not
// discard the rest of the batch, and the caller must be told WHICH id failed
// rather than just "something went wrong".
func TestBulkWithUnknownIDReportsPerID(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	a := h.createMonitor(admin, monitorRequest{Name: "a", Type: "fake", Interval: 30})
	b := h.createMonitor(admin, monitorRequest{Name: "b", Type: "fake", Interval: 30})
	const missing = int64(9999)

	resp := h.bulk(admin, bulkMonitorRequest{
		Action:     bulkActionPause,
		MonitorIDs: []int64{a.ID, missing, b.ID},
	})
	if resp.Succeeded != 2 || resp.Failed != 1 {
		t.Fatalf("bulk = %d ok / %d failed, want 2/1 (%+v)", resp.Succeeded, resp.Failed, resp.Results)
	}
	if len(resp.Results) != 3 {
		t.Fatalf("results = %d entries, want one per requested id", len(resp.Results))
	}
	byID := map[int64]bulkMonitorResult{}
	for _, r := range resp.Results {
		byID[r.MonitorID] = r
	}
	if r := byID[missing]; r.OK || r.Error != "not found" {
		t.Fatalf("unknown id result = %+v, want ok=false error=\"not found\"", r)
	}
	for _, id := range []int64{a.ID, b.ID} {
		if r := byID[id]; !r.OK {
			t.Fatalf("monitor %d result = %+v, want ok", id, r)
		}
	}

	// The good monitors really were paused — the batch was not abandoned.
	h.engine.mu.Lock()
	paused := len(h.engine.paused)
	h.engine.mu.Unlock()
	if paused != 2 {
		t.Fatalf("engine.Pause calls = %d, want 2 (the unknown id must not stop the batch)", paused)
	}
	for _, id := range []int64{a.ID, b.ID} {
		m, err := h.store.GetMonitor(t.Context(), id)
		if err != nil {
			t.Fatalf("get monitor %d: %v", id, err)
		}
		if m.Active {
			t.Fatalf("monitor %d still active: the batch was abandoned on the bad id", id)
		}
	}
}

// TestBulkTagIsAdditive checks that bulk tagging adds to what a monitor already
// carries instead of replacing it — the caller asked to add a tag to a mixed
// selection, not to flatten every monitor to one shared set.
func TestBulkTagIsAdditive(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	rec := h.requestWith(http.MethodPost, "/api/v1/tags", tagRequest{Name: "prod"}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create tag = %d", rec.Code)
	}
	var prod Tag
	mustJSON(t, rec, &prod)
	rec = h.requestWith(http.MethodPost, "/api/v1/tags", tagRequest{Name: "eu"}, admin)
	var eu Tag
	mustJSON(t, rec, &eu)

	// a already carries "eu"; b carries nothing.
	a := h.createMonitor(admin, monitorRequest{Name: "a", Type: "fake", Interval: 30, TagIDs: []int64{eu.ID}})
	b := h.createMonitor(admin, monitorRequest{Name: "b", Type: "fake", Interval: 30})

	resp := h.bulk(admin, bulkMonitorRequest{
		Action:     bulkActionTag,
		MonitorIDs: []int64{a.ID, b.ID},
		TagIDs:     []int64{prod.ID},
	})
	if resp.Succeeded != 2 {
		t.Fatalf("bulk tag = %+v, want 2 successes", resp)
	}

	links, err := h.store.MonitorLinks(t.Context(), []int64{a.ID, b.ID})
	if err != nil {
		t.Fatalf("monitor links: %v", err)
	}
	if got := links[a.ID].TagIDs; len(got) != 2 {
		t.Fatalf("a tags = %v, want both eu and prod (additive, not replaced)", got)
	}
	if got := links[b.ID].TagIDs; len(got) != 1 || got[0] != prod.ID {
		t.Fatalf("b tags = %v, want [%d]", got, prod.ID)
	}

	// Re-applying the same tag is a no-op, not a duplicate or an error.
	h.bulk(admin, bulkMonitorRequest{Action: bulkActionTag, MonitorIDs: []int64{b.ID}, TagIDs: []int64{prod.ID}})
	links, _ = h.store.MonitorLinks(t.Context(), []int64{b.ID})
	if got := links[b.ID].TagIDs; len(got) != 1 {
		t.Fatalf("b tags after repeat = %v, want no duplicate", got)
	}
}

func TestBulkGroupAssignsAndClears(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	g := h.createMonitorGroup(admin, "Production", 0)
	a := h.createMonitor(admin, monitorRequest{Name: "a", Type: "fake", Interval: 30})

	if resp := h.bulk(admin, bulkMonitorRequest{
		Action: bulkActionGroup, MonitorIDs: []int64{a.ID}, GroupID: &g.ID,
	}); resp.Succeeded != 1 {
		t.Fatalf("bulk group = %+v, want 1 success", resp)
	}
	m, _ := h.store.GetMonitor(t.Context(), a.ID)
	if m.GroupID == nil || *m.GroupID != g.ID {
		t.Fatalf("group_id = %v, want %d", m.GroupID, g.ID)
	}

	// A null group_id clears the folder rather than being treated as "absent".
	if resp := h.bulk(admin, bulkMonitorRequest{
		Action: bulkActionGroup, MonitorIDs: []int64{a.ID}, GroupID: nil,
	}); resp.Succeeded != 1 {
		t.Fatalf("bulk ungroup = %+v, want 1 success", resp)
	}
	m, _ = h.store.GetMonitor(t.Context(), a.ID)
	if m.GroupID != nil {
		t.Fatalf("group_id after clear = %d, want nil", *m.GroupID)
	}
}

// TestBulkNotifyIsAdditive is the notify mirror of TestBulkTagIsAdditive:
// bulk-attaching a channel ADDS to what each monitor already routes to instead
// of replacing it, so a mixed selection is not flattened to one shared set.
func TestBulkNotifyIsAdditive(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	var slack, email Notification
	mustJSON(t, h.requestWith(http.MethodPost, "/api/v1/notifications",
		notificationRequest{Name: "slack", Type: "fake"}, admin), &slack)
	mustJSON(t, h.requestWith(http.MethodPost, "/api/v1/notifications",
		notificationRequest{Name: "email", Type: "fake"}, admin), &email)

	// a already routes to slack; b routes nowhere.
	a := h.createMonitor(admin, monitorRequest{Name: "a", Type: "fake", Interval: 30, NotificationIDs: []int64{slack.ID}})
	b := h.createMonitor(admin, monitorRequest{Name: "b", Type: "fake", Interval: 30})

	resp := h.bulk(admin, bulkMonitorRequest{
		Action:          bulkActionNotify,
		MonitorIDs:      []int64{a.ID, b.ID},
		NotificationIDs: []int64{email.ID},
	})
	if resp.Succeeded != 2 {
		t.Fatalf("bulk notify = %+v, want 2 successes", resp)
	}

	links, err := h.store.MonitorLinks(t.Context(), []int64{a.ID, b.ID})
	if err != nil {
		t.Fatalf("monitor links: %v", err)
	}
	if got := links[a.ID].NotificationIDs; len(got) != 2 {
		t.Fatalf("a notifications = %v, want both slack and email (additive, not replaced)", got)
	}
	if got := links[b.ID].NotificationIDs; len(got) != 1 || got[0] != email.ID {
		t.Fatalf("b notifications = %v, want [%d]", got, email.ID)
	}

	// Re-attaching the same channel is a no-op, not a duplicate or an error.
	h.bulk(admin, bulkMonitorRequest{Action: bulkActionNotify, MonitorIDs: []int64{b.ID}, NotificationIDs: []int64{email.ID}})
	links, _ = h.store.MonitorLinks(t.Context(), []int64{b.ID})
	if got := links[b.ID].NotificationIDs; len(got) != 1 {
		t.Fatalf("b notifications after repeat = %v, want no duplicate", got)
	}
}

// TestBulkScheduleSetsTimingAndReachesEngine pins the two things a bulk schedule
// must do: write the new timing (the interval plus any optional override) and
// re-Upsert active monitors so the scheduler ticks on the new cadence — a
// schedule that skips the engine leaves monitors checking on the old interval.
// A paused monitor is re-timed on the row but must not be handed to the engine.
func TestBulkScheduleSetsTimingAndReachesEngine(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	a := h.createMonitor(admin, monitorRequest{Name: "a", Type: "fake", Interval: 30})
	b := h.createMonitor(admin, monitorRequest{Name: "b", Type: "fake", Interval: 30})
	h.bulk(admin, bulkMonitorRequest{Action: bulkActionPause, MonitorIDs: []int64{b.ID}})

	// Both monitors were Upserted into the engine at creation; forget those so
	// the assertion below sees only what the schedule action drives.
	h.engine.mu.Lock()
	h.engine.upserts = nil
	h.engine.mu.Unlock()

	timeout := 15
	resp := h.bulk(admin, bulkMonitorRequest{
		Action:          bulkActionSchedule,
		MonitorIDs:      []int64{a.ID, b.ID},
		IntervalSeconds: 300,
		TimeoutSeconds:  &timeout,
	})
	if resp.Succeeded != 2 || resp.Failed != 0 {
		t.Fatalf("bulk schedule = %d ok / %d failed, want 2/0 (%+v)", resp.Succeeded, resp.Failed, resp.Results)
	}

	// Both rows carry the new interval and the timeout override, paused or not.
	for _, id := range []int64{a.ID, b.ID} {
		m, err := h.store.GetMonitor(t.Context(), id)
		if err != nil {
			t.Fatalf("get monitor %d: %v", id, err)
		}
		if m.Interval != 300*time.Second {
			t.Fatalf("monitor %d interval = %s, want 5m", id, m.Interval)
		}
		if m.Timeout != 15*time.Second {
			t.Fatalf("monitor %d timeout = %s, want 15s", id, m.Timeout)
		}
	}

	// The active monitor was rescheduled; the paused one was not.
	h.engine.mu.Lock()
	var upsertedA, upsertedB bool
	for _, m := range h.engine.upserts {
		switch m.ID {
		case a.ID:
			upsertedA = true
		case b.ID:
			upsertedB = true
		}
	}
	h.engine.mu.Unlock()
	if !upsertedA {
		t.Fatal("active monitor was not re-Upserted after bulk schedule; the scheduler keeps the old interval")
	}
	if upsertedB {
		t.Fatal("paused monitor was handed to the engine by bulk schedule")
	}
}

// TestBulkScheduleLeavesOptionalFieldsUntouched pins the COALESCE semantics: an
// omitted override keeps the monitor's stored value rather than nulling it, so a
// bulk schedule can change only the interval.
func TestBulkScheduleLeavesOptionalFieldsUntouched(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	timeout := 42
	a := h.createMonitor(admin, monitorRequest{
		Name: "a", Type: "fake", Interval: 30, Timeout: &timeout, MaxRetries: 5, RetryInterval: 25,
	})

	resp := h.bulk(admin, bulkMonitorRequest{
		Action: bulkActionSchedule, MonitorIDs: []int64{a.ID}, IntervalSeconds: 120,
	})
	if resp.Succeeded != 1 {
		t.Fatalf("bulk schedule = %+v, want 1 success", resp)
	}
	m, _ := h.store.GetMonitor(t.Context(), a.ID)
	if m.Interval != 120*time.Second {
		t.Fatalf("interval = %s, want 2m", m.Interval)
	}
	if m.Timeout != 42*time.Second {
		t.Fatalf("timeout = %s, want the untouched 42s", m.Timeout)
	}
	if m.MaxRetries != 5 {
		t.Fatalf("max_retries = %d, want the untouched 5", m.MaxRetries)
	}
	if m.RetryInterval != 25*time.Second {
		t.Fatalf("retry_interval = %s, want the untouched 25s", m.RetryInterval)
	}
}

// TestBulkRequestValidation covers the whole-request rejections: these are
// malformed requests, not per-id outcomes, so they must be 400s and must write
// nothing.
func TestBulkRequestValidation(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	a := h.createMonitor(admin, monitorRequest{Name: "a", Type: "fake", Interval: 30})
	missing := int64(9999)
	zero := 0

	cases := []struct {
		name string
		body bulkMonitorRequest
	}{
		{"empty ids", bulkMonitorRequest{Action: bulkActionPause}},
		{"unknown action", bulkMonitorRequest{Action: "explode", MonitorIDs: []int64{a.ID}}},
		{"tag without tag_ids", bulkMonitorRequest{Action: bulkActionTag, MonitorIDs: []int64{a.ID}}},
		{"tag with unknown tag", bulkMonitorRequest{Action: bulkActionTag, MonitorIDs: []int64{a.ID}, TagIDs: []int64{missing}}},
		{"group with unknown group", bulkMonitorRequest{Action: bulkActionGroup, MonitorIDs: []int64{a.ID}, GroupID: &missing}},
		{"notify without notification_ids", bulkMonitorRequest{Action: bulkActionNotify, MonitorIDs: []int64{a.ID}}},
		{"notify with unknown notification", bulkMonitorRequest{Action: bulkActionNotify, MonitorIDs: []int64{a.ID}, NotificationIDs: []int64{missing}}},
		{"schedule with zero interval", bulkMonitorRequest{Action: bulkActionSchedule, MonitorIDs: []int64{a.ID}, IntervalSeconds: 0}},
		{"schedule with zero timeout", bulkMonitorRequest{Action: bulkActionSchedule, MonitorIDs: []int64{a.ID}, IntervalSeconds: 60, TimeoutSeconds: &zero}},
		{"invalid id", bulkMonitorRequest{Action: bulkActionPause, MonitorIDs: []int64{0}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := h.requestWith(http.MethodPost, "/api/v1/monitors/bulk", c.body, admin)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("= %d, want 400, body %s", rec.Code, rec.Body.String())
			}
		})
	}

	// Nothing above should have touched the monitor.
	m, _ := h.store.GetMonitor(t.Context(), a.ID)
	if !m.Active {
		t.Fatal("monitor was mutated by a rejected bulk request")
	}
}

// TestBulkDeduplicatesIDs pins the dedupe: a repeated id must not be counted
// twice, and for delete it must not report a spurious "not found" on its second
// pass over an id the first pass already removed.
func TestBulkDeduplicatesIDs(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	a := h.createMonitor(admin, monitorRequest{Name: "a", Type: "fake", Interval: 30})

	resp := h.bulk(admin, bulkMonitorRequest{Action: bulkActionDelete, MonitorIDs: []int64{a.ID, a.ID}})
	if resp.Succeeded != 1 || resp.Failed != 0 {
		t.Fatalf("duplicate ids = %d ok / %d failed, want 1/0 (%+v)", resp.Succeeded, resp.Failed, resp.Results)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("results = %d, want 1 after dedupe", len(resp.Results))
	}
}

// TestBulkRequiresWriterRole keeps the batch endpoint behind the same role gate
// as the single-monitor mutations it performs.
func TestBulkRequiresWriterRole(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	a := h.createMonitor(admin, monitorRequest{Name: "a", Type: "fake", Interval: 30})

	viewer := h.createUser(admin, "viewer@example.com", "viewer")
	rec := h.requestWith(http.MethodPost, "/api/v1/monitors/bulk",
		bulkMonitorRequest{Action: bulkActionPause, MonitorIDs: []int64{a.ID}}, viewer)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer bulk = %d, want 403", rec.Code)
	}
	m, _ := h.store.GetMonitor(t.Context(), a.ID)
	if !m.Active {
		t.Fatal("viewer's rejected bulk still paused the monitor")
	}
}

// A bulk schedule change that the engine refuses is REPORTED, not counted as a
// success.
//
// The row-only write is the failure this action's own comment rules out: the
// interval is persisted, the engine rejects the Upsert (a config it cannot open,
// a type it does not know), and the monitor keeps checking on its OLD cadence.
// Swallowing the error told the caller the change had landed, and the per-id
// result exists precisely so it can be told which monitor did not take it —
// which is what bulkActionResume already does.
func TestBulkScheduleReportsAnEngineRefusal(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	a := h.createMonitor(admin, monitorRequest{Name: "a", Type: "fake", Interval: 30})

	h.engine.failUpsert(errors.New("config cannot be opened"))
	resp := h.bulk(admin, bulkMonitorRequest{
		Action: bulkActionSchedule, MonitorIDs: []int64{a.ID}, IntervalSeconds: 120,
	})

	if resp.Succeeded != 0 || resp.Failed != 1 {
		t.Fatalf("bulk schedule = %d ok / %d failed, want 0/1 — an engine refusal was "+
			"reported as success, so the monitor keeps its old interval silently (%+v)",
			resp.Succeeded, resp.Failed, resp.Results)
	}
	if len(resp.Results) != 1 || resp.Results[0].Error == "" {
		t.Fatalf("results = %+v, want a reason naming the failure", resp.Results)
	}
}

// Creating or saving a monitor the scheduler refuses says so in the response.
//
// handleResumeMonitor states the principle — answering success is "how a monitor
// comes to look healthy in the UI while nothing watches it" — and the import path
// warns "created but not scheduled" for the same reason. Create and update only
// logged it, so the row read active, the dashboard showed it, and nothing was
// checking it. Reachable in production: a config the box cannot open comes back
// from decryptMonitorConfig as something the checker rejects, which is the
// incident #79 describes.
//
// Neither answers 5xx. Create must not, because the row already exists and a
// retry would produce a duplicate; update follows it so the two agree and so a
// save that landed is not reported as a server error.
func TestUnschedulableMonitorReportsItOnCreateAndUpdate(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	h.engine.failUpsert(errors.New("config cannot be opened"))

	rec := h.requestWith(http.MethodPost, "/api/v1/monitors", monitorRequest{
		Name: "unschedulable", Type: "fake", Interval: 60, RetryInterval: 60,
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201 (the row exists; a 5xx would invite a duplicate)", rec.Code)
	}
	var created monitorView
	mustJSON(t, rec, &created)
	if created.SchedulingError == "" {
		t.Fatal("create reported no scheduling error; the monitor reads active while " +
			"nothing is checking it")
	}

	// The same on save.
	upd := h.requestWith(http.MethodPut, "/api/v1/monitors/"+itoa64(created.ID), monitorRequest{
		Name: "unschedulable", Type: "fake", Interval: 120, RetryInterval: 60,
	}, admin)
	if upd.Code != http.StatusOK {
		t.Fatalf("update = %d, want 200", upd.Code)
	}
	var saved monitorView
	mustJSON(t, upd, &saved)
	if saved.SchedulingError == "" {
		t.Fatal("update reported no scheduling error")
	}

	// And a healthy engine says nothing at all.
	h.engine.failUpsert(nil)
	ok := h.requestWith(http.MethodPut, "/api/v1/monitors/"+itoa64(created.ID), monitorRequest{
		Name: "unschedulable", Type: "fake", Interval: 60, RetryInterval: 60,
	}, admin)
	var healthy monitorView
	mustJSON(t, ok, &healthy)
	if healthy.SchedulingError != "" {
		t.Fatalf("a successful save reported %q", healthy.SchedulingError)
	}
}
