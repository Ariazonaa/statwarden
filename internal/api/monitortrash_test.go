package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// trashFixture is an admin plus one monitor, ready to be deleted.
type trashFixture struct {
	h     *harness
	admin *session
	monID int64
}

func newTrashFixture(t *testing.T) *trashFixture {
	t.Helper()
	h := newHarness(t)
	admin := h.setupAdmin()
	rec := h.requestWith(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "api", "type": "fake", "interval_seconds": 60, "config": map[string]any{},
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create monitor = %d, body %s", rec.Code, rec.Body.String())
	}
	var mon struct {
		ID int64 `json:"id"`
	}
	mustJSON(t, rec, &mon)
	return &trashFixture{h: h, admin: admin, monID: mon.ID}
}

func (f *trashFixture) delete(t *testing.T) {
	t.Helper()
	rec := f.h.requestWith(http.MethodDelete, "/api/v1/monitors/"+id64(f.monID), nil, f.admin)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, body %s", rec.Code, rec.Body.String())
	}
}

func (f *trashFixture) trash(t *testing.T) []monitorView {
	t.Helper()
	rec := f.h.requestWith(http.MethodGet, "/api/v1/monitors/trash", nil, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("list trash = %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []monitorView `json:"items"`
	}
	mustJSON(t, rec, &body)
	return body.Items
}

// The point of the whole feature: a deleted monitor is gone from every ordinary
// read, and still there to bring back.
func TestDeletingBinsAMonitorInsteadOfDestroyingIt(t *testing.T) {
	f := newTrashFixture(t)
	f.delete(t)

	if rec := f.h.requestWith(http.MethodGet, "/api/v1/monitors/"+id64(f.monID), nil, f.admin); rec.Code != http.StatusNotFound {
		t.Errorf("GET a binned monitor = %d, want 404", rec.Code)
	}
	rec := f.h.requestWith(http.MethodGet, "/api/v1/monitors", nil, f.admin)
	if strings.Contains(rec.Body.String(), `"name":"api"`) {
		t.Errorf("a binned monitor is still in the list:\n%s", rec.Body.String())
	}

	bin := f.trash(t)
	if len(bin) != 1 || bin[0].Name != "api" {
		t.Fatalf("bin = %+v, want the deleted monitor", bin)
	}
}

// The engine must stop checking it. The store clears `active`, which is what
// every scheduler-side query selects on — but the handler also has to tell the
// running engine, or it keeps the job until the next restart.
func TestDeletingRemovesTheMonitorFromTheEngine(t *testing.T) {
	f := newTrashFixture(t)
	f.delete(t)
	if len(f.h.engine.removed) != 1 || f.h.engine.removed[0] != f.monID {
		t.Fatalf("engine.removed = %v, want [%d]", f.h.engine.removed, f.monID)
	}
}

// A restored monitor comes back PAUSED. Resuming checks — and alerts — for
// something that has been gone for a week is not what a restore should do on its
// own, and neither store can know what `active` was before the delete.
func TestRestoringBringsTheMonitorBackPaused(t *testing.T) {
	f := newTrashFixture(t)
	f.delete(t)

	// Counted, not scanned: creating the monitor already put one upsert in here,
	// so "is it in the list" would pass whatever restore does.
	upsertsBefore := len(f.h.engine.upserts)

	rec := f.h.requestWith(http.MethodPost, "/api/v1/monitors/"+id64(f.monID)+"/restore", nil, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore = %d, body %s", rec.Code, rec.Body.String())
	}
	// Restoring must not schedule it: the monitor is paused, and the engine is
	// deliberately told nothing.
	if len(f.h.engine.upserts) != upsertsBefore {
		t.Error("restore scheduled the monitor; it is paused and must stay unscheduled")
	}
	var restored monitorView
	mustJSON(t, rec, &restored)
	if restored.Active {
		t.Error("a restored monitor is active; it must come back paused")
	}
	if restored.Status != "paused" {
		t.Errorf("restored status = %q, want paused", restored.Status)
	}
	// And it is readable again, out of the bin.
	if rec := f.h.requestWith(http.MethodGet, "/api/v1/monitors/"+id64(f.monID), nil, f.admin); rec.Code != http.StatusOK {
		t.Errorf("GET after restore = %d, want 200", rec.Code)
	}
	if len(f.trash(t)) != 0 {
		t.Error("the monitor is still in the bin after being restored")
	}
}

// The history is the reason the bin exists. If a restore came back without it,
// this would be a delete with a delay.
func TestRestoringKeepsTheHeartbeatHistory(t *testing.T) {
	f := newTrashFixture(t)
	f.h.store.AddHeartbeat(HeartbeatView{
		MonitorID: f.monID, Status: "up", Important: true,
	})

	f.delete(t)
	rec := f.h.requestWith(http.MethodPost, "/api/v1/monitors/"+id64(f.monID)+"/restore", nil, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore = %d", rec.Code)
	}

	rec = f.h.requestWith(http.MethodGet, "/api/v1/monitors/"+id64(f.monID)+"/heartbeats", nil, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeats = %d, body %s", rec.Code, rec.Body.String())
	}
	// The envelope is `heartbeats`, not `items` — this endpoint predates
	// writeList and answers its own shape.
	var body struct {
		Heartbeats []HeartbeatView `json:"heartbeats"`
	}
	mustJSON(t, rec, &body)
	if len(body.Heartbeats) != 1 {
		t.Fatalf("got %d heartbeats after a restore, want the one that was there", len(body.Heartbeats))
	}
}

// Every narrow write has its own SQL and its own chance to forget the filter. A
// binned monitor must not be pausable, resumable, movable, taggable or
// re-scheduled — those are edits to something that, as far as every read is
// concerned, does not exist.
func TestABinnedMonitorCannotBeEdited(t *testing.T) {
	f := newTrashFixture(t)
	f.delete(t)
	id := id64(f.monID)

	for _, tc := range []struct {
		name, method, path string
		body               any
	}{
		{"update", http.MethodPut, "/api/v1/monitors/" + id, map[string]any{
			"name": "renamed", "type": "fake", "interval_seconds": 60, "config": map[string]any{},
		}},
		{"pause", http.MethodPost, "/api/v1/monitors/" + id + "/pause", nil},
		{"resume", http.MethodPost, "/api/v1/monitors/" + id + "/resume", nil},
		{"delete again", http.MethodDelete, "/api/v1/monitors/" + id, nil},
	} {
		rec := f.h.requestWith(tc.method, tc.path, tc.body, f.admin)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s on a binned monitor = %d, want 404 (body %s)", tc.name, rec.Code, rec.Body.String())
		}
	}
}

// Purging is the irreversible step, so it is reachable only through the bin.
// Allowing it on a live monitor would give back the one-click destruction the
// bin was added to remove.
func TestPurgeRefusesALiveMonitor(t *testing.T) {
	f := newTrashFixture(t)
	rec := f.h.requestWith(http.MethodDelete, "/api/v1/monitors/"+id64(f.monID)+"/purge", nil, f.admin)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("purge of a live monitor = %d, want 404", rec.Code)
	}
	if rec := f.h.requestWith(http.MethodGet, "/api/v1/monitors/"+id64(f.monID), nil, f.admin); rec.Code != http.StatusOK {
		t.Fatal("the live monitor was destroyed by a purge it should have refused")
	}
}

func TestPurgeDestroysABinnedMonitor(t *testing.T) {
	f := newTrashFixture(t)
	f.delete(t)

	rec := f.h.requestWith(http.MethodDelete, "/api/v1/monitors/"+id64(f.monID)+"/purge", nil, f.admin)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("purge = %d, body %s", rec.Code, rec.Body.String())
	}
	if len(f.trash(t)) != 0 {
		t.Error("the monitor is still in the bin after a purge")
	}
	// And it cannot be restored: there is nothing left.
	if rec := f.h.requestWith(http.MethodPost, "/api/v1/monitors/"+id64(f.monID)+"/restore", nil, f.admin); rec.Code != http.StatusNotFound {
		t.Errorf("restore after purge = %d, want 404", rec.Code)
	}
}

// Purging is admin-only; the bin exists so the irreversible step is separate and
// deliberate, and a writer keeps every reversible one.
func TestPurgeIsAdminOnlyButRestoreIsNot(t *testing.T) {
	f := newTrashFixture(t)
	writer := f.h.createUser(f.admin, "writer@example.com", "user")
	f.delete(t)

	if rec := f.h.requestWith(http.MethodDelete, "/api/v1/monitors/"+id64(f.monID)+"/purge", nil, writer); rec.Code != http.StatusForbidden {
		t.Errorf("writer purge = %d, want 403", rec.Code)
	}
	if rec := f.h.requestWith(http.MethodPost, "/api/v1/monitors/"+id64(f.monID)+"/restore", nil, writer); rec.Code != http.StatusOK {
		t.Errorf("writer restore = %d, want 200", rec.Code)
	}
}

// A viewer may read monitors but not delete them, so the bin — a list of things
// somebody deleted — is not theirs to see either.
func TestTheBinIsNotVisibleToAViewer(t *testing.T) {
	f := newTrashFixture(t)
	viewer := f.h.createUser(f.admin, "viewer@example.com", "viewer")
	if rec := f.h.requestWith(http.MethodGet, "/api/v1/monitors/trash", nil, viewer); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer trash = %d, want 403", rec.Code)
	}
}

// chi matches the literal segment before the parameter, so /monitors/trash is
// the bin and not "the monitor whose id is trash". Pinned because the two routes
// are registered in one file and reordering them would silently lose the bin.
func TestTrashIsARouteNotAMonitorId(t *testing.T) {
	f := newTrashFixture(t)
	rec := f.h.requestWith(http.MethodGet, "/api/v1/monitors/trash", nil, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /monitors/trash = %d, want the bin listing", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"items"`) {
		t.Fatalf("GET /monitors/trash answered something else: %s", rec.Body.String())
	}
}

// The bulk endpoint is the reason this feature exists: one confirmation removes
// a whole selection, and before the bin there was nothing to undo it with.
func TestBulkDeleteGoesToTheBin(t *testing.T) {
	f := newTrashFixture(t)
	rec := f.h.requestWith(http.MethodPost, "/api/v1/monitors/bulk", map[string]any{
		"action": "delete", "monitor_ids": []int64{f.monID},
	}, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("bulk delete = %d, body %s", rec.Code, rec.Body.String())
	}
	if bin := f.trash(t); len(bin) != 1 {
		t.Fatalf("bulk delete put %d monitors in the bin, want 1", len(bin))
	}
}

// Binning a parent un-suppresses its children rather than muting them forever.
//
// The engine looks a parent up in the SCHEDULED set, and a binned monitor is not
// scheduled (the delete clears `active`), so the suppression simply stops
// applying. Pinned here because the alternative — a child silently suppressed by
// a parent nobody can see any more — is the failure mode that would be hardest
// to diagnose.
func TestBinningAParentDoesNotStrandItsChildren(t *testing.T) {
	f := newTrashFixture(t)
	rec := f.h.requestWith(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "child", "type": "fake", "interval_seconds": 60,
		"config": map[string]any{}, "parent_id": f.monID,
	}, f.admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create child = %d, body %s", rec.Code, rec.Body.String())
	}
	var child struct {
		ID int64 `json:"id"`
	}
	mustJSON(t, rec, &child)

	f.delete(t)

	// The child survives the parent going to the bin, and is still readable.
	if rec := f.h.requestWith(http.MethodGet, "/api/v1/monitors/"+id64(child.ID), nil, f.admin); rec.Code != http.StatusOK {
		t.Fatalf("child after binning the parent = %d, want 200", rec.Code)
	}
}

// A child whose parent is in the bin cannot be saved until the dangling parent
// is cleared. That is deliberate: stopping the cycle walk at a binned ancestor
// would let an edge be added that only closes a loop once it is restored, and a
// cycle in the parent graph is a live hazard in the alerting path.
func TestEditingAChildOfABinnedParentNamesTheDanglingLink(t *testing.T) {
	f := newTrashFixture(t)
	rec := f.h.requestWith(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "child", "type": "fake", "interval_seconds": 60,
		"config": map[string]any{}, "parent_id": f.monID,
	}, f.admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create child = %d", rec.Code)
	}
	var child struct {
		ID int64 `json:"id"`
	}
	mustJSON(t, rec, &child)
	f.delete(t)

	rec = f.h.requestWith(http.MethodPut, "/api/v1/monitors/"+id64(child.ID), map[string]any{
		"name": "child renamed", "type": "fake", "interval_seconds": 60,
		"config": map[string]any{}, "parent_id": f.monID,
	}, f.admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("saving a child that still points at a binned parent = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "parent_id") {
		t.Errorf("the error does not name the field to fix: %s", rec.Body.String())
	}

	// Clearing the parent makes the same edit work, so the way out is one field.
	rec = f.h.requestWith(http.MethodPut, "/api/v1/monitors/"+id64(child.ID), map[string]any{
		"name": "child renamed", "type": "fake", "interval_seconds": 60,
		"config": map[string]any{},
	}, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("saving the child without a parent = %d, body %s", rec.Code, rec.Body.String())
	}
}

// The bin is a READ path, and every read path owes the caller the same two
// redactions: the config's secrets and the push token.
//
// This was a real leak in the first cut of the feature. The bin handler called
// toMonitorView directly — the raw mapper, which copies both verbatim — because
// forgetting the redaction afterwards is a silent leak and not a compile error.
// Both call sites go through safeMonitorView now, and this is what keeps them
// there.
func TestTheBinDoesNotLeakMonitorSecrets(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	// An inline URL password is masked for every type, so the harness's "fake"
	// checker is enough to exercise the redaction end to end.
	rec := h.requestWith(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "creds", "type": "fake", "interval_seconds": 60,
		"config": map[string]any{"url": "https://admin:hunter2@example.test/health"},
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, body %s", rec.Code, rec.Body.String())
	}
	var mon struct {
		ID int64 `json:"id"`
	}
	mustJSON(t, rec, &mon)

	if rec := h.requestWith(http.MethodDelete, "/api/v1/monitors/"+id64(mon.ID), nil, admin); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}

	rec = h.requestWith(http.MethodGet, "/api/v1/monitors/trash", nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("trash = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "hunter2") {
		t.Fatalf("the recycle bin leaked an inline URL password: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"push_token"`) {
		t.Errorf("the recycle bin carries a push_token field: %s", rec.Body.String())
	}

	// And the restore response, which is the same mapper on a different path.
	rec = h.requestWith(http.MethodPost, "/api/v1/monitors/"+id64(mon.ID)+"/restore", nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "hunter2") {
		t.Fatalf("the restore response leaked an inline URL password: %s", rec.Body.String())
	}
}

// The whole round trip a person actually performs: delete by mistake, restore,
// resume — and the monitor is being CHECKED again at the end of it.
//
// Worth crossing all three subsystems in one test, because each step hands off
// to the next through a different mechanism: the delete clears `active` (which
// is what the scheduler selects on), the restore deliberately does NOT touch the
// engine, and only the resume schedules — through scheduleActive, whose whole
// reason for existing is the monitor the engine has never heard of. A break
// anywhere in that chain leaves a monitor that looks healthy in the UI while
// nothing watches it.
func TestDeleteRestoreResumePutsTheMonitorBackOnTheSchedule(t *testing.T) {
	f := newTrashFixture(t)
	// Pretend the process restarted: the engine has no memory of this monitor,
	// so Resume fails and scheduleActive has to load and upsert it.
	f.h.engine.resumeErr = errors.New("not scheduled")
	f.h.engine.upserts = nil

	f.delete(t)
	if rec := f.h.requestWith(http.MethodPost, "/api/v1/monitors/"+id64(f.monID)+"/restore", nil, f.admin); rec.Code != http.StatusOK {
		t.Fatalf("restore = %d", rec.Code)
	}
	if len(f.h.engine.upserts) != 0 {
		t.Fatal("restore scheduled the monitor; it comes back paused and must stay unscheduled")
	}

	rec := f.h.requestWith(http.MethodPost, "/api/v1/monitors/"+id64(f.monID)+"/resume", nil, f.admin)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("resume = %d, body %s", rec.Code, rec.Body.String())
	}
	var scheduled bool
	for _, m := range f.h.engine.upserts {
		if m.ID == f.monID {
			scheduled = true
		}
	}
	if !scheduled {
		t.Fatalf("after delete → restore → resume the monitor is not on the schedule; "+
			"engine.upserts = %v", f.h.engine.upserts)
	}
}

// A restored monitor comes back with its tags and channels.
//
// The soft delete never touched either — that is the whole point of the bin —
// but the restore answered with an undecorated view, whose tag_ids and
// notification_ids are the empty arrays the raw mapper initialises. A client
// patching its list from the response showed the monitor stripped of both until
// the next full reload.
func TestRestoreAnswersWithTagsAndChannels(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	rec := h.requestWith(http.MethodPost, "/api/v1/tags", map[string]any{"name": "prod", "color": "#ff0000"}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create tag = %d, body %s", rec.Code, rec.Body.String())
	}
	var tag struct {
		ID int64 `json:"id"`
	}
	mustJSON(t, rec, &tag)

	rec = h.requestWith(http.MethodPost, "/api/v1/notifications", map[string]any{
		"name": "ops", "type": "fake", "active": true, "config": map[string]any{},
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create notification = %d, body %s", rec.Code, rec.Body.String())
	}
	var ch struct {
		ID int64 `json:"id"`
	}
	mustJSON(t, rec, &ch)

	rec = h.requestWith(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "tagged", "type": "fake", "interval_seconds": 60, "config": map[string]any{},
		"tag_ids": []int64{tag.ID}, "notification_ids": []int64{ch.ID},
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create monitor = %d, body %s", rec.Code, rec.Body.String())
	}
	var mon struct {
		ID int64 `json:"id"`
	}
	mustJSON(t, rec, &mon)

	if rec := h.requestWith(http.MethodDelete, "/api/v1/monitors/"+id64(mon.ID), nil, admin); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	rec = h.requestWith(http.MethodPost, "/api/v1/monitors/"+id64(mon.ID)+"/restore", nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore = %d, body %s", rec.Code, rec.Body.String())
	}
	var back struct {
		TagIDs          []int64 `json:"tag_ids"`
		NotificationIDs []int64 `json:"notification_ids"`
	}
	mustJSON(t, rec, &back)
	if len(back.TagIDs) != 1 || back.TagIDs[0] != tag.ID {
		t.Errorf("restored monitor came back with tag_ids %v, want [%d]", back.TagIDs, tag.ID)
	}
	if len(back.NotificationIDs) != 1 || back.NotificationIDs[0] != ch.ID {
		t.Errorf("restored monitor came back with notification_ids %v, want [%d]", back.NotificationIDs, ch.ID)
	}
}

// The push-token half of the redaction, asserted against a monitor that
// actually HAS one.
//
// TestTheBinDoesNotLeakMonitorSecrets creates a "fake" monitor, which carries no
// push token at all — so its `"push_token"` check passed whether or not the
// redaction was there, and would have gone on passing if safeMonitorView were
// swapped back for the raw mapper. A push monitor is the only shape that can
// tell the two apart, and the assertion is on the token's VALUE, because the
// field itself is omitempty and would be absent for the wrong reason.
func TestTheBinDoesNotLeakAPushToken(t *testing.T) {
	// newExtHarness, not newHarness: the plain one registers only the "fake"
	// checker, and a push monitor is the whole point of this test.
	h := newExtHarness(t, nil, nil)
	admin := h.setupAdmin()

	rec := h.requestWith(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "cron", "type": "push", "interval_seconds": 60, "config": map[string]any{},
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, body %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID        int64  `json:"id"`
		PushToken string `json:"push_token"`
	}
	mustJSON(t, rec, &created)
	// The create response is the one place the plaintext is revealed. If that
	// stops being true this test is asserting nothing, so it is checked.
	if len(created.PushToken) != 32 {
		t.Fatalf("create did not reveal a push token (%q); this test has nothing to look for",
			created.PushToken)
	}

	if rec := h.requestWith(http.MethodDelete, "/api/v1/monitors/"+id64(created.ID), nil, admin); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}

	rec = h.requestWith(http.MethodGet, "/api/v1/monitors/trash", nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("trash = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), created.PushToken) {
		t.Errorf("the recycle bin revealed a push token: %s", rec.Body.String())
	}
	// The FIELD too, not only the plaintext. The column stores a hash, so the
	// plaintext is unreachable from here whatever the mapper does — which is
	// exactly why a plaintext check alone cannot tell safeMonitorView from the
	// raw mapper. A monitor that HAS a token is the only case where the field
	// being absent means anything at all.
	if strings.Contains(rec.Body.String(), `"push_token"`) {
		t.Errorf("the recycle bin carries the push_token field for a monitor that has one: %s",
			rec.Body.String())
	}

	rec = h.requestWith(http.MethodPost, "/api/v1/monitors/"+id64(created.ID)+"/restore", nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), created.PushToken) {
		t.Errorf("the restore response revealed a push token: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"push_token"`) {
		t.Errorf("the restore response carries the push_token field: %s", rec.Body.String())
	}

	// A positive control: the redaction is real, not an artefact of the token
	// never having been stored. The regenerate endpoint still hands it over.
	//
	// Resumed first: a restore brings the monitor back PAUSED, and minting a push
	// token for a paused monitor is refused (there is nothing to receive the
	// heartbeats).
	if rec := h.requestWith(http.MethodPost, "/api/v1/monitors/"+id64(created.ID)+"/resume", nil, admin); rec.Code != http.StatusNoContent {
		t.Fatalf("resume = %d, body %s", rec.Code, rec.Body.String())
	}
	rec = h.requestWith(http.MethodPost, "/api/v1/monitors/"+id64(created.ID)+"/push-token", nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("regenerate = %d, body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"push_token"`) {
		t.Fatalf("regenerate returned no token, so the checks above prove nothing: %s", rec.Body.String())
	}
}

// Purging the last monitor of a scoped maintenance window must not widen the
// window to the whole fleet.
//
// maintenance_monitors cascades, so the purge empties the window's id set — and
// an empty set used to mean "every monitor". The retention sweep performs the
// same purge automatically about two weeks after a delete, so this happened on a
// timer with nobody present: a window scoped to one machine began suppressing
// alerts for everything and announcing itself on every public status page.
func TestPurgingTheLastMonitorDoesNotWidenAMaintenanceWindow(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	ctx := context.Background()

	rec := h.requestWith(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "db", "type": "fake", "interval_seconds": 60, "config": map[string]any{},
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create scoped monitor = %d", rec.Code)
	}
	var scopedMon struct {
		ID int64 `json:"id"`
	}
	mustJSON(t, rec, &scopedMon)

	rec = h.requestWith(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "unrelated", "type": "fake", "interval_seconds": 60, "config": map[string]any{},
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create bystander = %d", rec.Code)
	}
	var bystander struct {
		ID int64 `json:"id"`
	}
	mustJSON(t, rec, &bystander)

	from := time.Now().UTC().Add(-time.Hour)
	to := from.Add(4 * time.Hour)
	rec = h.requestWith(http.MethodPost, "/api/v1/maintenance", map[string]any{
		"title": "db upgrade", "strategy": "single", "timezone": "UTC",
		"starts_at": from.Format(time.RFC3339), "ends_at": to.Format(time.RFC3339),
		"monitor_ids": []int64{scopedMon.ID},
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create window = %d, body %s", rec.Code, rec.Body.String())
	}
	var win MaintenanceWindow
	mustJSON(t, rec, &win)
	if win.AppliesToAll {
		t.Fatalf("a window created with one monitor reports itself as fleet-wide: %+v", win)
	}

	// Bin and then purge the only monitor it covers.
	if rec := h.requestWith(http.MethodDelete, "/api/v1/monitors/"+id64(scopedMon.ID), nil, admin); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	if rec := h.requestWith(http.MethodDelete, "/api/v1/monitors/"+id64(scopedMon.ID)+"/purge", nil, admin); rec.Code != http.StatusNoContent {
		t.Fatalf("purge = %d", rec.Code)
	}

	stored, err := h.store.GetMaintenance(ctx, win.ID)
	if err != nil {
		t.Fatalf("GetMaintenance: %v", err)
	}
	if len(stored.MonitorIDs) != 0 {
		t.Fatalf("the cascade did not empty the window: %v", stored.MonitorIDs)
	}
	if stored.AppliesToAll {
		t.Fatal("the window became fleet-wide when its last monitor was purged")
	}

	// The bystander is not in maintenance.
	if h.maint != nil {
		if err := h.maint.Refresh(ctx); err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		if h.maint.InMaintenance(bystander.ID, time.Now().UTC()) {
			t.Error("an unrelated monitor is now suppressed by a window it was never in")
		}
	}
}

// An ordinary edit must not change a maintenance window's SCOPE.
//
// Migration 0019 stopped the scope changing on its own by making it a column.
// Deriving that column from the monitor list on every write undid the whole
// thing above the database: the editor always sends the list it loaded, so a
// window whose monitors were destroyed by the cascade sends an empty list, and
// re-deriving turned a covers-nothing window into a fleet-wide one on a title
// edit — with no audit trail and nobody having touched the scope.
func TestEditingAWindowDoesNotWidenItsScope(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	ctx := context.Background()

	rec := h.requestWith(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "db", "type": "fake", "interval_seconds": 60, "config": map[string]any{},
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create monitor = %d", rec.Code)
	}
	var mon struct {
		ID int64 `json:"id"`
	}
	mustJSON(t, rec, &mon)

	from := time.Now().UTC().Add(-time.Hour)
	to := from.Add(4 * time.Hour)
	rec = h.requestWith(http.MethodPost, "/api/v1/maintenance", map[string]any{
		"title": "db upgrade", "strategy": "single", "timezone": "UTC",
		"starts_at": from.Format(time.RFC3339), "ends_at": to.Format(time.RFC3339),
		"monitor_ids": []int64{mon.ID},
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create window = %d, body %s", rec.Code, rec.Body.String())
	}
	var win MaintenanceWindow
	mustJSON(t, rec, &win)
	if win.AppliesToAll {
		t.Fatalf("a scoped window was created fleet-wide: %+v", win)
	}

	// The cascade empties it.
	if rec := h.requestWith(http.MethodDelete, "/api/v1/monitors/"+id64(mon.ID), nil, admin); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	if rec := h.requestWith(http.MethodDelete, "/api/v1/monitors/"+id64(mon.ID)+"/purge", nil, admin); rec.Code != http.StatusNoContent {
		t.Fatalf("purge = %d", rec.Code)
	}

	// Now an ordinary edit, shaped exactly like the one the editor sends: every
	// field it loaded, including the (now empty) monitor list.
	rec = h.requestWith(http.MethodPut, "/api/v1/maintenance/"+id64(win.ID), map[string]any{
		"title": "db upgrade (rescheduled)", "strategy": "single", "timezone": "UTC",
		"starts_at": from.Format(time.RFC3339), "ends_at": to.Format(time.RFC3339),
		"monitor_ids": []int64{},
	}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d, body %s", rec.Code, rec.Body.String())
	}
	var edited MaintenanceWindow
	mustJSON(t, rec, &edited)
	if edited.AppliesToAll {
		t.Error("a title edit widened the window to the whole fleet")
	}
	stored, err := h.store.GetMaintenance(ctx, win.ID)
	if err != nil {
		t.Fatalf("GetMaintenance: %v", err)
	}
	if stored.AppliesToAll {
		t.Error("the stored window is now fleet-wide")
	}

	// Widening is still possible — it just has to be asked for.
	rec = h.requestWith(http.MethodPut, "/api/v1/maintenance/"+id64(win.ID), map[string]any{
		"title": "everything", "strategy": "single", "timezone": "UTC",
		"starts_at": from.Format(time.RFC3339), "ends_at": to.Format(time.RFC3339),
		"monitor_ids": []int64{}, "applies_to_all": true,
	}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("explicit widen = %d, body %s", rec.Code, rec.Body.String())
	}
	mustJSON(t, rec, &edited)
	if !edited.AppliesToAll {
		t.Error("an explicit applies_to_all=true was not honoured")
	}
}
