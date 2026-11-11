package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/importexport"
)

// An export used to drop every monitor's tags, alert channels, parent and
// folder, so a restored instance came back with monitors that page nobody,
// alert through outages their parent explains, and sit unfiled — and the import
// had nothing to warn about, because the bundle never mentioned them.
func TestExportImportKeepsMonitorLinks(t *testing.T) {
	h := newImportHarness(t, nil)
	admin := h.setupAdmin()
	ctx := context.Background()

	tag, err := h.store.CreateTag(ctx, "prod", "#0ea5e9")
	if err != nil {
		t.Fatal(err)
	}
	rec := h.requestWith(http.MethodPost, "/api/v1/notifications", notificationRequest{
		Name: "ops", Type: "fake", Config: json.RawMessage(`{}`),
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create channel = %d, body %s", rec.Code, rec.Body.String())
	}
	var channel Notification
	mustJSON(t, rec, &channel)
	group, err := h.store.CreateMonitorGroup(ctx, "Backend", 3)
	if err != nil {
		t.Fatal(err)
	}

	create := func(req monitorRequest) int64 {
		t.Helper()
		rec := h.requestWith(http.MethodPost, "/api/v1/monitors", req, admin)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s = %d, body %s", req.Name, rec.Code, rec.Body.String())
		}
		var v monitorView
		mustJSON(t, rec, &v)
		return v.ID
	}
	// The child is created FIRST in id order on purpose: the export lists it
	// before its parent, and the import must still wire it up.
	parentID := create(monitorRequest{Name: "gateway", Type: "fake", Interval: 60, NotificationIDs: []int64{}})
	childID := create(monitorRequest{
		Name: "api", Type: "fake", Interval: 60, ParentID: &parentID, GroupID: &group.ID,
		TagIDs: []int64{tag.ID}, NotificationIDs: []int64{channel.ID},
	})
	_ = childID

	rec = h.requestWith(http.MethodGet, "/api/v1/export", nil, admin)
	var bundle importexport.Bundle
	mustJSON(t, rec, &bundle)
	// Put the child ahead of its parent, as an id order could.
	if len(bundle.Monitors) == 2 && bundle.Monitors[0].Name == "gateway" {
		bundle.Monitors[0], bundle.Monitors[1] = bundle.Monitors[1], bundle.Monitors[0]
	}

	h2 := newImportHarness(t, nil)
	admin2 := h2.setupAdmin()
	rec = h2.requestWith(http.MethodPost, "/api/v1/import", bundle, admin2)
	var res importResult
	mustJSON(t, rec, &res)
	if res.Monitors != 2 || len(res.Warnings) != 0 {
		t.Fatalf("import = %+v, want both monitors and no warnings", res)
	}

	mons, _, _ := h2.store.ListMonitors(ctx, MonitorFilter{}, Page{Limit: 10})
	byName := map[string]int64{}
	for _, m := range mons {
		byName[m.Name] = m.ID
	}
	child, err := h2.store.GetMonitor(ctx, byName["api"])
	if err != nil {
		t.Fatal(err)
	}
	if child.ParentID == nil || *child.ParentID != byName["gateway"] {
		t.Errorf("child parent = %v, want the imported gateway (%d)", child.ParentID, byName["gateway"])
	}
	groups, _ := h2.store.ListMonitorGroups(ctx)
	if len(groups) != 1 || groups[0].Name != "Backend" || groups[0].SortOrder != 3 {
		t.Fatalf("folders = %+v, want Backend at 3", groups)
	}
	if child.GroupID == nil || *child.GroupID != groups[0].ID {
		t.Errorf("child folder = %v, want Backend", child.GroupID)
	}
	links, err := h2.store.MonitorLinks(ctx, []int64{child.ID})
	if err != nil {
		t.Fatal(err)
	}
	l := links[child.ID]
	if len(l.TagIDs) != 1 || len(l.NotificationIDs) != 1 {
		t.Fatalf("child links = %+v, want one tag and one channel", l)
	}
}
