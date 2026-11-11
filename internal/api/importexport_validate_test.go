package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/importexport"
)

// The import wrote tags and status pages straight to the store, past the rules
// the API enforces on the same values: a tag colour is interpolated into an
// inline style (a stored CSS injection), and a slug becomes a public URL path.
func TestImportAppliesTheWriteValidators(t *testing.T) {
	h := newImportHarness(t, nil)
	admin := h.setupAdmin()
	rec := h.requestWith(http.MethodPost, "/api/v1/import", importexport.Bundle{
		Version: importexport.BundleVersion,
		Tags: []importexport.TagSpec{
			{Name: "hostile", Color: "red;position:fixed;inset:0"},
			{Name: strings.Repeat("t", maxTagNameRunes+1), Color: "#123456"},
		},
		StatusPages: []importexport.StatusPageSpec{
			{Slug: "../admin", Title: "Bad slug"},
			{Slug: "no-title"},
			{Slug: "dup-groups", Title: "Dup", Groups: []importexport.StatusPageGroupSpec{{Name: "A"}, {Name: "A"}}},
			{Slug: "fine", Title: "Fine"},
		},
	}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("import = %d, body %s", rec.Code, rec.Body.String())
	}
	var res importResult
	mustJSON(t, rec, &res)

	tags, _ := h.store.ListTags(context.Background())
	if len(tags) != 1 {
		t.Fatalf("tags = %+v, want only the hostile-coloured one, recoloured", tags)
	}
	if tags[0].Color != defaultTagColor {
		t.Errorf("an invalid tag colour was stored as %q", tags[0].Color)
	}
	pages, _ := h.store.ListStatusPages(context.Background())
	if len(pages) != 1 || pages[0].Slug != "fine" {
		t.Fatalf("pages = %+v, want only the valid one", pages)
	}
	if res.StatusPages != 1 || len(res.Warnings) < 5 {
		t.Fatalf("result = %+v, want one page and a warning per refused value", res)
	}
}

// Importing the same bundle twice must not double every monitor. Tags,
// channels and pages were already matched by name or slug; monitors were not,
// although the handler promises that existing entities are left in place.
func TestReimportDoesNotDuplicateMonitors(t *testing.T) {
	h := newImportHarness(t, nil)
	admin := h.setupAdmin()
	bundle := importexport.Bundle{
		Version: importexport.BundleVersion,
		Monitors: []importexport.MonitorSpec{
			{Name: "web", Type: "fake", IntervalSeconds: 60, Active: true},
			{Name: "child", Type: "fake", IntervalSeconds: 60, Active: true, Parent: "web"},
		},
	}
	for i := 0; i < 2; i++ {
		rec := h.requestWith(http.MethodPost, "/api/v1/import", bundle, admin)
		if rec.Code != http.StatusOK {
			t.Fatalf("import %d = %d", i, rec.Code)
		}
		var res importResult
		mustJSON(t, rec, &res)
		if i == 1 && (res.Monitors != 0 || !strings.Contains(strings.Join(res.Warnings, "\n"), "already exists")) {
			t.Fatalf("second import = %+v, want nothing created and the skips reported", res)
		}
	}
	mons, _, _ := h.store.ListMonitors(context.Background(), MonitorFilter{}, Page{Limit: 10})
	if len(mons) != 2 {
		t.Fatalf("monitors after two imports = %d, want 2", len(mons))
	}
}
