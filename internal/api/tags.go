package api

import "net/http"

type tagRequest struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

// defaultTagColor is the neutral swatch a tag gets when the client sends none.
const defaultTagColor = "#8b8fa3"

// resolveTagColor normalises a tag colour, defaulting an empty one.
//
// The colour is stored and then interpolated straight into an inline style by
// the tag chips and the monitor cards, so an unconstrained string is a stored
// CSS injection: a writer could save `red;position:fixed;inset:0;…` to cover
// another user's screen, or a `background-image:url(…)` that turns every page
// view into a request to a server they control. Pinning it to a hex colour —
// the same normalizeHexColor the status-page accent already goes through —
// removes the injection instead of trying to escape it downstream.
func resolveTagColor(raw string) (string, bool) {
	if raw == "" {
		return defaultTagColor, true
	}
	normalized := normalizeHexColor(raw)
	if normalized == "" {
		return "", false
	}
	return normalized, true
}

func (s *server) handleListTags(w http.ResponseWriter, r *http.Request) {
	tags, err := s.store.ListTags(r.Context())
	if storeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, tags)
}

func (s *server) handleCreateTag(w http.ResponseWriter, r *http.Request) {
	var req tagRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		writeProblemDetail(w, http.StatusBadRequest, "name is required")
		return
	}
	color, ok := resolveTagColor(req.Color)
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "color must be a hex colour such as #0ea5e9")
		return
	}
	t, err := s.store.CreateTag(r.Context(), req.Name, color)
	if storeError(w, err) {
		return
	}
	p := principalFrom(r)
	s.audit(r.Context(), &p.userID, "tag.create", "tag", &t.ID, clientIP(r), nil)
	writeJSON(w, http.StatusCreated, t)
}

func (s *server) handleUpdateTag(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	var req tagRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		writeProblemDetail(w, http.StatusBadRequest, "name is required")
		return
	}
	color, ok := resolveTagColor(req.Color)
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "color must be a hex colour such as #0ea5e9")
		return
	}
	t, err := s.store.UpdateTag(r.Context(), id, req.Name, color)
	if storeError(w, err) {
		return
	}
	p := principalFrom(r)
	s.audit(r.Context(), &p.userID, "tag.update", "tag", &t.ID, clientIP(r), nil)
	writeJSON(w, http.StatusOK, t)
}

func (s *server) handleDeleteTag(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.store.DeleteTag(r.Context(), id); storeError(w, err) {
		return
	}
	p := principalFrom(r)
	s.audit(r.Context(), &p.userID, "tag.delete", "tag", &id, clientIP(r), nil)
	w.WriteHeader(http.StatusNoContent)
}
