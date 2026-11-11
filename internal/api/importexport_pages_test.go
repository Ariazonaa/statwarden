package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/importexport"
)

// A password-protected status page must not come back public from its own
// export. The bundle carried no trace of the password (rightly: a hash is not
// portable configuration), so the import created the page published and open
// to anyone — the one change to a status page nobody would make on purpose.
//
// The export now says the page was protected, and the import creates it
// UNPUBLISHED with a warning, for the operator to set a new password first. The
// rest of what makes up the page — indexing, groups, per-monitor uptime
// visibility — travels too, because it was silently lost the same way.
func TestStatusPageExportImportKeepsProtectionAndLayout(t *testing.T) {
	h := newImportHarness(t, nil)
	admin := h.setupAdmin()

	var ids []int64
	for _, name := range []string{"api", "db"} {
		rec := h.requestWith(http.MethodPost, "/api/v1/monitors", monitorRequest{Name: name, Type: "fake", Interval: 60}, admin)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create monitor = %d, body %s", rec.Code, rec.Body.String())
		}
		var v monitorView
		mustJSON(t, rec, &v)
		ids = append(ids, v.ID)
	}
	yes, no := true, false
	rec := h.requestWith(http.MethodPost, "/api/v1/status-pages", statusPageRequest{
		Slug: "internal", Title: "Internal", Published: &yes, AllowIndexing: &no,
		Password: "correct horse battery",
		Groups:   []statusPageGroupInput{{Name: "Core", SortOrder: 0}},
		Monitors: []statusPageMonitorInput{
			{MonitorID: ids[0], GroupName: "Core", SortOrder: 0},
			{MonitorID: ids[1], SortOrder: 1, ShowUptime: &no},
		},
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create page = %d, body %s", rec.Code, rec.Body.String())
	}

	rec = h.requestWith(http.MethodGet, "/api/v1/export", nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("export = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "argon2") {
		t.Fatal("the export carries the page's password hash")
	}
	var bundle importexport.Bundle
	mustJSON(t, rec, &bundle)
	if len(bundle.StatusPages) != 1 || !bundle.StatusPages[0].HasPassword {
		t.Fatalf("the export does not record the page's password protection: %+v", bundle.StatusPages)
	}

	h2 := newImportHarness(t, nil)
	admin2 := h2.setupAdmin()
	raw, _ := json.Marshal(bundle)
	rec = h2.requestRaw(http.MethodPost, "/api/v1/import", string(raw), admin2)
	if rec.Code != http.StatusOK {
		t.Fatalf("import = %d, body %s", rec.Code, rec.Body.String())
	}
	var res importResult
	mustJSON(t, rec, &res)
	if res.StatusPages != 1 {
		t.Fatalf("imported pages = %d; warnings %v", res.StatusPages, res.Warnings)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "password") {
		t.Fatalf("no warning tells the operator the page lost its password: %v", res.Warnings)
	}

	pages, err := h2.store.ListStatusPages(context.Background())
	if err != nil || len(pages) != 1 {
		t.Fatalf("pages = %v, %v", pages, err)
	}
	p := pages[0]
	if p.Published {
		t.Fatal("a password-protected page was imported published and open to anyone")
	}
	if p.AllowIndexing {
		t.Error("allow_indexing=false was lost on the way")
	}
	if len(p.Groups) != 1 || p.Groups[0].Name != "Core" {
		t.Fatalf("groups = %+v, want the Core group", p.Groups)
	}
	if len(p.Monitors) != 2 {
		t.Fatalf("page monitors = %+v, want both", p.Monitors)
	}
	for _, e := range p.Monitors {
		inCore := e.GroupID != nil && *e.GroupID == p.Groups[0].ID
		switch e.SortOrder {
		case 0:
			if !inCore || !e.ShowUptime {
				t.Errorf("first monitor = %+v, want it in Core with uptime shown", e)
			}
		default:
			if inCore || e.ShowUptime {
				t.Errorf("second monitor = %+v, want it ungrouped with uptime hidden", e)
			}
		}
	}
}

// A bundle written before the layout existed still imports, flat, with every
// monitor's uptime shown — what those pages looked like.
func TestStatusPageImportReadsAFlatBundle(t *testing.T) {
	h := newImportHarness(t, nil)
	admin := h.setupAdmin()
	rec := h.requestRaw(http.MethodPost, "/api/v1/import", `{
		"version": 1,
		"monitors": [{"name": "web", "type": "fake", "interval_seconds": 60, "active": true}],
		"status_pages": [{"slug": "public", "title": "Public", "published": true, "monitors": ["web"]}]
	}`, admin)
	var res importResult
	mustJSON(t, rec, &res)
	if res.StatusPages != 1 {
		t.Fatalf("import = %+v", res)
	}
	pages, _ := h.store.ListStatusPages(context.Background())
	if len(pages) != 1 || !pages[0].Published || len(pages[0].Monitors) != 1 || !pages[0].Monitors[0].ShowUptime {
		t.Fatalf("flat page = %+v", pages)
	}
}
