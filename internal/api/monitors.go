package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/engine"
)

// monitorRequest is the create/update payload. Durations are integer
// seconds/minutes matching the schema columns.
type monitorRequest struct {
	Name       string          `json:"name"`
	Type       string          `json:"type"`
	Config     json.RawMessage `json:"config"`
	Interval   int             `json:"interval_seconds"`
	Timeout    *int            `json:"timeout_seconds"`
	MaxRetries int             `json:"max_retries"`
	// RecoveryChecks: consecutive successes before a DOWN monitor is UP again.
	// Omitted or 0 means 1, the single-success behaviour older clients expect.
	RecoveryChecks  int     `json:"recovery_checks"`
	RetryInterval   int     `json:"retry_interval_seconds"`
	ResendInterval  int     `json:"resend_interval_minutes"`
	UpsideDown      bool    `json:"upside_down"`
	Active          *bool   `json:"active"`
	ParentID        *int64  `json:"parent_id"`
	GroupID         *int64  `json:"group_id"`
	Description     string  `json:"description"`
	LinkURL         string  `json:"link_url"`
	TagIDs          []int64 `json:"tag_ids"`
	NotificationIDs []int64 `json:"notification_ids"`
}

func (s *server) handleListMonitors(w http.ResponseWriter, r *http.Request) {
	f := MonitorFilter{
		Status: r.URL.Query().Get("status"),
		Type:   r.URL.Query().Get("type"),
		Search: r.URL.Query().Get("search"),
	}
	if raw := r.URL.Query().Get("tag_id"); raw != "" {
		if id, ok := parseInt64(raw); ok {
			f.TagID = &id
		}
	}
	// group_id=none selects the monitors filed in no folder, which no numeric
	// id can express.
	if raw := r.URL.Query().Get("group_id"); raw != "" {
		if raw == "none" {
			id := GroupUnassigned
			f.GroupID = &id
		} else if id, ok := parseInt64(raw); ok {
			f.GroupID = &id
		}
	}
	p := parsePage(r)
	monitors, total, err := s.store.ListMonitors(r.Context(), f, p)
	if storeError(w, err) {
		return
	}
	views, err := s.decorateMonitors(r, monitors, StatsList)
	if storeError(w, err) {
		return
	}
	writeList(w, views, total, p)
}

// decorateMonitors turns a page of monitors into their views, filling the
// association ids and the heartbeat-derived summary. Both decorations are
// batched across the whole page — two store calls in total, not two per
// monitor — so adding a monitor never adds a query to this endpoint.
func (s *server) decorateMonitors(r *http.Request, monitors []domain.Monitor, scope StatsScope) ([]monitorView, error) {
	ids := make([]int64, 0, len(monitors))
	for _, m := range monitors {
		ids = append(ids, m.ID)
	}
	links, err := s.store.MonitorLinks(r.Context(), ids)
	if err != nil {
		return nil, err
	}
	stats, err := s.store.MonitorStats(r.Context(), ids, s.now(), scope)
	if err != nil {
		return nil, err
	}
	views := make([]monitorView, 0, len(monitors))
	for _, m := range monitors {
		// safeMonitorView unseals the config (so the derived URL is right), redacts
		// its secret fields and drops the push token. See it for why those three
		// steps live in one place.
		v := s.decorateStatus(s.safeMonitorView(m))
		v = applyMonitorLinks(v, links[m.ID])
		v = applyMonitorStats(v, stats[m.ID])
		views = append(views, v)
	}
	return views, nil
}

func (s *server) handleGetMonitor(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	m, err := s.store.GetMonitor(r.Context(), id)
	if storeError(w, err) {
		return
	}
	views, err := s.decorateMonitors(r, []domain.Monitor{m}, StatsDetail)
	if storeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, views[0])
}

// decorateStatus overlays the engine's live in-memory status onto the stored
// row, so a just-transitioned monitor reads correctly before its row is
// flushed.
func (s *server) decorateStatus(v monitorView) monitorView {
	if s.engine != nil {
		if st, ok := s.engine.StatusOf(v.ID); ok {
			v.Status = string(st)
		}
	}
	return v
}

func (s *server) handleCreateMonitor(w http.ResponseWriter, r *http.Request) {
	var req monitorRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// A placeholder on CREATE has nothing to restore, and storing it stores the
	// mask itself as the credential.
	//
	// The read path then masks it again, so a monitor whose token is literally
	// "__redacted__" looks exactly like one with a real secret — every check
	// fails on a credential the operator cannot see is wrong. The obvious way
	// in is copying an existing monitor's configuration out of the API, where
	// the mask is what you get.
	if fields := s.placeholderFields(req.Type, req.Config); len(fields) > 0 {
		writeProblemDetail(w, http.StatusBadRequest,
			"enter the real value for "+strings.Join(fields, ", ")+
				": the masked placeholder only means \"keep the stored value\" when editing")
		return
	}
	if err := s.validateMonitor(req); err != nil {
		writeProblemDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	// Global default channels. A request that OMITS notification_ids gets every
	// active channel flagged is_default — the behaviour notify.Store already
	// documents on the flag ("channels that are auto-attached to new monitors")
	// and which nothing implemented.
	//
	// Omitted, not empty. Go decodes an absent JSON field to a nil slice and an
	// explicit [] to an empty non-nil one, and the difference is the whole
	// safety of this: "[]" is a caller saying *no channels*, and defaulting over
	// that would silently re-attach what they just removed, with no way to
	// express the choice at all. The web form always sends the field, so it is
	// unaffected — it pre-selects the same defaults visibly instead (see
	// MonitorForm), which is where an override belongs.
	//
	// Only ACTIVE defaults: attaching a disabled channel would show a monitor as
	// covered by alerting that cannot deliver.
	if req.NotificationIDs == nil {
		ids, err := s.defaultNotificationIDs(r.Context())
		if storeError(w, err) {
			return
		}
		req.NotificationIDs = ids
	}
	// A monitor that does not exist yet cannot be anyone's parent, so id 0 here
	// can only fail the "parent must exist" leg — never the cycle leg.
	if err := s.validateMonitorRelations(r.Context(), 0, req); validationOrStoreError(w, err) {
		return
	}
	p := principalFrom(r)
	in, err := s.monitorInput(req, &p.userID)
	if err != nil {
		s.logger.Error("seal monitor config failed", "error", err)
		writeProblemDetail(w, http.StatusInternalServerError, "could not secure the monitor configuration")
		return
	}
	m, err := s.store.CreateMonitor(r.Context(), in)
	if storeError(w, err) {
		return
	}
	// The store round-trips the sealed config; the engine and the view both
	// need plaintext, so open it once here.
	m.Config = s.decryptMonitorConfig(m.Config)
	// Drive the engine: a new active monitor is scheduled immediately.
	//
	// A refusal is carried back to the caller, not just logged. The row says
	// active, the dashboard shows it, and nothing is checking it — the exact
	// condition handleResumeMonitor refuses to answer 204 to. It is reachable: a
	// config the box cannot open comes back from decryptMonitorConfig as something
	// the checker rejects, which is the production incident #79 describes.
	var schedulingError string
	if s.engine != nil && m.Active {
		if err := s.engine.Upsert(m); err != nil {
			s.logger.Error("monitor created but could not be scheduled",
				"monitor_id", m.ID, "error", err)
			schedulingError = "the monitor was created but could not be scheduled; it is not being checked"
		}
	}
	s.audit(r.Context(), &p.userID, "monitor.create", "monitor", &m.ID, clientIP(r), map[string]any{"name": m.Name, "type": m.Type})
	// The associations were just written from this request, so echo them
	// rather than re-reading them: a fresh monitor has no stats to fetch.
	//
	// The push token is the exception this endpoint is allowed to make: it is
	// revealed exactly once, here, and never again through a read. So the plain
	// mapper is used and only the config is redacted.
	view := applyMonitorLinks(toMonitorView(m), MonitorLinks{TagIDs: in.TagIDs, NotificationIDs: in.NotificationIDs})
	view.Config = s.redactMonitorConfig(m.Type, view.Config)
	view.SchedulingError = schedulingError
	writeJSON(w, http.StatusCreated, view)
}

func (s *server) handleUpdateMonitor(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	var req monitorRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// Restore any secret the client sent back as the redaction placeholder from
	// the stored (decrypted) config BEFORE validation, so leaving a masked
	// credential untouched keeps it rather than persisting the placeholder or
	// failing type validation (e.g. a headers object masked to a string).
	existing, err := s.store.GetMonitor(r.Context(), id)
	if storeError(w, err) {
		return
	}
	cfg, ok := s.keepRedactedSecrets(req.Type, req.Config, s.decryptMonitorConfig(existing.Config))
	if !ok {
		// The write kept a masked credential AND changed where the check
		// connects. Honouring both would send a password the caller cannot read
		// to a host the caller chose — redaction protecting the value on screen
		// and handing it over on the wire.
		writeProblemDetail(w, http.StatusBadRequest,
			"re-enter the credentials when changing where this monitor connects; "+
				"a masked value cannot be carried over to a new target")
		return
	}
	req.Config = cfg
	if err := s.validateMonitor(req); err != nil {
		writeProblemDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.validateMonitorRelations(r.Context(), id, req); validationOrStoreError(w, err) {
		return
	}
	in, err := s.monitorInput(req, nil)
	if err != nil {
		s.logger.Error("seal monitor config failed", "error", err)
		writeProblemDetail(w, http.StatusInternalServerError, "could not secure the monitor configuration")
		return
	}
	m, err := s.store.UpdateMonitor(r.Context(), id, in)
	if storeError(w, err) {
		return
	}
	// The engine and the view both need plaintext; the store round-trips the
	// sealed config.
	m.Config = s.decryptMonitorConfig(m.Config)
	// Live-reload the engine: an inactive monitor is removed, an active one
	// re-scheduled with the new config.
	var schedulingError string
	if s.engine != nil {
		if m.Active {
			if err := s.engine.Upsert(m); err != nil {
				// Same condition create reports, same reason: the row is active and the
				// scheduler does not have it. A PUT is idempotent, so unlike create this
				// one COULD answer 5xx safely — it does not, because the write did
				// happen and reporting it as a server error would send the caller
				// hunting a save that actually landed. The body carries the reason.
				s.logger.Error("monitor updated but could not be scheduled",
					"monitor_id", m.ID, "error", err)
				schedulingError = "the monitor was saved but could not be scheduled; it is not being checked"
			}
		} else {
			s.engine.Remove(m.ID)
		}
	}
	// Deactivating through a full-replace PUT is a pause by another name, and it
	// unschedules the monitor the same way — so the outage it may be in has to be
	// closed here too. Without it the resend timer goes on announcing an outage
	// on a monitor nobody is checking, exactly as it did before pause was wired.
	if !m.Active && s.dispatcher != nil {
		s.dispatcher.Reset(m.ID)
	}
	p := principalFrom(r)
	s.audit(r.Context(), &p.userID, "monitor.update", "monitor", &m.ID, clientIP(r), nil)
	views, err := s.decorateMonitors(r, []domain.Monitor{m}, StatsDetail)
	if storeError(w, err) {
		return
	}
	view := views[0]
	view.SchedulingError = schedulingError
	writeJSON(w, http.StatusOK, view)
}

func (s *server) handleDeleteMonitor(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.store.DeleteMonitor(r.Context(), id); storeError(w, err) {
		return
	}
	if s.engine != nil {
		s.engine.Remove(id)
	}
	// Unscheduling it is only half of it: the dispatcher may still be holding an
	// open outage for this monitor, whose recovery event can now never arrive.
	if s.dispatcher != nil {
		s.dispatcher.Forget(id)
	}
	p := principalFrom(r)
	s.audit(r.Context(), &p.userID, "monitor.delete", "monitor", &id, clientIP(r), nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handlePauseMonitor(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.store.SetMonitorActive(r.Context(), id, false); storeError(w, err) {
		return
	}
	if s.engine != nil {
		_ = s.engine.Pause(id)
	}
	// A pause is not an outage, so the engine deliberately emits no event for
	// it — which means nothing else will ever close the outage this monitor may
	// be in. Left alone, its resend timer announces an outage on a monitor
	// nobody is checking, and resuming it later meets a full `notified` set that
	// swallows the first real alert.
	if s.dispatcher != nil {
		s.dispatcher.Reset(id)
	}
	p := principalFrom(r)
	s.audit(r.Context(), &p.userID, "monitor.pause", "monitor", &id, clientIP(r), nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleResumeMonitor(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.store.SetMonitorActive(r.Context(), id, true); storeError(w, err) {
		return
	}
	if err := s.scheduleActive(r.Context(), id); err != nil {
		// The row says active and the scheduler does not have it, which means
		// the monitor is not being checked. Saying "no content" here is how a
		// monitor comes to look healthy in the UI while nothing watches it, so
		// the failure is reported instead.
		s.logger.Error("monitor resumed in the database but could not be scheduled",
			"monitor_id", id, "error", err)
		writeProblemDetail(w, http.StatusInternalServerError,
			"monitor was activated but could not be scheduled; it is not being checked")
		return
	}
	p := principalFrom(r)
	s.audit(r.Context(), &p.userID, "monitor.resume", "monitor", &id, clientIP(r), nil)
	w.WriteHeader(http.StatusNoContent)
}

// scheduleActive gets a monitor running in the engine after it has been marked
// active. Resume covers the monitor the scheduler already knows; anything else
// — created while paused, or added since this process started — has to be
// loaded and upserted.
//
// The load path is the one that matters: monitors.config is stored SEALED, and
// handing the envelope to Upsert means the checker rejects it and the monitor
// is silently not scheduled. The single-monitor handler decrypted; the bulk
// action did not, and swallowed the error on top, so a bulk resume reported
// success for monitors that would not run again until a restart. One helper for
// both, so the two cannot drift apart again.
func (s *server) scheduleActive(ctx context.Context, id int64) error {
	if s.engine == nil {
		return nil
	}
	if err := s.engine.Resume(id); err == nil {
		return nil
	}
	m, err := s.store.GetMonitor(ctx, id)
	if err != nil {
		return fmt.Errorf("load monitor %d: %w", id, err)
	}
	m.Config = s.decryptMonitorConfig(m.Config)
	if err := s.engine.Upsert(m); err != nil {
		return fmt.Errorf("schedule monitor %d: %w", id, err)
	}
	return nil
}

func (s *server) handleCheckMonitor(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	if _, err := s.store.GetMonitor(r.Context(), id); storeError(w, err) {
		return
	}
	if s.engine != nil {
		// A check already in flight is success, not a conflict: the caller asked
		// for the monitor to be checked now and it is being checked now. The
		// frontend polls last_checked_at either way, and the running check
		// updates it.
		if err := s.engine.CheckNow(id); err != nil && !errors.Is(err, engine.ErrCheckInProgress) {
			writeProblemDetail(w, http.StatusConflict, err.Error())
			return
		}
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *server) handleMonitorStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	m, err := s.store.GetMonitor(r.Context(), id)
	if storeError(w, err) {
		return
	}
	status := string(m.Status)
	if s.engine != nil {
		if st, ok := s.engine.StatusOf(id); ok {
			status = string(st)
		}
	}
	latest, has, err := s.store.LatestHeartbeat(r.Context(), id)
	if storeError(w, err) {
		return
	}
	resp := map[string]any{"monitor_id": id, "status": status}
	if has {
		resp["last_heartbeat"] = latest
	}
	writeJSON(w, http.StatusOK, resp)
}

// maxRecoveryChecks bounds monitorRequest.RecoveryChecks.
const maxRecoveryChecks = 100

// validateMonitor checks required fields and delegates type-specific config
// validation to the registered checker.
func (s *server) validateMonitor(req monitorRequest) error {
	if req.Name == "" {
		return validationError("name is required")
	}
	checker, ok := s.monReg.Get(req.Type)
	if !ok {
		return validationError("unknown monitor type " + req.Type)
	}
	if req.Interval < 1 {
		return validationError("interval_seconds must be >= 1")
	}
	// The ceiling is a sanity bound, not a policy: past it a monitor would take
	// hours to be believed back up, which is a typo, not a setting.
	if req.RecoveryChecks < 0 || req.RecoveryChecks > maxRecoveryChecks {
		return validationError(fmt.Sprintf("recovery_checks must be between 1 and %d", maxRecoveryChecks))
	}
	if err := validateLinkURL(req.LinkURL); err != nil {
		return validationError(err.Error())
	}
	cfg := req.Config
	if len(cfg) == 0 {
		cfg = json.RawMessage(`{}`)
	}
	if err := checker.Validate(cfg); err != nil {
		return validationError("invalid config: " + err.Error())
	}
	return nil
}

// maxParentChain bounds the parent walk in validateMonitorParent. It is a
// safety stop, not a product limit: a dependency chain deeper than this is
// already pathological, and the bound guarantees the walk terminates even if
// stored data somehow already contains a cycle.
const maxParentChain = 64

// validateMonitorRelations checks the two foreign keys a monitor write can
// carry: the parent it depends on and the folder it is filed under. id is the
// monitor being written, or 0 on create.
func (s *server) validateMonitorRelations(ctx context.Context, id int64, req monitorRequest) error {
	if err := s.validateMonitorParent(ctx, id, req.ParentID); err != nil {
		return err
	}
	if req.GroupID != nil {
		if _, err := s.store.GetMonitorGroup(ctx, *req.GroupID); err != nil {
			if errors.Is(err, ErrNotFound) {
				return validationError("group_id references an unknown monitor group")
			}
			return err
		}
	}
	return nil
}

// validateMonitorParent rejects a parent assignment that would close a loop in
// the monitors.parent_id chain: a monitor pointing at itself, a two-monitor
// A→B→A, and any longer chain.
//
// This has to be server-side. The engine walks this same chain to decide
// whether to suppress a monitor's alerts while its parent is DOWN, so a cycle
// is not untidy data — it is a live hazard in the alerting path. A client-side
// guard would also miss the case where two writers each add a benign-looking
// edge that only forms a loop together.
//
// The walk starts at the PROPOSED parent and climbs. Reaching id means the new
// edge closes the loop. It costs one store read per hop, on a write path, over
// chains that are a handful of links deep in practice.
func (s *server) validateMonitorParent(ctx context.Context, id int64, parentID *int64) error {
	if parentID == nil {
		return nil
	}
	if *parentID == id {
		return validationError("parent_id must not reference the monitor itself")
	}
	seen := make(map[int64]bool)
	for cur := *parentID; ; {
		if cur == id {
			return validationError("parent_id would create a dependency cycle")
		}
		// A repeat means the STORED chain already loops, independently of this
		// request. Refuse to extend it rather than spin.
		if seen[cur] || len(seen) >= maxParentChain {
			return validationError("parent_id would create a dependency cycle")
		}
		seen[cur] = true
		// A monitor in the recycle bin is not found here, so a chain that runs
		// through one is refused — including when it is an ANCESTOR rather than
		// the proposed parent, which makes a child of a binned monitor
		// un-editable until its parent field is cleared.
		//
		// That is deliberate, and the strict reading is the safe one. Stopping
		// the walk at a binned ancestor instead would let an edge be added that
		// only closes a loop once that ancestor is restored — and a cycle here is
		// a live hazard in the alerting path, not untidy data. The error names the
		// field, and clearing a dangling parent is one dropdown.
		m, err := s.store.GetMonitor(ctx, cur)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return validationError("parent_id references an unknown monitor")
			}
			return err
		}
		if m.ParentID == nil {
			return nil
		}
		cur = *m.ParentID
	}
}

// defaultNotificationIDs lists the channels a newly created monitor is attached
// to when the request names none: those flagged is_default AND still active.
//
// Empty, never nil, so the caller can tell "no defaults configured" from "not
// asked" — handleCreateMonitor assigns the result over a nil NotificationIDs and
// must not put nil back.
func (s *server) defaultNotificationIDs(ctx context.Context) ([]int64, error) {
	all, err := s.store.ListNotifications(ctx)
	if err != nil {
		return nil, err
	}
	ids := []int64{}
	for _, n := range all {
		if n.IsDefault && n.Active {
			ids = append(ids, n.ID)
		}
	}
	return ids, nil
}

func (s *server) monitorInput(req monitorRequest, createdBy *int64) (MonitorInput, error) {
	active := true
	if req.Active != nil {
		active = *req.Active
	}
	cfg := req.Config
	if len(cfg) == 0 {
		cfg = json.RawMessage(`{}`)
	}
	retry := req.RetryInterval
	if retry < 1 {
		retry = req.Interval
	}
	// Seal secrets at rest when a box is configured; the store expects an
	// already-encrypted config, mirroring the notify package's persistence
	// boundary. Fail CLOSED: a seal error must never fall through to storing the
	// credentials in plaintext — the whole point of the P0 fix is that
	// monitor.config is encrypted at rest.
	if s.box != nil {
		sealed, err := s.box.EncryptConfig(cfg)
		if err != nil {
			return MonitorInput{}, fmt.Errorf("seal monitor config: %w", err)
		}
		cfg = sealed
	}
	return MonitorInput{
		Name:            req.Name,
		Type:            req.Type,
		Config:          cfg,
		Interval:        req.Interval,
		Timeout:         req.Timeout,
		MaxRetries:      req.MaxRetries,
		RecoveryChecks:  recoveryChecksOrDefault(req.RecoveryChecks),
		RetryInterval:   retry,
		ResendInterval:  req.ResendInterval,
		UpsideDown:      req.UpsideDown,
		Active:          active,
		ParentID:        req.ParentID,
		GroupID:         req.GroupID,
		Description:     req.Description,
		LinkURL:         req.LinkURL,
		TagIDs:          req.TagIDs,
		NotificationIDs: req.NotificationIDs,
		CreatedBy:       createdBy,
	}, nil
}

// parseInt64 accepts a bare non-negative decimal id and nothing else — no sign,
// no whitespace, no leading "+".
//
// The digit check stays because strconv.ParseInt would accept "+5" and "-5";
// the accumulation is delegated because the hand-rolled loop silently WRAPPED on
// overflow, so a long digit string parsed as an arbitrary id (and could land on
// 0, the GroupUnassigned sentinel).
func parseInt64(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// validationError is a lightweight error carrying a client-facing message.
type validationError string

func (e validationError) Error() string { return string(e) }
