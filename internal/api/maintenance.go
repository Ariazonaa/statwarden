package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/Ariazonaa/statwarden/internal/maintenance"
)

// maintenanceRefreshTimeout bounds the snapshot reload that follows a
// maintenance mutation.
const maintenanceRefreshTimeout = 5 * time.Second

type maintenanceRequest struct {
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Active      *bool           `json:"active"`
	Strategy    string          `json:"strategy"`
	StartsAt    *time.Time      `json:"starts_at"`
	EndsAt      *time.Time      `json:"ends_at"`
	Recurrence  json.RawMessage `json:"recurrence"`
	Timezone    string          `json:"timezone"`
	// AppliesToAll is the window's SCOPE, and it is a POINTER so that "I am not
	// changing the scope" is expressible.
	//
	// Deriving it from `len(MonitorIDs) == 0` on every write undid migration
	// 0019 entirely: a window whose monitors had been destroyed by the cascade
	// correctly kept applies_to_all=false, and then the next edit of anything at
	// all — a typo in the title — sent an empty monitor list and re-derived it as
	// fleet-wide. The column stopped scope changing on its own; this stops an
	// unrelated edit changing it.
	//
	// Absent on CREATE still means "derive from the monitor list", so the API
	// contract for new windows is unchanged.
	AppliesToAll *bool   `json:"applies_to_all"`
	MonitorIDs   []int64 `json:"monitor_ids"`
}

func (r maintenanceRequest) toWindow() MaintenanceWindow {
	active := true
	if r.Active != nil {
		active = *r.Active
	}
	// An omitted strategy still means "single", the shape every client sent
	// before recurrence existed. A strategy that was sent but is not a known
	// one is NOT coerced: it falls through to validation and is rejected, so a
	// typo cannot quietly become a one-off window.
	strategy := r.Strategy
	if strategy == "" {
		strategy = maintenance.StrategySingle
	}
	tz := r.Timezone
	if tz == "" {
		tz = "UTC"
	}
	return MaintenanceWindow{
		Title:       r.Title,
		Description: r.Description,
		Active:      active,
		Strategy:    strategy,
		StartsAt:    r.StartsAt,
		EndsAt:      r.EndsAt,
		Recurrence:  r.Recurrence,
		Timezone:    tz,
		MonitorIDs:  r.MonitorIDs,
		// Explicit when the caller said so; otherwise derived from the monitor
		// list, which is the historical meaning and the right default for a NEW
		// window. handleUpdateMaintenance overrides this with the stored value
		// when the request is silent — see there.
		AppliesToAll: r.AppliesToAll != nil && *r.AppliesToAll ||
			r.AppliesToAll == nil && len(r.MonitorIDs) == 0,
	}
}

// toEvaluator converts a stored row into the evaluator's window shape, parsing
// the opaque recurrence payload. This is the only place the two representations
// meet: the maintenance package stays free of API and storage types.
func (m MaintenanceWindow) toEvaluator() (maintenance.Window, error) {
	w := maintenance.Window{
		ID:           m.ID,
		Title:        m.Title,
		Active:       m.Active,
		Strategy:     m.Strategy,
		StartsAt:     m.StartsAt,
		EndsAt:       m.EndsAt,
		Timezone:     m.Timezone,
		AppliesToAll: m.AppliesToAll,
		MonitorIDs:   m.MonitorIDs,
	}
	if len(m.Recurrence) > 0 && string(m.Recurrence) != "null" {
		rec, err := maintenance.ParseRecurrence(m.Recurrence)
		if err != nil {
			return maintenance.Window{}, err
		}
		w.Recurrence = rec
	}
	return w, nil
}

// NewMaintenanceLoader adapts the Store to maintenance.Loader: it reads every
// active window with its monitor ids and converts it for the evaluator. Rows
// that cannot be converted are logged and skipped rather than failing the load,
// so one unusable window never lifts maintenance for the rest.
func NewMaintenanceLoader(store Store, log *slog.Logger) maintenance.Loader {
	if log == nil {
		log = slog.Default()
	}
	return func(ctx context.Context) ([]maintenance.Window, error) {
		rows, err := store.ListMaintenance(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]maintenance.Window, 0, len(rows))
		for _, row := range rows {
			if !row.Active {
				continue
			}
			w, err := row.toEvaluator()
			if err != nil {
				log.Error("maintenance: skipping unusable window", "id", row.ID, "error", err)
				continue
			}
			out = append(out, w)
		}
		return out, nil
	}
}

// refreshMaintenance reloads the shared snapshot after a mutation so the engine
// and the public status page see the change now instead of at the next periodic
// refresh. The write has already succeeded, so a failure here is logged, not
// returned: the periodic refresh picks the change up regardless.
//
// The request context is detached first — the client disconnecting the instant
// after its write must not leave the snapshot stale.
func (s *server) refreshMaintenance(ctx context.Context) {
	if s.maint == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), maintenanceRefreshTimeout)
	defer cancel()
	if err := s.maint.Refresh(ctx); err != nil {
		s.logger.Error("maintenance: refresh after mutation", "error", err)
	}
}

func (s *server) handleListMaintenance(w http.ResponseWriter, r *http.Request) {
	windows, err := s.store.ListMaintenance(r.Context())
	if storeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, windows)
}

func (s *server) handleGetMaintenance(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	m, err := s.store.GetMaintenance(r.Context(), id)
	if storeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *server) handleCreateMaintenance(w http.ResponseWriter, r *http.Request) {
	var req maintenanceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	win := req.toWindow()
	if err := validateMaintenance(win); err != nil {
		writeProblemDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	m, err := s.store.CreateMaintenance(r.Context(), win)
	if storeError(w, err) {
		return
	}
	s.refreshMaintenance(r.Context())
	p := principalFrom(r)
	s.audit(r.Context(), &p.userID, "maintenance.create", "maintenance", &m.ID, clientIP(r), nil)
	writeJSON(w, http.StatusCreated, m)
}

func (s *server) handleUpdateMaintenance(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	var req maintenanceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	win := req.toWindow()
	// A request that does not MENTION the scope keeps the stored one.
	//
	// This is the half of migration 0019 that lives above the database. The
	// column stopped the scope changing on its own; without this, an edit that
	// changes something else changes it anyway: the editor always sends the
	// monitor list it loaded, so a window whose monitors were destroyed by the
	// cascade sends an empty list, and re-deriving from that turns a
	// covers-nothing window into a fleet-wide one on a title edit. Widening is
	// still possible — it just has to be asked for.
	if req.AppliesToAll == nil {
		if existing, err := s.store.GetMaintenance(r.Context(), id); err == nil {
			win.AppliesToAll = existing.AppliesToAll
		}
	}
	if err := validateMaintenance(win); err != nil {
		writeProblemDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	m, err := s.store.UpdateMaintenance(r.Context(), id, win)
	if storeError(w, err) {
		return
	}
	s.refreshMaintenance(r.Context())
	writeJSON(w, http.StatusOK, m)
}

func (s *server) handleDeleteMaintenance(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.store.DeleteMaintenance(r.Context(), id); storeError(w, err) {
		return
	}
	s.refreshMaintenance(r.Context())
	p := principalFrom(r)
	s.audit(r.Context(), &p.userID, "maintenance.delete", "maintenance", &id, clientIP(r), nil)
	w.WriteHeader(http.StatusNoContent)
}
