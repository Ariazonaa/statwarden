package api

import "net/http"

// layoutGroupOrderInput is one group's slot in a reorder payload.
type layoutGroupOrderInput struct {
	GroupID    int64   `json:"group_id"`
	MonitorIDs []int64 `json:"monitor_ids"`
}

// layoutReorderRequest is a complete re-statement of a page's display order.
// Partial payloads are rejected: the caller must name every group and every
// monitor currently on the page, each exactly once.
type layoutReorderRequest struct {
	Ungrouped []int64                 `json:"ungrouped"`
	Groups    []layoutGroupOrderInput `json:"groups"`
}

func (r layoutReorderRequest) toOrder() StatusPageLayoutOrder {
	o := StatusPageLayoutOrder{Ungrouped: r.Ungrouped}
	for _, g := range r.Groups {
		o.Groups = append(o.Groups, StatusPageGroupOrder(g))
	}
	return o
}

// handleReorderStatusPageLayout rewrites group and monitor positions from a
// complete ordering. The validation below exists for readable error messages
// only; the store repeats every check inside its transaction, which is what
// actually guarantees an interleaved page save cannot produce a half-applied
// order. Ids that belong to another page are rejected here as stale rather than
// silently written, and the store's per-row `WHERE status_page_id = $1` is the
// boundary that makes that true.
func (s *server) handleReorderStatusPageLayout(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	var req layoutReorderRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	page, err := s.store.GetStatusPage(r.Context(), id)
	if storeError(w, err) {
		return
	}
	const staleDetail = "layout is out of date; reload the status page and retry"

	// Group ids: no duplicates, and exactly the set currently on the page.
	wantGroups := make(map[int64]bool, len(page.Groups))
	for _, g := range page.Groups {
		wantGroups[g.ID] = true
	}
	gotGroups := make(map[int64]bool, len(req.Groups))
	for _, g := range req.Groups {
		if gotGroups[g.GroupID] {
			writeProblemDetail(w, http.StatusBadRequest, "duplicate group id in layout")
			return
		}
		gotGroups[g.GroupID] = true
	}
	if len(gotGroups) != len(wantGroups) {
		writeProblemDetail(w, http.StatusConflict, staleDetail)
		return
	}
	for gid := range gotGroups {
		if !wantGroups[gid] {
			writeProblemDetail(w, http.StatusConflict, staleDetail)
			return
		}
	}

	// Monitor ids: no duplicates across ungrouped plus every group, and exactly
	// the set currently placed on the page.
	wantMonitors := make(map[int64]bool, len(page.Monitors))
	for _, m := range page.Monitors {
		wantMonitors[m.MonitorID] = true
	}
	gotMonitors := map[int64]bool{}
	distinct := func(ids []int64) bool {
		for _, mid := range ids {
			if gotMonitors[mid] {
				return false
			}
			gotMonitors[mid] = true
		}
		return true
	}
	if !distinct(req.Ungrouped) {
		writeProblemDetail(w, http.StatusBadRequest, "duplicate monitor id in layout")
		return
	}
	for _, g := range req.Groups {
		if !distinct(g.MonitorIDs) {
			writeProblemDetail(w, http.StatusBadRequest, "duplicate monitor id in layout")
			return
		}
	}
	if len(gotMonitors) != len(wantMonitors) {
		writeProblemDetail(w, http.StatusConflict, staleDetail)
		return
	}
	for mid := range gotMonitors {
		if !wantMonitors[mid] {
			writeProblemDetail(w, http.StatusConflict, staleDetail)
			return
		}
	}

	updated, err := s.store.ReorderStatusPageLayout(r.Context(), id, req.toOrder())
	if storeError(w, err) {
		return
	}
	pr := principalFrom(r)
	s.audit(r.Context(), &pr.userID, "status_page.reorder", "status_page", &id, clientIP(r), nil)
	// Public viewers hold a status-page subscription and re-fetch on any
	// envelope, so a reorder must be announced or they keep the stale order
	// until they reload.
	if s.wsPub != nil {
		s.wsPub.PublishStatusPage(updated.Slug, "layout", map[string]any{
			"at": s.now().UnixMilli(),
		})
	}
	writeJSON(w, http.StatusOK, updated)
}
