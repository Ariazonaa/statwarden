package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/notify/secrets"
)

// pauseMonitor takes a monitor out of the scheduler the way the API does, so a
// following resume exercises the "engine does not know this one" branch.
func pauseMonitor(t *testing.T, h *harness, admin *session, id int64) {
	t.Helper()
	rec := h.requestWith(http.MethodPost, monitorPath(id, "/pause"), nil, admin)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("pause = %d, body %s", rec.Code, rec.Body.String())
	}
}

func monitorPath(id int64, suffix string) string {
	return "/api/v1/monitors/" + itoa64(id) + suffix
}

// createMonitorWithConfig makes a monitor whose config is stored the way a real
// one is — sealed when the harness has a box.
func createMonitorWithConfig(t *testing.T, h *harness, admin *session, cfg string) int64 {
	t.Helper()
	rec := h.requestWith(http.MethodPost, "/api/v1/monitors", monitorRequest{
		Name: "resume-target", Type: "fake", Interval: 60,
		Config: json.RawMessage(cfg),
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, body %s", rec.Code, rec.Body.String())
	}
	var m monitorView
	mustJSON(t, rec, &m)
	return m.ID
}

// TestBulkResumeSchedulesWithAnOpenedConfig is the bulk half of a bug the
// single-monitor handler did not have.
//
// monitors.config is stored SEALED. Resuming a monitor the scheduler does not
// know loads the row and upserts it, and the bulk path handed over the sealed
// envelope: the checker rejects it, the monitor is not scheduled, and the error
// was discarded. The result was the worst possible combination — the database
// says active, the API says success, and nothing checks the monitor until the
// process is restarted.
func TestBulkResumeSchedulesWithAnOpenedConfig(t *testing.T) {
	box, err := secrets.New("test-key-for-resume")
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	h := newHarnessWithBox(t, box)
	admin := h.setupAdmin()

	id := createMonitorWithConfig(t, h, admin, `{"url":"https://example.test"}`)
	pauseMonitor(t, h, admin, id)

	// The scheduler has forgotten it, exactly as after a restart or for a
	// monitor created while paused.
	h.engine.failResume(errors.New("not scheduled"))

	rec := h.requestWith(http.MethodPost, "/api/v1/monitors/bulk",
		map[string]any{"action": "resume", "monitor_ids": []int64{id}}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("bulk resume = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp bulkMonitorResponse
	mustJSON(t, rec, &resp)
	if resp.Succeeded != 1 || resp.Failed != 0 {
		t.Fatalf("bulk result = %+v, want 1 succeeded", resp)
	}

	got := h.engine.lastUpsert(t)
	if strings.Contains(string(got.Config), "$encrypted") {
		t.Fatalf("the engine was handed a sealed config, so the monitor is not "+
			"actually scheduled: %s", got.Config)
	}
	var probe map[string]any
	if err := json.Unmarshal(got.Config, &probe); err != nil {
		t.Fatalf("config handed to the engine is not readable JSON: %v", err)
	}
	if probe["url"] != "https://example.test" {
		t.Fatalf("config handed to the engine = %s, want the plaintext original", got.Config)
	}
}

// A resume that cannot schedule must not report success. Both entry points.
func TestResumeReportsAFailureToSchedule(t *testing.T) {
	t.Run("single", func(t *testing.T) {
		h := newHarness(t)
		admin := h.setupAdmin()
		id := createMonitorWithConfig(t, h, admin, `{}`)
		pauseMonitor(t, h, admin, id)
		h.engine.failResume(errors.New("not scheduled"))
		h.engine.failUpsert(errors.New("config unusable"))

		rec := h.requestWith(http.MethodPost, monitorPath(id, "/resume"), nil, admin)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("resume = %d, want 500 — a monitor that is not being checked "+
				"must not report success; body %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("bulk", func(t *testing.T) {
		h := newHarness(t)
		admin := h.setupAdmin()
		id := createMonitorWithConfig(t, h, admin, `{}`)
		pauseMonitor(t, h, admin, id)
		h.engine.failResume(errors.New("not scheduled"))
		h.engine.failUpsert(errors.New("config unusable"))

		rec := h.requestWith(http.MethodPost, "/api/v1/monitors/bulk",
			map[string]any{"action": "resume", "monitor_ids": []int64{id}}, admin)
		var resp bulkMonitorResponse
		mustJSON(t, rec, &resp)
		if resp.Failed != 1 || resp.Succeeded != 0 {
			t.Fatalf("bulk result = %+v, want the unschedulable monitor reported "+
				"as failed", resp)
		}
	})
}
