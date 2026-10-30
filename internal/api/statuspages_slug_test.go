package api

import (
	"net/http"
	"strings"
	"testing"
)

// TestStatusPageSlugIsValidated pins the slug format on both writers.
//
// The emptiness check used to run on the RAW value while toPage trimmed
// afterwards, so "   " passed and was stored as an empty slug. There was also no
// length or charset limit at all, while the slug reaches three parsers that each
// read it differently: the URL path /status/{slug}, the Path attribute of the
// page unlock cookie, and the WebSocket topic status-page:{slug}.
func TestStatusPageSlugIsValidated(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	published := true

	bad := []struct {
		name string
		slug string
	}{
		{"empty", ""},
		{"whitespace only", "   "},
		{"uppercase", "Acme"},
		{"path separator", "acme/../other"},
		{"space", "acme corp"},
		{"over 64 characters", strings.Repeat("a", 65)},
		{"dot segment", ".."},
		{"percent escape", "acme%2f"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			req := statusPageRequest{Slug: tc.slug, Title: "T", Published: &published}
			rec := h.requestWith(http.MethodPost, "/api/v1/status-pages", req, admin)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("create with slug %q = %d, want 400; body %s", tc.slug, rec.Code, rec.Body.String())
			}
		})
	}

	// A surrounding-whitespace slug is accepted and stored trimmed — the check
	// and the storage now agree on which value they are talking about.
	rec := h.requestWith(http.MethodPost, "/api/v1/status-pages",
		statusPageRequest{Slug: "  padded  ", Title: "Padded", Published: &published}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create with a padded slug = %d, want 201; body %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID   int64  `json:"id"`
		Slug string `json:"slug"`
	}
	mustJSON(t, rec, &created)
	if created.Slug != "padded" {
		t.Fatalf("stored slug = %q, want %q", created.Slug, "padded")
	}

	// The same rule holds on update, or a page could be edited into a shape the
	// create path refuses.
	for _, tc := range bad {
		req := statusPageRequest{Slug: tc.slug, Title: "T", Published: &published}
		rec := h.requestWith(http.MethodPut, "/api/v1/status-pages/"+itoa64(created.ID), req, admin)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("update with slug %q = %d, want 400; body %s", tc.slug, rec.Code, rec.Body.String())
		}
	}
}
