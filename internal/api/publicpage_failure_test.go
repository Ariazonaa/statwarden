package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/report"
)

// errStoreDown stands in for a database that stopped answering.
var errStoreDown = errors.New("pool exhausted")

// failingStore is a MemStore that starts refusing a chosen read on demand.
//
// The whole point is to fail AFTER the page row has been fetched, which is the
// realistic shape: a pool that saturates under a traffic spike, or a
// statement_timeout that starts biting once the working set grows.
type failingStore struct {
	*MemStore
	failMonitors atomic.Bool
	failIncident atomic.Bool
	failMaint    atomic.Bool
}

func (f *failingStore) GetMonitor(ctx context.Context, id int64) (domain.Monitor, error) {
	if f.failMonitors.Load() {
		return domain.Monitor{}, errStoreDown
	}
	return f.MemStore.GetMonitor(ctx, id)
}

func (f *failingStore) ListIncidents(ctx context.Context, pageID int64) ([]Incident, error) {
	if f.failIncident.Load() {
		return nil, errStoreDown
	}
	return f.MemStore.ListIncidents(ctx, pageID)
}

func (f *failingStore) ListMaintenance(ctx context.Context) ([]MaintenanceWindow, error) {
	if f.failMaint.Load() {
		return nil, errStoreDown
	}
	return f.MemStore.ListMaintenance(ctx)
}

// A status page must never answer "everything is fine" on the strength of reads
// that failed.
//
// Every counter the banner is derived from is a COUNT, so a read that returns
// nothing leaves them all at zero — and zero monitors down out of zero monitors
// is, by definition, "All systems operational". Three separate swallowed errors
// fed that: `incidents, _ :=`, buildMonitorView treating any GetMonitor failure
// as an ordinary soft delete, and pageMaintenance discarding its own. The result
// was HTTP 200, a green banner and a green badge, produced BECAUSE the database
// was unreachable.
func TestAPublicPageWithABrokenStoreDoesNotClaimEverythingIsFine(t *testing.T) {
	store := &failingStore{MemStore: NewMemStore()}
	h := newHarnessTweaked(t, func(o *Options) { o.Store = store })
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
	rec = h.requestWith(http.MethodPost, "/api/v1/status-pages", map[string]any{
		"slug": "public", "title": "Public", "theme": "auto", "published": true,
		"monitors": []map[string]any{{"monitor_id": mon.ID, "show_uptime": true}},
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create page = %d, body %s", rec.Code, rec.Body.String())
	}

	// Sanity: while the store answers, the page renders and reports the monitor.
	rec = h.request(http.MethodGet, "/api/v1/public/status/public", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthy page = %d, body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"api"`) {
		t.Fatalf("the healthy page does not list the monitor, so this test proves nothing:\n%s",
			rec.Body.String())
	}

	for _, c := range []struct {
		name string
		flag *atomic.Bool
	}{
		{"the monitor read fails", &store.failMonitors},
		{"the incident read fails", &store.failIncident},
		{"the maintenance read fails", &store.failMaint},
	} {
		c.flag.Store(true)
		rec := h.request(http.MethodGet, "/api/v1/public/status/public", nil)
		if rec.Code == http.StatusOK {
			t.Errorf("%s: page answered 200 with body %s", c.name, rec.Body.String())
		}
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: page = %d, want 503", c.name, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "operational") {
			t.Errorf("%s: the response still claims the page is operational: %s",
				c.name, rec.Body.String())
		}
		c.flag.Store(false)
	}
}

// The badge is pasted into a README and glanced at. Painting it green because
// the database is unreachable is worse than painting nothing — and the badge
// reads only OverallStatus, which is exactly the value the swallowed errors
// falsified, so a badge-based canary stayed green through the whole failure.
func TestTheBadgeDoesNotPaintGreenWhenTheStoreIsBroken(t *testing.T) {
	store := &failingStore{MemStore: NewMemStore()}
	h := newHarnessTweaked(t, func(o *Options) { o.Store = store })
	admin := h.setupAdmin()

	rec := h.requestWith(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "api", "type": "fake", "interval_seconds": 60, "config": map[string]any{},
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create monitor = %d", rec.Code)
	}
	var mon struct {
		ID int64 `json:"id"`
	}
	mustJSON(t, rec, &mon)
	rec = h.requestWith(http.MethodPost, "/api/v1/status-pages", map[string]any{
		"slug": "public", "title": "Public", "theme": "auto", "published": true,
		"monitors": []map[string]any{{"monitor_id": mon.ID, "show_uptime": true}},
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create page = %d", rec.Code)
	}

	green := badgeFill(t, h, "the healthy badge")
	store.failMonitors.Store(true)
	broken := badgeFill(t, h, "the badge with a broken store")
	if broken == green {
		t.Errorf("the badge is unchanged (%s) while the store is failing; it is reporting health it did not measure", broken)
	}
}

// badgeFill fetches the page badge and returns the status colour it painted.
func badgeFill(t *testing.T, h *harness, what string) string {
	t.Helper()
	rec := h.request(http.MethodGet, "/api/v1/public/status/public/badge.svg", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: badge = %d, body %s", what, rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// Matched against the known palette rather than by position: the SVG also
	// carries shadow and text fills, and picking one of those would make the
	// assertion pass for the wrong reason.
	palette := map[string]string{
		"#4c1":    "green (up)",
		"#e05d44": "red (down)",
		"#dfb317": "yellow (pending)",
		"#007ec6": "blue (maintenance)",
		"#9f9f9f": "gray (unknown)",
	}
	found := ""
	for hex, name := range palette {
		if strings.Contains(body, `fill="`+hex+`"`) {
			if found != "" {
				t.Fatalf("%s: the badge paints two status colours at once (%s and %s)", what, found, name)
			}
			found = name
		}
	}
	if found == "" {
		t.Fatalf("%s: no known status colour in the badge SVG:\n%s", what, body)
	}
	return found
}

// show_uptime=false must keep the FIGURES off the wire, not merely off screen.
//
// Both the headline uptime and the ninety daily bars were computed regardless
// and the flag was only echoed for the client to act on — so an anonymous curl
// of the JSON got exactly the numbers the operator had marked as not for
// publication, and the page drew the bar strip unconditionally with a per-day
// percentage in every tooltip. The archive endpoint has always honoured the
// flag; this was the half that leaked.
func TestHidingUptimeKeepsItOutOfThePublicPayload(t *testing.T) {
	h := newHarnessTweaked(t, func(o *Options) {
		o.Reporter = &fakeReporter{uptime: report.UptimeResult{Up: 99, Total: 100, Uptime: 0.99}}
	})
	admin := h.setupAdmin()

	ids := map[string]int64{}
	for _, name := range []string{"shown", "hidden"} {
		rec := h.requestWith(http.MethodPost, "/api/v1/monitors", map[string]any{
			"name": name, "type": "fake", "interval_seconds": 60, "config": map[string]any{},
		}, admin)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s = %d", name, rec.Code)
		}
		var mon struct {
			ID int64 `json:"id"`
		}
		mustJSON(t, rec, &mon)
		ids[name] = mon.ID
	}

	rec := h.requestWith(http.MethodPost, "/api/v1/status-pages", map[string]any{
		"slug": "public", "title": "Public", "theme": "auto", "published": true,
		"monitors": []map[string]any{
			{"monitor_id": ids["shown"], "show_uptime": true},
			{"monitor_id": ids["hidden"], "show_uptime": false},
		},
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create page = %d, body %s", rec.Code, rec.Body.String())
	}

	rec = h.request(http.MethodGet, "/api/v1/public/status/public", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("public page = %d", rec.Code)
	}
	var view PublicPageView
	mustJSON(t, rec, &view)

	byID := map[int64]PublicMonitorView{}
	for _, m := range view.Ungrouped {
		byID[m.ID] = m
	}
	shown, hidden := byID[ids["shown"]], byID[ids["hidden"]]

	// Positive control: the opted-in monitor really does publish figures, so a
	// change that simply stopped computing them everywhere would fail here.
	if shown.Uptime < 0 || len(shown.Heatmap) == 0 {
		t.Fatalf("the opted-in monitor published no figures (%v, %d bars); this test would "+
			"pass for the wrong reason", shown.Uptime, len(shown.Heatmap))
	}

	if hidden.Uptime >= 0 {
		t.Errorf("a monitor with show_uptime=false published uptime %v", hidden.Uptime)
	}
	if len(hidden.Heatmap) != 0 {
		t.Errorf("a monitor with show_uptime=false published %d heatmap bars, each carrying "+
			"a per-day percentage", len(hidden.Heatmap))
	}
	if hidden.Heatmap == nil {
		t.Error("the heatmap is nil rather than empty; it marshals as null and the page maps over it")
	}
}

// Internal user ids must not reach anonymous visitors.
//
// publicCopy stripped only the draft retrospective, so every incident and every
// timeline entry carried created_by — enough to count staff accounts and
// correlate which one handles which outage. The client's own PublicIncident type
// does not even declare the field.
func TestThePublicPageDoesNotPublishInternalUserIDs(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	rec := h.requestWith(http.MethodPost, "/api/v1/status-pages", map[string]any{
		"slug": "public", "title": "Public", "published": true,
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create page = %d", rec.Code)
	}
	var page StatusPage
	mustJSON(t, rec, &page)

	rec = h.requestWith(http.MethodPost, "/api/v1/status-pages/"+id64(page.ID)+"/incidents",
		map[string]any{"title": "Outage", "body": "Looking into it.", "severity": "major"}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create incident = %d", rec.Code)
	}

	rec = h.request(http.MethodGet, "/api/v1/public/status/public", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("public page = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `"created_by"`) {
		t.Errorf("the public payload carries created_by:\n%s", rec.Body.String())
	}

	// The authenticated read still has it — that is who did what, for operators.
	rec = h.requestWith(http.MethodGet, "/api/v1/status-pages/"+id64(page.ID)+"/incidents", nil, admin)
	if !strings.Contains(rec.Body.String(), `"created_by"`) {
		t.Error("the authenticated incident list lost created_by; the strip is too wide")
	}
}

// A window with no monitors named is fleet-wide, and the public page must
// announce it.
//
// Nothing covered the DERIVATION of applies_to_all, so setting it to a constant
// false left the whole tree green while every fleet-wide maintenance window
// silently covered nothing: suppression stopped working for the commonest window
// shape and the page stopped announcing planned downtime. The fixtures in
// internal/maintenance now hard-code the flag, so they cannot catch it either.
func TestAWindowWithNoMonitorsIsFleetWideAndIsAnnounced(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	ctx := context.Background()

	rec := h.requestWith(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "api", "type": "fake", "interval_seconds": 60, "config": map[string]any{},
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create monitor = %d", rec.Code)
	}
	var mon struct {
		ID int64 `json:"id"`
	}
	mustJSON(t, rec, &mon)
	rec = h.requestWith(http.MethodPost, "/api/v1/status-pages", map[string]any{
		"slug": "public", "title": "Public", "theme": "auto", "published": true,
		"monitors": []map[string]any{{"monitor_id": mon.ID, "show_uptime": true}},
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create page = %d", rec.Code)
	}

	from := time.Now().UTC().Add(-time.Hour)
	to := from.Add(4 * time.Hour)
	rec = h.requestWith(http.MethodPost, "/api/v1/maintenance", map[string]any{
		"title": "everything", "strategy": "single", "timezone": "UTC",
		"starts_at": from.Format(time.RFC3339), "ends_at": to.Format(time.RFC3339),
		// No monitor_ids at all: the historical spelling of "every monitor".
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create window = %d, body %s", rec.Code, rec.Body.String())
	}
	var win MaintenanceWindow
	mustJSON(t, rec, &win)
	if !win.AppliesToAll {
		t.Fatal("a window created with no monitors is not fleet-wide")
	}

	// The evaluator agrees.
	if h.maint != nil {
		if err := h.maint.Refresh(ctx); err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		if !h.maint.InMaintenance(mon.ID, time.Now().UTC()) {
			t.Error("a fleet-wide window does not suppress a monitor's alerts")
		}
	}

	// And the public page announces it — this is the `touches` half, which
	// reverting to the old len(MonitorIDs)==0 inference also leaves green.
	rec = h.request(http.MethodGet, "/api/v1/public/status/public", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("public page = %d", rec.Code)
	}
	var view PublicPageView
	mustJSON(t, rec, &view)
	if len(view.Maintenance) != 1 {
		t.Fatalf("the page announces %d windows, want the fleet-wide one: %+v",
			len(view.Maintenance), view.Maintenance)
	}
}

// A group with nothing in it is not rendered.
//
// Reverting the skip leaves the tree green, and an empty heading on a public
// page cannot be removed from the editor once its only monitor is binned.
func TestAnEmptyGroupIsNotRendered(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	rec := h.requestWith(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "api", "type": "fake", "interval_seconds": 60, "config": map[string]any{},
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create monitor = %d", rec.Code)
	}
	var mon struct {
		ID int64 `json:"id"`
	}
	mustJSON(t, rec, &mon)

	rec = h.requestWith(http.MethodPost, "/api/v1/status-pages", map[string]any{
		"slug": "public", "title": "Public", "theme": "auto", "published": true,
		"groups": []map[string]any{
			{"name": "Core", "sort_order": 0},
			{"name": "Unstaffed", "sort_order": 1},
		},
		"monitors": []map[string]any{
			{"monitor_id": mon.ID, "group_name": "Core", "show_uptime": true},
		},
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create page = %d, body %s", rec.Code, rec.Body.String())
	}

	rec = h.request(http.MethodGet, "/api/v1/public/status/public", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("public page = %d", rec.Code)
	}
	var view PublicPageView
	mustJSON(t, rec, &view)
	if len(view.Groups) != 1 || view.Groups[0].Name != "Core" {
		t.Fatalf("groups = %+v, want only the one with a monitor in it", view.Groups)
	}
	// The one that IS rendered still carries an array, never null.
	if view.Groups[0].Monitors == nil {
		t.Error("a rendered group has a nil monitor list; it marshals as null")
	}
}
