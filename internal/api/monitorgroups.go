package api

import "net/http"

// monitorGroupRequest is the create/update payload for an organisational
// folder. SortOrder positions the folder in the monitor list; ties fall back to
// id in both store implementations, so an all-zero ordering is still stable.
type monitorGroupRequest struct {
	Name      string `json:"name"`
	SortOrder int    `json:"sort_order"`
}

func (s *server) handleListMonitorGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := s.store.ListMonitorGroups(r.Context())
	if storeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, groups)
}

func (s *server) handleCreateMonitorGroup(w http.ResponseWriter, r *http.Request) {
	var req monitorGroupRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		writeProblemDetail(w, http.StatusBadRequest, "name is required")
		return
	}
	g, err := s.store.CreateMonitorGroup(r.Context(), req.Name, req.SortOrder)
	if storeError(w, err) {
		return
	}
	p := principalFrom(r)
	s.audit(r.Context(), &p.userID, "monitor_group.create", "monitor_group", &g.ID, clientIP(r), map[string]any{"name": g.Name})
	writeJSON(w, http.StatusCreated, g)
}

func (s *server) handleUpdateMonitorGroup(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	var req monitorGroupRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		writeProblemDetail(w, http.StatusBadRequest, "name is required")
		return
	}
	g, err := s.store.UpdateMonitorGroup(r.Context(), id, req.Name, req.SortOrder)
	if storeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, g)
}

// handleDeleteMonitorGroup removes a folder. The monitors filed in it survive
// with their group_id cleared — the schema says ON DELETE SET NULL and MemStore
// mirrors it — so deleting a folder is never a way to lose monitors. No engine
// call: a folder is presentation only and nothing scheduled changed.
func (s *server) handleDeleteMonitorGroup(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.store.DeleteMonitorGroup(r.Context(), id); storeError(w, err) {
		return
	}
	p := principalFrom(r)
	s.audit(r.Context(), &p.userID, "monitor_group.delete", "monitor_group", &id, clientIP(r), nil)
	w.WriteHeader(http.StatusNoContent)
}
