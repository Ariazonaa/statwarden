package api

import (
	"net/http"
	"strings"
	"testing"
)

// Names and descriptions are bounded server-side. They had no limit at all, and
// every one of them is rendered somewhere that pays per character — the SLA PDF
// measured a monitor name quadratically, and the lists, alerts and status pages
// carry them verbatim. Limits are in runes, so a non-Latin name gets the same
// allowance as an ASCII one.
func TestNameAndDescriptionLengthsAreBounded(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	atLimit := strings.Repeat("ä", maxNameRunes)
	tooLong := atLimit + "x"

	post := func(path string, body any) int {
		t.Helper()
		return h.requestWith(http.MethodPost, path, body, admin).Code
	}

	if c := post("/api/v1/monitors", monitorRequest{Name: tooLong, Type: "fake", Interval: 60}); c != http.StatusBadRequest {
		t.Errorf("monitor name of %d runes = %d, want 400", maxNameRunes+1, c)
	}
	if c := post("/api/v1/monitors", monitorRequest{
		Name: "m", Type: "fake", Interval: 60, Description: strings.Repeat("d", maxDescriptionRunes+1),
	}); c != http.StatusBadRequest {
		t.Errorf("description of %d runes = %d, want 400", maxDescriptionRunes+1, c)
	}
	if c := post("/api/v1/monitors", monitorRequest{
		Name: atLimit, Type: "fake", Interval: 60, Description: strings.Repeat("d", maxDescriptionRunes),
	}); c != http.StatusCreated {
		t.Errorf("monitor at the limits = %d, want 201", c)
	}

	if c := post("/api/v1/tags", tagRequest{Name: strings.Repeat("t", maxTagNameRunes+1)}); c != http.StatusBadRequest {
		t.Errorf("tag name of %d runes = %d, want 400", maxTagNameRunes+1, c)
	}
	rec := h.requestWith(http.MethodPost, "/api/v1/tags", tagRequest{Name: strings.Repeat("t", maxTagNameRunes)}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("tag at the limit = %d, want 201", rec.Code)
	}
	var tag Tag
	mustJSON(t, rec, &tag)
	if c := h.requestWith(http.MethodPut, "/api/v1/tags/"+itoa64(tag.ID),
		tagRequest{Name: strings.Repeat("t", maxTagNameRunes+1)}, admin).Code; c != http.StatusBadRequest {
		t.Errorf("tag rename past the limit = %d, want 400", c)
	}

	if c := post("/api/v1/monitor-groups", monitorGroupRequest{Name: tooLong}); c != http.StatusBadRequest {
		t.Errorf("group name of %d runes = %d, want 400", maxNameRunes+1, c)
	}
	rec = h.requestWith(http.MethodPost, "/api/v1/monitor-groups", monitorGroupRequest{Name: atLimit}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("group at the limit = %d, want 201", rec.Code)
	}
	var group MonitorGroup
	mustJSON(t, rec, &group)
	if c := h.requestWith(http.MethodPut, "/api/v1/monitor-groups/"+itoa64(group.ID),
		monitorGroupRequest{Name: tooLong}, admin).Code; c != http.StatusBadRequest {
		t.Errorf("group rename past the limit = %d, want 400", c)
	}
}
