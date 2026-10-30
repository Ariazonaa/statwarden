package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"runtime"
	"time"

	"github.com/Ariazonaa/statwarden/internal/db"
	"github.com/Ariazonaa/statwarden/internal/version"
)

const readyzTimeout = 2 * time.Second

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(emptySliceAsArray(v))
}

// emptySliceAsArray turns a nil slice into an empty one so a collection
// endpoint answers `[]` rather than `null`. encoding/json marshals a nil slice
// as null, and the stores return nil when a query matches nothing, so every
// list endpoint used to answer null on a fresh install — which any client
// iterating the response has to special-case. Scalars and structs pass through
// untouched.
func emptySliceAsArray(v any) any {
	if v == nil {
		return v
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Slice && rv.IsNil() {
		return reflect.MakeSlice(rv.Type(), 0, 0).Interface()
	}
	return v
}

// writeProblem emits a minimal RFC 7807 problem+json response.
func writeProblem(w http.ResponseWriter, status int, title string) {
	w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":   "about:blank",
		"title":  title,
		"status": status,
	})
}

// handleHealthz is the liveness probe: the process is up and serving.
func (s *server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz is the readiness probe: the database must be reachable.
func (s *server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "unavailable",
			"reason": "database not configured",
		})
		return
	}
	if err := db.Ping(r.Context(), s.pool, readyzTimeout); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "unavailable",
			"reason": "database unreachable",
		})
		return
	}
	// A reachable database is not sufficient. If the engine loaded monitors and
	// could schedule none of them, this process is checking nothing — the exact
	// state a wrong or rotated ENCRYPTION_KEY produces, since every config then
	// fails to decrypt and is rejected by its checker. Reporting "ready" there
	// means the outage that matters most is the one nobody is told about.
	//
	// Only the total failure is fatal. A single monitor with a broken config is
	// a content problem, not a readiness problem, and must not take the instance
	// out of service.
	if unscheduled, ok := schedulingReady(s.engine); !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status":      "unavailable",
			"reason":      "no monitor could be scheduled; check ENCRYPTION_KEY and the monitor configurations",
			"unscheduled": unscheduled,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// schedulingReady reports whether the engine is actually checking anything, and
// how many monitors it could not schedule.
//
// Split out of handleReadyz so the rule can be tested without a live database:
// the readiness handler answers the database check first, which would otherwise
// mask this one in any unit test.
func schedulingReady(e Engine) (unscheduled int, ok bool) {
	if e == nil {
		return 0, true
	}
	scheduled, unscheduled := e.Scheduling()
	// Only TOTAL failure is fatal. A single monitor with a broken config is a
	// content problem and must not take the instance out of service; zero of
	// many scheduled is the signature of a bad key.
	return unscheduled, scheduled > 0 || unscheduled == 0
}

// handleVersion reports build metadata injected via -ldflags.
func (s *server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"version":    version.Version,
		"commit":     version.Commit,
		"build_date": version.Date,
		"go_version": runtime.Version(),
	})
}
