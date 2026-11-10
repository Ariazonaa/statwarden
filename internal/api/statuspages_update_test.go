package api

import (
	"net/http"
	"testing"
)

// TestUpdateStatusPageKeepsOmittedVisibilityFlags pins that a PUT which does not
// mention published or allow_indexing leaves them as stored. Both used to be
// defaulted to true on every write, so saving a draft's title from a client
// that only sent the fields it changed published the draft to the internet and
// opened it to search engines.
func TestUpdateStatusPageKeepsOmittedVisibilityFlags(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	no := false
	page := h.createPage(admin, statusPageRequest{
		Slug: "draft", Title: "Draft", Published: &no, AllowIndexing: &no,
	})

	rec := h.requestWith(http.MethodPut, "/api/v1/status-pages/"+itoa64(page.ID),
		statusPageRequest{Slug: "draft", Title: "Draft (renamed)"}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, body %s", rec.Code, rec.Body.String())
	}
	var got StatusPage
	mustJSON(t, rec, &got)
	if got.Published {
		t.Error("PUT without published published the draft")
	}
	if got.AllowIndexing {
		t.Error("PUT without allow_indexing opened the page to indexing")
	}
	if got.Title != "Draft (renamed)" {
		t.Errorf("title = %q, the rest of the PUT was not applied", got.Title)
	}

	// An explicit value still wins.
	yes := true
	rec = h.requestWith(http.MethodPut, "/api/v1/status-pages/"+itoa64(page.ID),
		statusPageRequest{Slug: "draft", Title: "Draft", Published: &yes}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT published=true = %d, body %s", rec.Code, rec.Body.String())
	}
	mustJSON(t, rec, &got)
	if !got.Published || got.AllowIndexing {
		t.Errorf("published=%v allow_indexing=%v, want true/false", got.Published, got.AllowIndexing)
	}

	// Creating a page without the flags keeps the historical default.
	created := h.createPage(admin, statusPageRequest{Slug: "fresh", Title: "Fresh"})
	if !created.Published || !created.AllowIndexing {
		t.Errorf("create defaults = published %v, indexing %v; want true/true", created.Published, created.AllowIndexing)
	}
}
