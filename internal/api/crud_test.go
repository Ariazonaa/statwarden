package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMonitorCreateDrivesEngineUpsert(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	before := h.engine.upsertCount()
	rec := h.requestWith(http.MethodPost, "/api/v1/monitors",
		monitorRequest{Name: "web", Type: "fake", Interval: 30}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, body %s", rec.Code, rec.Body.String())
	}
	if h.engine.upsertCount() != before+1 {
		t.Fatalf("engine Upsert calls = %d, want %d", h.engine.upsertCount(), before+1)
	}
	var m monitorView
	mustJSON(t, rec, &m)
	if m.ID == 0 || m.Interval != 30 || m.Type != "fake" {
		t.Fatalf("unexpected monitor view: %+v", m)
	}
}

// recovery_checks round-trips through the API and reaches the engine, and an
// omitted value (every client that predates the field) means one success.
func TestMonitorRecoveryChecksRoundTrip(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	rec := h.requestWith(http.MethodPost, "/api/v1/monitors",
		monitorRequest{Name: "web", Type: "fake", Interval: 30, RecoveryChecks: 3}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, body %s", rec.Code, rec.Body.String())
	}
	var m monitorView
	mustJSON(t, rec, &m)
	if m.RecoveryChecks != 3 {
		t.Fatalf("created recovery_checks = %d, want 3", m.RecoveryChecks)
	}
	if got := h.engine.lastUpsert(t).RecoveryChecks; got != 3 {
		t.Fatalf("engine got RecoveryChecks = %d, want 3", got)
	}

	// A full-replace PUT without the field falls back to the default.
	rec = h.requestWith(http.MethodPut, fmt.Sprintf("/api/v1/monitors/%d", m.ID),
		monitorRequest{Name: "web", Type: "fake", Interval: 30}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d, body %s", rec.Code, rec.Body.String())
	}
	mustJSON(t, rec, &m)
	if m.RecoveryChecks != 1 {
		t.Fatalf("recovery_checks after omitting it = %d, want the default 1", m.RecoveryChecks)
	}
}

func TestMonitorLifecycleEngineCalls(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	rec := h.requestWith(http.MethodPost, "/api/v1/monitors",
		monitorRequest{Name: "web", Type: "fake", Interval: 30}, admin)
	var m monitorView
	mustJSON(t, rec, &m)
	base := fmt.Sprintf("/api/v1/monitors/%d", m.ID)

	// Pause → engine.Pause.
	if rec := h.requestWith(http.MethodPost, base+"/pause", nil, admin); rec.Code != http.StatusNoContent {
		t.Fatalf("pause = %d", rec.Code)
	}
	// Resume → engine.Resume.
	if rec := h.requestWith(http.MethodPost, base+"/resume", nil, admin); rec.Code != http.StatusNoContent {
		t.Fatalf("resume = %d", rec.Code)
	}
	// Check now → engine.CheckNow.
	if rec := h.requestWith(http.MethodPost, base+"/check", nil, admin); rec.Code != http.StatusAccepted {
		t.Fatalf("check = %d", rec.Code)
	}
	// Delete → engine.Remove.
	if rec := h.requestWith(http.MethodDelete, base, nil, admin); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}

	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if len(h.engine.paused) != 1 || len(h.engine.resumed) != 1 || len(h.engine.checked) != 1 || len(h.engine.removed) != 1 {
		t.Fatalf("engine calls paused=%d resumed=%d checked=%d removed=%d",
			len(h.engine.paused), len(h.engine.resumed), len(h.engine.checked), len(h.engine.removed))
	}
}

func TestMonitorValidationErrors(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	cases := []struct {
		name string
		body monitorRequest
	}{
		{"missing name", monitorRequest{Type: "fake", Interval: 60}},
		{"unknown type", monitorRequest{Name: "x", Type: "nope", Interval: 60}},
		{"bad interval", monitorRequest{Name: "x", Type: "fake", Interval: 0}},
		{"negative recovery checks", monitorRequest{Name: "x", Type: "fake", Interval: 60, RecoveryChecks: -1}},
		{"absurd recovery checks", monitorRequest{Name: "x", Type: "fake", Interval: 60, RecoveryChecks: 101}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := h.requestWith(http.MethodPost, "/api/v1/monitors", c.body, admin)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s = %d, want 400", c.name, rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); ct[:len("application/problem+json")] != "application/problem+json" {
				t.Fatalf("content-type = %q, want problem+json", ct)
			}
		})
	}
}

func TestMonitorNotFound(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	rec := h.requestWith(http.MethodGet, "/api/v1/monitors/999", nil, admin)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get missing = %d, want 404", rec.Code)
	}
}

func TestMonitorPagination(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	for i := 0; i < 5; i++ {
		rec := h.requestWith(http.MethodPost, "/api/v1/monitors",
			monitorRequest{Name: fmt.Sprintf("m%d", i), Type: "fake", Interval: 60}, admin)
		if rec.Code != http.StatusCreated {
			t.Fatalf("seed create %d = %d", i, rec.Code)
		}
	}
	rec := h.requestWith(http.MethodGet, "/api/v1/monitors?limit=2&offset=1", nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	var env struct {
		Items  []monitorView `json:"items"`
		Total  int           `json:"total"`
		Limit  int           `json:"limit"`
		Offset int           `json:"offset"`
	}
	mustJSON(t, rec, &env)
	if env.Total != 5 {
		t.Fatalf("total = %d, want 5", env.Total)
	}
	if len(env.Items) != 2 {
		t.Fatalf("page size = %d, want 2", len(env.Items))
	}
	if env.Limit != 2 || env.Offset != 1 {
		t.Fatalf("limit/offset = %d/%d, want 2/1", env.Limit, env.Offset)
	}
}

func TestTagCRUD(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	rec := h.requestWith(http.MethodPost, "/api/v1/tags", tagRequest{Name: "prod"}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create tag = %d, body %s", rec.Code, rec.Body.String())
	}
	var tag Tag
	mustJSON(t, rec, &tag)
	if tag.Color == "" {
		t.Fatal("default color not applied")
	}
	// Duplicate name → conflict.
	rec = h.requestWith(http.MethodPost, "/api/v1/tags", tagRequest{Name: "prod"}, admin)
	if rec.Code != http.StatusConflict {
		t.Fatalf("dup tag = %d, want 409", rec.Code)
	}
	// Update + delete.
	rec = h.requestWith(http.MethodPut, fmt.Sprintf("/api/v1/tags/%d", tag.ID), tagRequest{Name: "production", Color: "#fff"}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("update tag = %d", rec.Code)
	}
	rec = h.requestWith(http.MethodDelete, fmt.Sprintf("/api/v1/tags/%d", tag.ID), nil, admin)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete tag = %d", rec.Code)
	}
}

func TestNotificationCRUDAndTest(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	rec := h.requestWith(http.MethodPost, "/api/v1/notifications",
		notificationRequest{Name: "chan", Type: "fake"}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create notification = %d, body %s", rec.Code, rec.Body.String())
	}
	var n Notification
	mustJSON(t, rec, &n)

	// Test send with no dispatcher configured → 503.
	rec = h.requestWith(http.MethodPost, fmt.Sprintf("/api/v1/notifications/%d/test", n.ID), nil, admin)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("test send without dispatcher = %d, want 503", rec.Code)
	}
}

func TestHeartbeatETag(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	rec := h.requestWith(http.MethodPost, "/api/v1/monitors",
		monitorRequest{Name: "web", Type: "fake", Interval: 60}, admin)
	var m monitorView
	mustJSON(t, rec, &m)
	h.store.AddHeartbeat(HeartbeatView{MonitorID: m.ID, Status: "up"})

	url := fmt.Sprintf("/api/v1/monitors/%d/heartbeats", m.ID)
	rec = h.requestWith(http.MethodGet, url, nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeats = %d", rec.Code)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag header")
	}
	// If-None-Match with the same tag → 304.
	rec2 := h.conditionalGet(url, etag, admin)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("conditional get = %d, want 304", rec2.Code)
	}
}

func TestSettingsAdminOnly(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	user := h.createUser(admin, "user@example.com", "user")

	// user (write role) cannot access settings (admin only).
	rec := h.requestWith(http.MethodGet, "/api/v1/settings", nil, user)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("user list settings = %d, want 403", rec.Code)
	}
	// admin can set a setting.
	rec = h.requestWith(http.MethodPut, "/api/v1/settings/theme", map[string]string{"mode": "dark"}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin set setting = %d, body %s", rec.Code, rec.Body.String())
	}
}

func TestRegistryListings(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	rec := h.requestWith(http.MethodGet, "/api/v1/registry/monitor-types", nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("monitor types = %d", rec.Code)
	}
	var types []monitorTypeInfo
	mustJSON(t, rec, &types)
	if len(types) != 1 || types[0].Type != "fake" {
		t.Fatalf("monitor types = %+v", types)
	}

	rec = h.requestWith(http.MethodGet, "/api/v1/registry/notification-types", nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("notification types = %d", rec.Code)
	}
}

// TestMonitorReadDTOMirrorsWriteDTO locks in the fix for the gap that made the
// dashboard read undefined: tag_ids and notification_ids were accepted by
// monitorRequest but absent from monitorView, so a client could not read back
// the associations it had just written.
func TestMonitorReadDTOMirrorsWriteDTO(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	var tag Tag
	mustJSON(t, h.requestWith(http.MethodPost, "/api/v1/tags", tagRequest{Name: "prod", Color: "#fff"}, admin), &tag)
	var notif Notification
	mustJSON(t, h.requestWith(http.MethodPost, "/api/v1/notifications",
		notificationRequest{Name: "ops", Type: "fake"}, admin), &notif)

	var created monitorView
	mustJSON(t, h.requestWith(http.MethodPost, "/api/v1/monitors", monitorRequest{
		Name: "web", Type: "fake", Interval: 30,
		Config:          json.RawMessage(`{"url":"https://example.com"}`),
		TagIDs:          []int64{tag.ID},
		NotificationIDs: []int64{notif.ID},
	}, admin), &created)
	if len(created.TagIDs) != 1 || created.TagIDs[0] != tag.ID {
		t.Fatalf("create echoed tag_ids = %v, want [%d]", created.TagIDs, tag.ID)
	}

	// The read path must agree with the write path, not just the create echo.
	var got monitorView
	mustJSON(t, h.requestWith(http.MethodGet, fmt.Sprintf("/api/v1/monitors/%d", created.ID), nil, admin), &got)
	if len(got.TagIDs) != 1 || got.TagIDs[0] != tag.ID {
		t.Fatalf("GET tag_ids = %v, want [%d]", got.TagIDs, tag.ID)
	}
	if len(got.NotificationIDs) != 1 || got.NotificationIDs[0] != notif.ID {
		t.Fatalf("GET notification_ids = %v, want [%d]", got.NotificationIDs, notif.ID)
	}
	// url is lifted out of config so the client does not re-guess it.
	if got.URL != "https://example.com" {
		t.Fatalf("GET url = %q, want the config url", got.URL)
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("GET created_at is zero; the model requires it")
	}
	// Empty rather than null: the client must not have to null-check.
	if got.Sparkline == nil {
		t.Fatal("GET sparkline is null, want []")
	}
}

// TestMonitorStatsSummariseHeartbeats covers the derived block the list view
// renders — latency, last check and the uptime windows — which had no server
// counterpart at all before.
func TestMonitorStatsSummariseHeartbeats(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	var m monitorView
	mustJSON(t, h.requestWith(http.MethodPost, "/api/v1/monitors",
		monitorRequest{Name: "web", Type: "fake", Interval: 30}, admin), &m)

	now := time.Now()
	lat := 120
	h.store.AddHeartbeat(HeartbeatView{MonitorID: m.ID, CheckedAt: now.Add(-3 * time.Minute), Status: "up", LatencyMs: &lat})
	h.store.AddHeartbeat(HeartbeatView{MonitorID: m.ID, CheckedAt: now.Add(-2 * time.Minute), Status: "down"})
	last := 90
	h.store.AddHeartbeat(HeartbeatView{MonitorID: m.ID, CheckedAt: now.Add(-1 * time.Minute), Status: "up", LatencyMs: &last})

	var got monitorView
	mustJSON(t, h.requestWith(http.MethodGet, fmt.Sprintf("/api/v1/monitors/%d", m.ID), nil, admin), &got)
	if got.LatencyMs == nil || *got.LatencyMs != last {
		t.Fatalf("latency_ms = %v, want the newest beat's %d", got.LatencyMs, last)
	}
	if got.LastCheckedAt == nil {
		t.Fatal("last_checked_at is nil despite recorded heartbeats")
	}
	// Two up of three conclusive beats, as a 0..1 fraction.
	if got.Uptime24h == nil || *got.Uptime24h < 0.66 || *got.Uptime24h > 0.67 {
		t.Fatalf("uptime_24h = %v, want ~0.667", got.Uptime24h)
	}
	if len(got.Sparkline) != 2 {
		t.Fatalf("sparkline = %v, want the two beats that carried a latency", got.Sparkline)
	}
}

// TestMonitorStatsWithoutHeartbeatsAreNull guards the "no data" case: a fresh
// monitor must report null uptime, not a misleading 0%.
func TestMonitorStatsWithoutHeartbeatsAreNull(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	var m monitorView
	mustJSON(t, h.requestWith(http.MethodPost, "/api/v1/monitors",
		monitorRequest{Name: "web", Type: "fake", Interval: 30}, admin), &m)

	var got monitorView
	mustJSON(t, h.requestWith(http.MethodGet, fmt.Sprintf("/api/v1/monitors/%d", m.ID), nil, admin), &got)
	if got.Uptime24h != nil || got.Uptime30d != nil || got.Uptime1y != nil || got.LatencyMs != nil {
		t.Fatalf("fresh monitor reported stats: %+v", got)
	}
}

// TestHeartbeatsImportantOnly covers the transition log the monitor detail page
// derives its state events from.
func TestHeartbeatsImportantOnly(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	var m monitorView
	mustJSON(t, h.requestWith(http.MethodPost, "/api/v1/monitors",
		monitorRequest{Name: "web", Type: "fake", Interval: 30}, admin), &m)

	now := time.Now()
	h.store.AddHeartbeat(HeartbeatView{MonitorID: m.ID, CheckedAt: now.Add(-2 * time.Minute), Status: "up"})
	h.store.AddHeartbeat(HeartbeatView{MonitorID: m.ID, CheckedAt: now.Add(-1 * time.Minute), Status: "down", Important: true})

	var all struct {
		Heartbeats []HeartbeatView `json:"heartbeats"`
	}
	mustJSON(t, h.requestWith(http.MethodGet, fmt.Sprintf("/api/v1/monitors/%d/heartbeats", m.ID), nil, admin), &all)
	if len(all.Heartbeats) != 2 {
		t.Fatalf("unfiltered heartbeats = %d, want 2", len(all.Heartbeats))
	}

	var important struct {
		Heartbeats []HeartbeatView `json:"heartbeats"`
	}
	mustJSON(t, h.requestWith(http.MethodGet, fmt.Sprintf("/api/v1/monitors/%d/heartbeats?important=true", m.ID), nil, admin), &important)
	if len(important.Heartbeats) != 1 || !important.Heartbeats[0].Important {
		t.Fatalf("important-only heartbeats = %+v, want the single transition", important.Heartbeats)
	}
}

// TestWriteJSONEncodesNilSliceAsArray pins the shape of an empty collection.
// encoding/json marshals a nil slice as `null`, and PgStore returns nil when a
// query matches nothing, so every list endpoint used to answer `null` against a
// real database. That is not cosmetic: the SPA maps over these responses and
// threw "Cannot read properties of null" on the status-pages screen.
//
// This drives writeJSON directly rather than going through the handlers,
// because MemStore returns empty non-nil slices where PgStore returns nil — a
// handler-level test would pass with or without the fix and guard nothing.
func TestWriteJSONEncodesNilSliceAsArray(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"nil slice of structs", []StatusPage(nil), "[]"},
		{"nil slice of pointers", []*StatusPage(nil), "[]"},
		{"nil slice of strings", []string(nil), "[]"},
		{"empty non-nil slice", []string{}, "[]"},
		{"populated slice", []string{"a"}, `["a"]`},
		{"nil map stays null", map[string]string(nil), "null"},
		{"struct passes through", struct {
			A int `json:"a"`
		}{1}, `{"a":1}`},
		{"scalar passes through", 42, "42"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeJSON(rec, http.StatusOK, tc.in)
			if got := strings.TrimSpace(rec.Body.String()); got != tc.want {
				t.Errorf("writeJSON(%#v) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

// The store's own cycle check, independent of the API's pre-flight walk.
//
// The API validates first and gives a friendlier message, so this path is only
// reachable when two parent changes race — which MemStore's single mutex cannot
// reproduce. The check is still worth having here: it is the one that runs
// inside the writing transaction in PgStore, and a test that never exercises it
// would let it rot. The concurrent case itself is in
// parent_cycle_integration_test.go, against a real database.
func TestStoreRejectsAParentCycle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	a, err := h.store.CreateMonitor(ctx, MonitorInput{Name: "a", Type: "fake", Interval: 60})
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	b, err := h.store.CreateMonitor(ctx, MonitorInput{Name: "b", Type: "fake", Interval: 60, ParentID: &a.ID})
	if err != nil {
		t.Fatalf("create b: %v", err)
	}

	// a.parent = b closes the loop a -> b -> a.
	if _, err := h.store.UpdateMonitor(ctx, a.ID, MonitorInput{
		Name: "a", Type: "fake", Interval: 60, ParentID: &b.ID,
	}); !errors.Is(err, ErrParentCycle) {
		t.Fatalf("closing the loop = %v, want ErrParentCycle", err)
	}

	// And a monitor cannot be its own parent.
	if _, err := h.store.UpdateMonitor(ctx, a.ID, MonitorInput{
		Name: "a", Type: "fake", Interval: 60, ParentID: &a.ID,
	}); !errors.Is(err, ErrParentCycle) {
		t.Fatalf("self-parent = %v, want ErrParentCycle", err)
	}
}
