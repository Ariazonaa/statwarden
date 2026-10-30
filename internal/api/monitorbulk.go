package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// Bulk action names.
const (
	bulkActionPause    = "pause"
	bulkActionResume   = "resume"
	bulkActionDelete   = "delete"
	bulkActionTag      = "tag"
	bulkActionGroup    = "group"
	bulkActionNotify   = "notify"
	bulkActionSchedule = "schedule"
)

// maxBulkMonitors caps one batch. It matches the list endpoint's page cap, so
// "select everything on screen and act" always fits in a single call.
const maxBulkMonitors = 200

// bulkMonitorRequest is one action applied to an explicit set of monitors. The
// id list is explicit rather than a filter on purpose: a filter evaluated
// server-side can match monitors the operator never saw, which is the wrong
// failure mode for an endpoint whose actions include delete.
type bulkMonitorRequest struct {
	Action     string  `json:"action"`
	MonitorIDs []int64 `json:"monitor_ids"`
	// TagIDs is required by the "tag" action and ignored otherwise.
	TagIDs []int64 `json:"tag_ids"`
	// GroupID is used by the "group" action: a folder id to file the monitors
	// under, or null to clear the folder.
	GroupID *int64 `json:"group_id"`
	// NotificationIDs is required by the "notify" action and ignored otherwise;
	// its channels are ADDED to each monitor, matching the additive "tag" action.
	NotificationIDs []int64 `json:"notification_ids"`
	// The "schedule" action's timing values. IntervalSeconds is required by it
	// and ignored otherwise; the three pointer fields are optional overrides,
	// left untouched on each monitor when nil.
	IntervalSeconds      int  `json:"interval_seconds"`
	TimeoutSeconds       *int `json:"timeout_seconds"`
	MaxRetries           *int `json:"max_retries"`
	RetryIntervalSeconds *int `json:"retry_interval_seconds"`
}

// bulkMonitorResult is one monitor's outcome. Error is a short, client-facing
// reason, empty when OK.
type bulkMonitorResult struct {
	MonitorID int64  `json:"monitor_id"`
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
}

// bulkMonitorResponse reports the whole batch: the counts a toast can render
// plus the per-id detail a UI needs to say WHICH monitor failed.
type bulkMonitorResponse struct {
	Action    string              `json:"action"`
	Succeeded int                 `json:"succeeded"`
	Failed    int                 `json:"failed"`
	Results   []bulkMonitorResult `json:"results"`
}

// handleBulkMonitors applies one action to many monitors in a single request.
//
// On partial failure it reports per-id outcomes and returns 200 rather than
// failing the batch. Two reasons, and they are not cosmetic:
//
//   - The engine side effects (Pause, Remove, Upsert) are not transactional and
//     cannot be rolled back. Wrapping the batch in one database transaction
//     would, on a late failure, roll the rows back while leaving the scheduler
//     already stopped — the database and the engine would disagree, which is
//     strictly worse than a partial batch the caller was told about.
//   - One stale id in a selection (a monitor someone else deleted between the
//     list render and the click) is routine, and must not silently discard the
//     other 49 monitors' worth of intent.
//
// Each monitor's own mutation is still atomic: the store writes are single
// statements or already-transactional methods.
//
// Authorisation is uniform, not per-id: the route sits behind
// requireRole(roleWriter) and this product has no per-monitor ownership, so a
// caller either may act on all of them or on none and never gets this far.
// Should per-monitor permissions ever land, they belong in the per-id loop
// below as an "forbidden" result, NOT as a whole-batch rejection — same reason
// as the stale-id case.
func (s *server) handleBulkMonitors(w http.ResponseWriter, r *http.Request) {
	var req bulkMonitorRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ids, err := normaliseBulkIDs(req.MonitorIDs)
	if err != nil {
		writeProblemDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	// Action-wide arguments are validated ONCE, before anything is written: an
	// unknown tag or folder is a malformed request, not a per-monitor outcome,
	// and would otherwise fail identically on every id in the batch.
	if err := s.validateBulkArgs(r.Context(), req); validationOrStoreError(w, err) {
		return
	}

	p := principalFrom(r)
	resp := bulkMonitorResponse{Action: req.Action, Results: make([]bulkMonitorResult, 0, len(ids))}
	for _, id := range ids {
		err := s.applyBulkAction(r.Context(), req, id)
		res := bulkMonitorResult{MonitorID: id, OK: err == nil}
		if err != nil {
			res.Error = bulkErrorReason(err)
			resp.Failed++
		} else {
			resp.Succeeded++
		}
		resp.Results = append(resp.Results, res)
	}
	s.audit(r.Context(), &p.userID, "monitor.bulk."+req.Action, "monitor", nil, clientIP(r),
		map[string]any{"requested": len(ids), "succeeded": resp.Succeeded, "failed": resp.Failed})
	writeJSON(w, http.StatusOK, resp)
}

// normaliseBulkIDs rejects an empty or oversized batch and drops duplicate ids,
// preserving the caller's order. Deduplication matters for correctness, not
// just efficiency: applying "tag" twice is harmless, but a repeated id would
// otherwise be counted twice in the response and report a spurious "not found"
// on its second pass through "delete".
func normaliseBulkIDs(raw []int64) ([]int64, error) {
	if len(raw) == 0 {
		return nil, validationError("monitor_ids must not be empty")
	}
	if len(raw) > maxBulkMonitors {
		return nil, validationError("monitor_ids exceeds the maximum batch size")
	}
	seen := make(map[int64]bool, len(raw))
	out := make([]int64, 0, len(raw))
	for _, id := range raw {
		if id <= 0 {
			return nil, validationError("monitor_ids contains an invalid id")
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, nil
}

// validateBulkArgs checks the action name and the arguments it needs.
func (s *server) validateBulkArgs(ctx context.Context, req bulkMonitorRequest) error {
	switch req.Action {
	case bulkActionPause, bulkActionResume, bulkActionDelete:
		return nil
	case bulkActionTag:
		if len(req.TagIDs) == 0 {
			return validationError("tag_ids is required for the tag action")
		}
		for _, tagID := range req.TagIDs {
			if _, err := s.store.GetTag(ctx, tagID); err != nil {
				if errors.Is(err, ErrNotFound) {
					return validationError("tag_ids references an unknown tag")
				}
				return err
			}
		}
		return nil
	case bulkActionGroup:
		// A null group_id is meaningful here: it clears the folder.
		if req.GroupID == nil {
			return nil
		}
		if _, err := s.store.GetMonitorGroup(ctx, *req.GroupID); err != nil {
			if errors.Is(err, ErrNotFound) {
				return validationError("group_id references an unknown monitor group")
			}
			return err
		}
		return nil
	case bulkActionNotify:
		if len(req.NotificationIDs) == 0 {
			return validationError("notification_ids is required for the notify action")
		}
		for _, nid := range req.NotificationIDs {
			if _, err := s.store.GetNotification(ctx, nid); err != nil {
				if errors.Is(err, ErrNotFound) {
					return validationError("notification_ids references an unknown notification")
				}
				return err
			}
		}
		return nil
	case bulkActionSchedule:
		// The bounds mirror the monitors table's CHECK constraints and the create
		// path's own interval guard, so an out-of-range value is a whole-request
		// 400 here rather than a per-id constraint violation once the writes start.
		if req.IntervalSeconds < 1 {
			return validationError("interval_seconds must be >= 1")
		}
		if req.TimeoutSeconds != nil && *req.TimeoutSeconds < 1 {
			return validationError("timeout_seconds must be >= 1")
		}
		if req.MaxRetries != nil && *req.MaxRetries < 0 {
			return validationError("max_retries must be >= 0")
		}
		if req.RetryIntervalSeconds != nil && *req.RetryIntervalSeconds < 1 {
			return validationError("retry_interval_seconds must be >= 1")
		}
		return nil
	default:
		return validationError("unknown action " + req.Action)
	}
}

// applyBulkAction performs one action on one monitor, driving the engine
// exactly as the corresponding single-monitor handler does. That parity is the
// whole point: a bulk pause that updates rows without reaching the scheduler
// leaves monitors that read as paused and keep checking and keep alerting.
func (s *server) applyBulkAction(ctx context.Context, req bulkMonitorRequest, id int64) error {
	switch req.Action {
	case bulkActionPause:
		if err := s.store.SetMonitorActive(ctx, id, false); err != nil {
			return err
		}
		if s.engine != nil {
			_ = s.engine.Pause(id)
		}
		// Same as the single pause: the engine emits no event for it, so nothing
		// else closes an outage this monitor may be in.
		if s.dispatcher != nil {
			s.dispatcher.Reset(id)
		}
		return nil

	case bulkActionResume:
		if err := s.store.SetMonitorActive(ctx, id, true); err != nil {
			return err
		}
		// Reported rather than swallowed: the per-id result exists precisely so
		// a caller can be told WHICH monitor did not come back.
		return s.scheduleActive(ctx, id)

	case bulkActionDelete:
		if err := s.store.DeleteMonitor(ctx, id); err != nil {
			return err
		}
		if s.engine != nil {
			s.engine.Remove(id)
		}
		// Same as the single delete: drop the outage the dispatcher is still
		// holding, which nothing will ever close now that the checks have
		// stopped.
		if s.dispatcher != nil {
			s.dispatcher.Forget(id)
		}
		return nil

	case bulkActionTag:
		// No engine call: tags are not part of domain.Monitor and nothing
		// scheduled changed.
		return s.store.AddMonitorTags(ctx, id, req.TagIDs)

	case bulkActionGroup:
		// No engine call either: a folder is presentation only. Unlike
		// parent_id, the engine never reads group_id.
		return s.store.SetMonitorGroup(ctx, id, req.GroupID)

	case bulkActionNotify:
		// No engine call: notification links are not part of domain.Monitor, so
		// nothing the scheduler reads changed.
		return s.store.AddMonitorNotifications(ctx, id, req.NotificationIDs)

	case bulkActionSchedule:
		if err := s.store.SetMonitorSchedule(ctx, id, MonitorSchedule{
			Interval:      req.IntervalSeconds,
			Timeout:       req.TimeoutSeconds,
			MaxRetries:    req.MaxRetries,
			RetryInterval: req.RetryIntervalSeconds,
		}); err != nil {
			return err
		}
		// The cadence the scheduler ticks on just changed, so an active monitor
		// must be re-Upserted or it keeps checking on the old interval — the same
		// engine-parity requirement as bulk pause, and the reason this is not a
		// row-only write. A paused monitor is not scheduled, so it is left alone
		// and picks up the new timing when it is resumed.
		//
		// Both failures below are REPORTED, like bulkActionResume above and unlike
		// the version of this that swallowed them. Dropping them produced exactly
		// the row-only write the paragraph above rules out: an Upsert the engine
		// rejects (a config it cannot open, a type it does not know) left the
		// monitor ticking on its old interval while the per-id result said ok, and
		// the per-id result exists precisely so a caller can be told which monitor
		// did not take the change.
		if s.engine == nil {
			return nil
		}
		m, err := s.store.GetMonitor(ctx, id)
		if err != nil {
			return fmt.Errorf("re-read monitor %d after the schedule change: %w", id, err)
		}
		if !m.Active {
			return nil
		}
		m.Config = s.decryptMonitorConfig(m.Config)
		if err := s.engine.Upsert(m); err != nil {
			return fmt.Errorf("apply the new schedule to monitor %d: %w", id, err)
		}
		return nil

	default:
		// Unreachable: validateBulkArgs rejected unknown actions already.
		return validationError("unknown action " + req.Action)
	}
}

// bulkErrorReason renders a per-id failure for the client without leaking
// internals: a missing monitor is named as such, everything else is generic.
func bulkErrorReason(err error) string {
	switch {
	case errors.Is(err, ErrNotFound):
		return "not found"
	case errors.Is(err, ErrConflict):
		return "conflict"
	default:
		return "internal error"
	}
}
