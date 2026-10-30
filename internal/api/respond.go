package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

// writeProblemDetail emits an RFC 7807 problem+json response with a human
// detail string in addition to the title. writeProblem (in handlers.go) stays
// the minimal title-only form for the router's NotFound/MethodNotAllowed hooks.
func writeProblemDetail(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":   "about:blank",
		"title":  http.StatusText(status),
		"status": status,
		"detail": detail,
	})
}

// storeError maps a store sentinel error to a problem+json response and reports
// whether it handled err. Handlers call: if storeError(w, err) { return }.
func storeError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrNotFound):
		writeProblemDetail(w, http.StatusNotFound, "resource not found")
		return true
	case errors.Is(err, ErrConflict):
		writeProblemDetail(w, http.StatusConflict, "resource already exists")
		return true
	case errors.Is(err, ErrParentCycle):
		// A 409, not a 500: the request was well-formed and lost a race with
		// another parent change. Retrying after re-reading is the right move,
		// which is what a conflict says.
		writeProblemDetail(w, http.StatusConflict,
			"parent_id would create a dependency cycle")
		return true
	default:
		writeProblemDetail(w, http.StatusInternalServerError, "internal error")
		return true
	}
}

// validationOrStoreError splits a mixed error: a validationError is the
// caller's fault and becomes a 400 naming the offending field, anything else is
// the store's and goes through storeError. Handlers that validate against
// stored state (e.g. the monitor parent-cycle walk, which reads monitors) need
// this — reporting a database outage as "invalid parent_id" would send the
// operator hunting a data problem that isn't there.
func validationOrStoreError(w http.ResponseWriter, err error) bool {
	var ve validationError
	if errors.As(err, &ve) {
		writeProblemDetail(w, http.StatusBadRequest, ve.Error())
		return true
	}
	return storeError(w, err)
}

// decodeJSON reads a JSON request body into v, rejecting unknown fields and
// oversized payloads. It returns false (and has already written a 400) when the
// body is unusable.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	const maxBody = 1 << 20 // 1 MiB
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeProblemDetail(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	// One JSON value, and nothing after it.
	//
	// Decode stops at the end of the first value and never looks further, so a
	// body of `{"role":"viewer"} {"role":"admin"}` was accepted with the SECOND
	// object silently discarded — or the first, depending on which end a
	// different parser reads from. That divergence is the whole point of the
	// strictness this function already applies to unknown fields: two readers
	// of the same bytes must not disagree about what was requested.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeProblemDetail(w, http.StatusBadRequest,
			"invalid JSON body: unexpected data after the JSON value")
		return false
	}
	return true
}

// idParam parses a positive int64 path parameter named key.
func idParam(r *http.Request, key string) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, key), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// parsePage reads limit/offset query parameters with sane defaults and caps.
func parsePage(r *http.Request) Page {
	const (
		defaultLimit = 50
		maxLimit     = 200
	)
	limit := defaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	offset := 0
	if raw := r.URL.Query().Get("offset"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			offset = n
		}
	}
	return Page{Limit: limit, Offset: offset}
}

// listEnvelope is the paginated list response shape shared by all list
// endpoints.
type listEnvelope struct {
	Items  any `json:"items"`
	Total  int `json:"total"`
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}

func writeList(w http.ResponseWriter, items any, total int, p Page) {
	writeJSON(w, http.StatusOK, listEnvelope{Items: items, Total: total, Limit: p.Limit, Offset: p.Offset})
}
