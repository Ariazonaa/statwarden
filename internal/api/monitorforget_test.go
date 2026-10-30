package api

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/notify"
)

// fakeDispatcher records which monitors the API asked it to forget.
type fakeDispatcher struct {
	mu       sync.Mutex
	forgot   []int64
	reset    []int64
	testSent int
}

func (f *fakeDispatcher) TestSend(context.Context, notify.Notification) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.testSent++
	return nil
}

func (f *fakeDispatcher) Forget(monitorID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forgot = append(f.forgot, monitorID)
}

func (f *fakeDispatcher) Reset(monitorID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reset = append(f.reset, monitorID)
}

func (f *fakeDispatcher) wasReset() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.reset...)
}

func (f *fakeDispatcher) forgotten() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.forgot...)
}

// Every path that takes a monitor away from the scheduler has to tell the
// dispatcher as well.
//
// The delete is a SOFT delete now, and it stops the checks. If the monitor was
// down at that moment, the outage the dispatcher is holding is unclosable: the
// recovery event that would have closed it can never be produced, because
// nothing is checking any more. What survives is a resend timer that keeps
// announcing an outage on a binned monitor, and a `notified` set that makes the
// FIRST alert after a restore a no-op.
func TestDeletingAMonitorTellsTheDispatcher(t *testing.T) {
	disp := &fakeDispatcher{}
	h := newHarnessTweaked(t, func(o *Options) { o.Dispatcher = disp })
	admin := h.setupAdmin()

	create := func(name string) int64 {
		rec := h.requestWith(http.MethodPost, "/api/v1/monitors", map[string]any{
			"name": name, "type": "fake", "interval_seconds": 60, "config": map[string]any{},
		}, admin)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s = %d, body %s", name, rec.Code, rec.Body.String())
		}
		var mon struct {
			ID int64 `json:"id"`
		}
		mustJSON(t, rec, &mon)
		return mon.ID
	}

	single := create("single")
	bulkA, bulkB := create("bulk-a"), create("bulk-b")

	if rec := h.requestWith(http.MethodDelete, "/api/v1/monitors/"+id64(single), nil, admin); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, body %s", rec.Code, rec.Body.String())
	}
	// The bulk path is a separate switch arm and used to be fixed on its own.
	rec := h.requestWith(http.MethodPost, "/api/v1/monitors/bulk", map[string]any{
		"action": "delete", "monitor_ids": []int64{bulkA, bulkB},
	}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("bulk delete = %d, body %s", rec.Code, rec.Body.String())
	}
	// A restore clears it too: whatever is left over is about the monitor's
	// previous life.
	if rec := h.requestWith(http.MethodPost, "/api/v1/monitors/"+id64(single)+"/restore", nil, admin); rec.Code != http.StatusOK {
		t.Fatalf("restore = %d, body %s", rec.Code, rec.Body.String())
	}

	got := disp.forgotten()
	want := map[int64]int{single: 1, bulkA: 1, bulkB: 1}
	seen := map[int64]int{}
	for _, id := range got {
		seen[id]++
	}
	for id, n := range want {
		if seen[id] != n {
			t.Errorf("monitor %d was forgotten %d times, want %d (all calls: %v)", id, seen[id], n, got)
		}
	}
	// The restore calls Reset, not Forget: the monitor is back and must be able
	// to alert immediately, which means lifting the delete's tombstone.
	reset := disp.wasReset()
	if len(reset) != 1 || reset[0] != single {
		t.Errorf("restore reset %v, want exactly [%d]", reset, single)
	}
}

// A nil dispatcher is the normal configuration for most of the suite and for
// DEV_MODE, and must not turn a delete into a panic.
func TestDeletingAMonitorWithoutADispatcher(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	rec := h.requestWith(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "lonely", "type": "fake", "interval_seconds": 60, "config": map[string]any{},
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d", rec.Code)
	}
	var mon struct {
		ID int64 `json:"id"`
	}
	mustJSON(t, rec, &mon)
	if rec := h.requestWith(http.MethodDelete, "/api/v1/monitors/"+id64(mon.ID), nil, admin); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
}

// Pausing a monitor must tell the dispatcher too.
//
// The engine emits no event for a pause — deliberately, it is not an outage — so
// nothing else closes an outage the monitor may be in. Left alone, its resend
// timer announces an outage on a monitor nobody is checking, and resuming it
// later meets a full `notified` set that swallows the first real alert. Deleting
// both calls left the whole tree green.
func TestPausingAMonitorResetsTheDispatcher(t *testing.T) {
	disp := &fakeDispatcher{}
	h := newHarnessTweaked(t, func(o *Options) { o.Dispatcher = disp })
	admin := h.setupAdmin()

	create := func(name string) int64 {
		rec := h.requestWith(http.MethodPost, "/api/v1/monitors", map[string]any{
			"name": name, "type": "fake", "interval_seconds": 60, "config": map[string]any{},
		}, admin)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s = %d, body %s", name, rec.Code, rec.Body.String())
		}
		var mon struct {
			ID int64 `json:"id"`
		}
		mustJSON(t, rec, &mon)
		return mon.ID
	}

	single := create("single")
	bulk := create("bulk")

	if rec := h.requestWith(http.MethodPost, "/api/v1/monitors/"+id64(single)+"/pause", nil, admin); rec.Code != http.StatusNoContent {
		t.Fatalf("pause = %d, body %s", rec.Code, rec.Body.String())
	}
	rec := h.requestWith(http.MethodPost, "/api/v1/monitors/bulk", map[string]any{
		"action": "pause", "monitor_ids": []int64{bulk},
	}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("bulk pause = %d, body %s", rec.Code, rec.Body.String())
	}

	reset := disp.wasReset()
	seen := map[int64]bool{}
	for _, id := range reset {
		seen[id] = true
	}
	if !seen[single] {
		t.Errorf("pausing a monitor did not reset the dispatcher (calls: %v)", reset)
	}
	if !seen[bulk] {
		t.Errorf("bulk-pausing a monitor did not reset the dispatcher (calls: %v)", reset)
	}
	// A pause is not a delete: the monitor must NOT be tombstoned, or it could
	// never alert again after being resumed.
	for _, id := range disp.forgotten() {
		if id == single || id == bulk {
			t.Errorf("pausing monitor %d tombstoned it; it would never alert again", id)
		}
	}
}
