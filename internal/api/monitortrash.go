package api

import (
	"net/http"

	"github.com/Ariazonaa/statwarden/internal/domain"
)

// The monitor recycle bin.
//
// Deleting a monitor is a soft delete (migration 0018): the row is marked, every
// ordinary read stops seeing it, and its heartbeats, tags, channel links and
// status-page placements stay exactly where they are. These three handlers are
// the only way back to it.
//
// Why this exists at all: the schema cascades heartbeats off a monitor, so a
// hard delete took years of history with it — and the bulk endpoint deletes a
// whole selection behind one confirmation. A monitor is cheap to recreate; its
// history is not, and there was nothing to undo it with.
//
// Roles match the delete itself: a writer may bin and restore, because both are
// ordinary edits with a way back. PURGING is admin-only — it is the one action
// in this file that cannot be undone, and the bin exists precisely so that the
// irreversible step is separate, deliberate, and taken by someone who can also
// reach the backups.

// handleListDeletedMonitors returns the bin, most recently deleted first.
func (s *server) handleListDeletedMonitors(w http.ResponseWriter, r *http.Request) {
	monitors, err := s.store.ListDeletedMonitors(r.Context())
	if storeError(w, err) {
		return
	}
	// Deliberately NOT decorated with links or stats. A binned monitor is not
	// scheduled, so its stats are frozen at the moment of deletion and would read
	// as current; the bin is a list of names and dates to choose from, not a
	// second dashboard.
	items := make([]monitorView, 0, len(monitors))
	for _, m := range monitors {
		// safeMonitorView, not toMonitorView: the raw mapper copies the config and
		// the push token verbatim, and a monitor in the bin is no less of a secret
		// than a live one.
		items = append(items, s.safeMonitorView(m))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleRestoreMonitor brings a monitor back from the bin.
//
// It comes back PAUSED — the store cannot know what `active` was before the
// delete, and guessing "on" would restart checks and alerts for something that
// has been gone for a week, at the moment somebody is only trying to look at its
// history. So the engine is deliberately NOT asked to schedule it here; resuming
// is one more explicit click, and that click already has a handler.
func (s *server) handleRestoreMonitor(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	restored, err := s.store.RestoreMonitor(r.Context(), id)
	if storeError(w, err) {
		return
	}
	// Reset, not Forget: this monitor is back and must be able to alert at once.
	// It also lifts the delete's tombstone, and drops whatever the dispatcher
	// still believes about this id — that belief is about a different era of the
	// monitor, and a leftover `notified` set would swallow its first DOWN alert.
	if s.dispatcher != nil {
		s.dispatcher.Reset(id)
	}
	p := principalFrom(r)
	s.audit(r.Context(), &p.userID, "monitor.restore", "monitor", &id, clientIP(r),
		map[string]any{"name": restored.Name})
	// DECORATED, unlike the bin listing above.
	//
	// This one is a live monitor again, and the response is what a client patches
	// its list with. safeMonitorView leaves tag_ids and notification_ids as the
	// empty arrays toMonitorView initialises them to, so a restore answered with
	// a monitor that had lost every tag and every alert channel — none of which
	// the soft delete had touched. The next full reload put them back, which is
	// the kind of bug that only shows up on the screen that does not reload.
	// The restore is COMMITTED by now, so a failure to decorate the response must
	// not be reported as a failure to restore.
	//
	// decorateMonitors issues two more store reads. Letting their error reach
	// storeError answered 500 for a restore that had already succeeded — and the
	// bin dialog then left the row in place, so the next click hit
	// `WHERE deleted_at IS NOT NULL`, matched nothing and returned 404 for a
	// monitor that was sitting in the live list.
	views, derr := s.decorateMonitors(r, []domain.Monitor{restored}, StatsDetail)
	if derr != nil {
		s.logger.Error("restore succeeded but the response could not be decorated",
			"monitor_id", id, "error", derr)
		writeJSON(w, http.StatusOK, s.safeMonitorView(restored))
		return
	}
	writeJSON(w, http.StatusOK, views[0])
}

// handlePurgeMonitor destroys a binned monitor and its whole history.
//
// The name is read BEFORE the delete, for the audit entry. Afterwards the row is
// gone and the log would record an id and nothing else — and this is the one
// operation whose audit line is the only remaining trace of what was removed.
func (s *server) handlePurgeMonitor(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	// Resolving it first also gives the caller a 404 for a LIVE monitor rather
	// than the store's, which matters: "purge" on something that was never
	// deleted must not be a way to skip the bin.
	doomed, err := s.store.GetDeletedMonitor(r.Context(), id)
	if storeError(w, err) {
		return
	}
	if err := s.store.PurgeMonitor(r.Context(), id); storeError(w, err) {
		return
	}
	// The same argument as the delete, only more so: the row is gone, so a
	// pending outbox row for it can never resolve to anything.
	if s.dispatcher != nil {
		s.dispatcher.Forget(id)
	}
	if s.engine != nil {
		s.engine.Remove(id)
	}
	p := principalFrom(r)
	s.audit(r.Context(), &p.userID, "monitor.purge", "monitor", &id, clientIP(r),
		map[string]any{"name": doomed.Name, "type": doomed.Type})
	w.WriteHeader(http.StatusNoContent)
}
