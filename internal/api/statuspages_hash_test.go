package api

import (
	"errors"
	"net/http"
	"testing"
)

// TestStatusPagePasswordHashFailureIsNotSwallowed pins that a page whose
// password could not be hashed is NOT saved. The error used to be dropped and
// the page stored without protection, so an operator who asked for a
// password-protected page got a public one and a 201.
func TestStatusPagePasswordHashFailureIsNotSwallowed(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	pub := true
	page := h.createPage(admin, statusPageRequest{Slug: "kept", Title: "Kept", Published: &pub, Password: "hunter2"})

	orig := hashPagePassword
	hashPagePassword = func(string) (string, error) { return "", errors.New("entropy source gone") }
	t.Cleanup(func() { hashPagePassword = orig })

	rec := h.requestWith(http.MethodPost, "/api/v1/status-pages",
		statusPageRequest{Slug: "fresh", Title: "Fresh", Published: &pub, Password: "hunter2"}, admin)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("create with failing hash = %d, want 500; body %s", rec.Code, rec.Body.String())
	}
	if _, err := h.store.GetStatusPageBySlug(t.Context(), "fresh"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a page was stored despite the failed hash (err %v)", err)
	}

	// An update that rotates the password must not leave the OLD one in place
	// silently either — the caller asked for a change and has to be told it
	// did not happen.
	rec = h.requestWith(http.MethodPut, "/api/v1/status-pages/"+itoa64(page.ID),
		statusPageRequest{Slug: "kept", Title: "Kept", Published: &pub, Password: "rotated"}, admin)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("update with failing hash = %d, want 500; body %s", rec.Code, rec.Body.String())
	}
}
