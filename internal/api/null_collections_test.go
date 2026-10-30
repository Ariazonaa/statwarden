package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/report"
)

// collectionKeys are response fields whose value is a JSON array. A null there
// forces every client to special-case it, which is the whole reason
// emptySliceAsArray exists — but that helper only reaches a slice at the TOP
// level of a response, so a collection nested in a wrapper object escapes it.
var collectionKeys = map[string]bool{
	"items": true, "heartbeats": true, "groups": true, "ungrouped": true,
	"active_incidents": true, "recent_incidents": true, "maintenance": true,
	"updates": true, "monitor_ids": true, "tag_ids": true, "notification_ids": true,
	"results": true, "sparkline": true, "backup_codes": true, "warnings": true,
	"identities": true, "sessions": true, "monitors": true,
	"downtime": true, "heatmap": true, "bars": true,
}

// findNullCollections walks a decoded response and reports every collection key
// whose value is null.
func findNullCollections(path string, v any, out *[]string) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if val == nil && collectionKeys[k] {
				*out = append(*out, path+"."+k)
				continue
			}
			findNullCollections(path+"."+k, val, out)
		}
	case []any:
		for i, val := range t {
			if i > 2 { // a couple of elements is enough to see the shape
				break
			}
			findNullCollections(path+"[]", val, out)
		}
	}
}

// TestNoEndpointAnswersANullCollection sweeps a FRESH instance — the state in
// which every list is empty and therefore the state that produces nil slices.
//
// This is the systemic guard for a defect class that kept coming back one
// endpoint at a time: the stores return nil when a query matches nothing,
// json.Marshal renders that as null, and emptySliceAsArray only normalises a
// slice that IS the response. Anything wrapped — writeList's Items, or a
// map[string]any{"items": ...} — sails past it, and no single-endpoint test
// notices because MemStore and PgStore each got there by a different route.
//
// Hitting the endpoints is what makes it exhaustive: reading the producers finds
// the ones you thought to look at.
func TestNoEndpointAnswersANullCollection(t *testing.T) {
	// The REAL report.Service over report.MemStore, not a fake. The point is to
	// exercise the production producers: a scripted fake returns whatever it was
	// handed and cannot reproduce RankFlakiest returning nil for an empty input.
	h := newHarnessTweaked(t, func(o *Options) {
		o.Reporter = report.NewService(report.NewMemStore(), nil)
	})
	admin := h.setupAdmin()

	// One monitor and one page, so the per-resource endpoints resolve. Both are
	// still empty of heartbeats and incidents, which is the interesting state.
	mon := h.createMonitorFake(admin)
	pub := true
	page := h.createPage(admin, statusPageRequest{Slug: "empty", Title: "Empty", Published: &pub})

	targets := []string{
		"/api/v1/me",
		"/api/v1/me/sessions",
		"/api/v1/me/api-keys",
		"/api/v1/monitors",
		"/api/v1/monitors/" + itoa64(mon),
		"/api/v1/monitors/" + itoa64(mon) + "/heartbeats",
		// Per-monitor reporting. These were missing from the first version of this
		// sweep, which is how a null `downtime` survived it: FullReport is a struct,
		// so emptySliceAsArray never reaches the slice inside.
		"/api/v1/monitors/" + itoa64(mon) + "/uptime",
		"/api/v1/monitors/" + itoa64(mon) + "/report",
		"/api/v1/monitors/" + itoa64(mon) + "/status",
		"/api/v1/monitor-groups",
		"/api/v1/tags",
		"/api/v1/notifications",
		"/api/v1/maintenance",
		"/api/v1/status-pages",
		"/api/v1/status-pages/" + itoa64(page.ID),
		"/api/v1/status-pages/" + itoa64(page.ID) + "/incidents",
		"/api/v1/users",
		"/api/v1/audit-log",
		"/api/v1/settings",
		"/api/v1/registry/monitor-types",
		"/api/v1/registry/notification-types",
		"/api/v1/report/flakiest",
		"/api/v1/report/certificates",
	}

	for _, target := range targets {
		rec := h.requestWith(http.MethodGet, target, nil, admin)
		if rec.Code == http.StatusServiceUnavailable {
			continue // subsystem not wired in this harness
		}
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, body %s", target, rec.Code, rec.Body.String())
			continue
		}
		body := rec.Body.String()
		if body == "null\n" || body == "null" {
			t.Errorf("GET %s answered a bare null", target)
			continue
		}
		var decoded any
		if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
			t.Errorf("GET %s: undecodable body: %v", target, err)
			continue
		}
		var nulls []string
		findNullCollections("", decoded, &nulls)
		for _, n := range nulls {
			t.Errorf("GET %s answered null at %s, want an empty array — every client "+
				"then has to special-case it", target, n)
		}
	}
}
