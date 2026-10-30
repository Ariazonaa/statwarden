package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

func validIncidentStatus(s string) bool {
	switch s {
	case IncidentInvestigating, IncidentIdentified, IncidentMonitoring, IncidentResolved:
		return true
	}
	return false
}

func validSeverity(s string) bool {
	switch s {
	case "minor", "major", "critical":
		return true
	}
	return false
}

type incidentRequest struct {
	Title      string  `json:"title"`
	Body       string  `json:"body"`
	Severity   string  `json:"severity"`
	Status     string  `json:"status"`
	Pinned     bool    `json:"pinned"`
	MonitorIDs []int64 `json:"monitor_ids"`
}

func (s *server) handleListIncidents(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	if _, err := s.store.GetStatusPage(r.Context(), id); storeError(w, err) {
		return
	}
	incidents, err := s.store.ListIncidents(r.Context(), id)
	if storeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": incidents})
}

// incidentForPage resolves the {incID} incident and proves it belongs to the
// {id} status page. It writes the response and returns false when either id is
// unusable or the incident belongs to a different page.
//
// Every one of these routes carries BOTH ids and only one was ever read: Get,
// Update, Delete and AddUpdate addressed the incident directly, so
// /status-pages/1/incidents/99 acted on incident 99 even when it belonged to page
// 2. That was never a privilege boundary — a writer may edit every status page,
// and a viewer may already list every page's incidents — which is why it is a
// correctness bug rather than an IDOR: the caller asked about one page and was
// answered about another, an edit landed on a page nobody named, and the audit
// entry recorded it against the id in the URL.
//
// A mismatch is a 404, not a 403: under the page that was asked about, that
// incident genuinely does not exist.
func (s *server) incidentForPage(w http.ResponseWriter, r *http.Request) (Incident, bool) {
	pageID, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return Incident{}, false
	}
	incID, ok := idParam(r, "incID")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid incident id")
		return Incident{}, false
	}
	in, err := s.store.GetIncident(r.Context(), incID)
	if storeError(w, err) {
		return Incident{}, false
	}
	if in.StatusPageID != pageID {
		writeProblemDetail(w, http.StatusNotFound, "resource not found")
		return Incident{}, false
	}
	return in, true
}

func (s *server) handleGetIncident(w http.ResponseWriter, r *http.Request) {
	in, ok := s.incidentForPage(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, in)
}

func (s *server) handleCreateIncident(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	page, err := s.store.GetStatusPage(r.Context(), id)
	if storeError(w, err) {
		return
	}
	var req incidentRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Title == "" {
		writeProblemDetail(w, http.StatusBadRequest, "title is required")
		return
	}
	if req.Severity == "" {
		req.Severity = "minor"
	}
	if req.Status == "" {
		req.Status = IncidentInvestigating
	}
	if !validSeverity(req.Severity) || !validIncidentStatus(req.Status) {
		writeProblemDetail(w, http.StatusBadRequest, "invalid severity or status")
		return
	}
	pr := principalFrom(r)
	in := Incident{
		StatusPageID: id,
		Title:        req.Title,
		Body:         req.Body,
		Severity:     req.Severity,
		Status:       req.Status,
		Pinned:       req.Pinned,
		MonitorIDs:   req.MonitorIDs,
		CreatedBy:    &pr.userID,
	}
	created, err := s.store.CreateIncident(r.Context(), in)
	if storeError(w, err) {
		return
	}
	s.audit(r.Context(), &pr.userID, "incident.create", "incident", &created.ID, clientIP(r), nil)
	s.announceIncident(page, created, "New incident")
	writeJSON(w, http.StatusCreated, created)
}

func (s *server) handleUpdateIncident(w http.ResponseWriter, r *http.Request) {
	existing, ok := s.incidentForPage(w, r)
	if !ok {
		return
	}
	incID := existing.ID
	var req incidentRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Severity == "" {
		req.Severity = existing.Severity
	}
	if req.Status == "" {
		req.Status = existing.Status
	}
	if !validSeverity(req.Severity) || !validIncidentStatus(req.Status) {
		writeProblemDetail(w, http.StatusBadRequest, "invalid severity or status")
		return
	}
	existing.Title = req.Title
	existing.Body = req.Body
	existing.Severity = req.Severity
	existing.Status = req.Status
	existing.Pinned = req.Pinned
	existing.MonitorIDs = req.MonitorIDs
	if req.Status == IncidentResolved && existing.ResolvedAt == nil {
		now := s.now()
		existing.ResolvedAt = &now
	}
	if req.Status != IncidentResolved {
		existing.ResolvedAt = nil
	}
	updated, err := s.store.UpdateIncident(r.Context(), incID, existing)
	if storeError(w, err) {
		return
	}
	// Announced, like every other change to an incident.
	//
	// Adding a timeline entry announced; editing the incident directly did not,
	// so a status page open in a browser kept showing the old title, severity —
	// or "investigating" on something that had just been marked resolved —
	// until someone reloaded. The page's whole point is that nobody has to.
	if page, perr := s.store.GetStatusPage(r.Context(), updated.StatusPageID); perr == nil {
		verb := "Incident updated"
		if updated.Status == IncidentResolved {
			verb = "Incident resolved"
		}
		s.announceIncident(page, updated, verb)
	}
	pr := principalFrom(r)
	s.audit(r.Context(), &pr.userID, "incident.edit", "incident", &incID, clientIP(r), nil)
	writeJSON(w, http.StatusOK, updated)
}

func (s *server) handleDeleteIncident(w http.ResponseWriter, r *http.Request) {
	existing, ok := s.incidentForPage(w, r)
	if !ok {
		return
	}
	incID := existing.ID
	// Read the page BEFORE the delete: afterwards the incident is gone and its
	// StatusPageID with it, and the announcement below needs the slug.
	page, perr := s.store.GetStatusPage(r.Context(), existing.StatusPageID)
	if err := s.store.DeleteIncident(r.Context(), incID); storeError(w, err) {
		return
	}
	// Announced, for the reason handleUpdateIncident states: an open status page
	// must not keep showing something that no longer exists. Editing an incident
	// was fixed to announce and deleting one was not, so a removed incident stayed
	// on every public page until somebody reloaded — the longest-lived stale state
	// of the three, because nothing else would ever touch that incident again.
	if perr == nil {
		s.announceIncident(page, existing, "Incident deleted")
	}
	pr := principalFrom(r)
	s.audit(r.Context(), &pr.userID, "incident.delete", "incident", &incID, clientIP(r), nil)
	w.WriteHeader(http.StatusNoContent)
}

type incidentUpdateRequest struct {
	Status string `json:"status"`
	Body   string `json:"body"`
}

// handleAddIncidentUpdate posts a new update to an incident's timeline and
// advances its lifecycle status.
func (s *server) handleAddIncidentUpdate(w http.ResponseWriter, r *http.Request) {
	existing, ok := s.incidentForPage(w, r)
	if !ok {
		return
	}
	incID := existing.ID
	var req incidentUpdateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Status == "" {
		req.Status = existing.Status
	}
	if !validIncidentStatus(req.Status) {
		writeProblemDetail(w, http.StatusBadRequest, "invalid status")
		return
	}
	if req.Body == "" {
		writeProblemDetail(w, http.StatusBadRequest, "update body is required")
		return
	}
	pr := principalFrom(r)
	u := IncidentUpdate{
		IncidentID: incID,
		Status:     req.Status,
		Body:       req.Body,
		CreatedBy:  &pr.userID,
	}
	if _, err := s.store.AddIncidentUpdate(r.Context(), u); storeError(w, err) {
		return
	}
	// Reload the incident (status/resolved_at advanced) and announce.
	refreshed, err := s.store.GetIncident(r.Context(), incID)
	if storeError(w, err) {
		return
	}
	page, err := s.store.GetStatusPage(r.Context(), existing.StatusPageID)
	if err == nil {
		verb := "Incident updated"
		if req.Status == IncidentResolved {
			verb = "Incident resolved"
		}
		s.announceIncident(page, refreshed, verb)
	}
	s.audit(r.Context(), &pr.userID, "incident.update", "incident", &incID, clientIP(r), nil)
	writeJSON(w, http.StatusCreated, refreshed)
}

// maxPostmortemBytes caps one retrospective.
//
// decodeJSON already refuses a body over 1 MiB, but that is a transport guard,
// not a content one: this text is embedded in EVERY public status-page payload
// for the incident, so an unbounded field is an unbounded page. 64 KiB is far
// more than a retrospective anyone reads and far less than a page nobody can
// load.
const maxPostmortemBytes = 64 * 1024

// postmortemRequest is the body of PUT …/incidents/{incID}/postmortem.
//
// Two fields, not one, because writing and publishing are separate decisions: a
// retrospective is drafted over days and reviewed before it is shown to the
// public. Published=false with a non-empty Body is the ordinary intermediate
// state, and it is what the handler defaults to.
type postmortemRequest struct {
	// Body is a POINTER so that "leave the text as it is" and "clear the text"
	// are different requests.
	//
	// As a plain string they were the same one. An unpublish is naturally
	// written as {"published": false} — that is the whole of what the caller
	// means to change — and the zero value silently made it {"body": "",
	// "published": false}, which overwrote the retrospective with nothing and
	// answered 200 OK. Days of writing, gone, with the audit line recording a
	// perfectly ordinary postmortem save. Clearing the text is still possible
	// and still easy; it just has to be said out loud, as {"body": ""}.
	Body *string `json:"body"`
	// Published is a POINTER for the same reason Body is, and leaving it as a
	// plain bool while fixing Body was half a fix.
	//
	// The endpoint then had MIXED semantics: an omitted body meant "leave the
	// text alone" while an omitted published meant "unpublish". So the natural
	// partial edit — PUT {"body": "typo fixed"} — decoded published:false and
	// took a live retrospective off the public page, with 200 OK and an audit
	// line that reads like an ordinary save. Exactly the failure the Body
	// pointer was introduced to remove, on the field next to it.
	Published *bool `json:"published"`
}

// handleSetIncidentPostmortem writes (and optionally publishes) an incident's
// retrospective.
//
// It is a separate endpoint rather than two more fields on handleUpdateIncident
// for the reason SetStatusPageLogo is separate from UpdateStatusPage: that
// handler is full-replace, so a client that loaded the incident before the
// retrospective existed would erase it by saving a title change. Here the
// post-mortem is the only thing that moves.
func (s *server) handleSetIncidentPostmortem(w http.ResponseWriter, r *http.Request) {
	existing, ok := s.incidentForPage(w, r)
	if !ok {
		return
	}
	var req postmortemRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// An omitted field keeps whatever is stored. Either one can be sent alone:
	// {"body": …} is a text edit that leaves visibility alone, {"published": …}
	// is a toggle that leaves the text alone.
	body := strings.TrimSpace(existing.Postmortem)
	if req.Body != nil {
		body = strings.TrimSpace(*req.Body)
		// The cap applies to what the REQUEST is writing, not to what is already
		// stored. Checking the resolved value meant an over-cap row — written
		// before the cap existed, or through psql — could not even be UNPUBLISHED
		// any more: every toggle-only request 400d, so the only way to take the
		// document down was to destroy its text.
		if len(body) > maxPostmortemBytes {
			writeProblemDetail(w, http.StatusBadRequest,
				fmt.Sprintf("postmortem must be at most %d bytes", maxPostmortemBytes))
			return
		}
	}

	publish := existing.PostmortemPublishedAt != nil
	if req.Published != nil {
		publish = *req.Published
	}

	var publishedAt *time.Time
	if publish {
		// Publishing an empty retrospective would put an empty section on the
		// public page. Clearing one is done with published=false and an empty
		// body, which is a different request and reads as a different intent.
		if body == "" {
			writeProblemDetail(w, http.StatusBadRequest, "cannot publish an empty postmortem")
			return
		}
		// A retrospective on an ongoing incident is not a retrospective. Drafting
		// one early is allowed — that is often when the notes get written — but
		// it cannot go public until the incident is actually closed.
		//
		// Only the nil→published TRANSITION is gated. Applying it unconditionally
		// also blocked EDITING a retrospective that was already public, which
		// happens the moment a resolved incident recurs: the publish button is
		// disabled in that state, so the only save left was the one labelled
		// "Unpublish", and correcting a typo meant taking the document down.
		if existing.PostmortemPublishedAt == nil && existing.Status != IncidentResolved {
			writeProblemDetail(w, http.StatusConflict,
				"the incident must be resolved before its postmortem can be published")
			return
		}
		// An already-published retrospective keeps its original timestamp. The
		// public page shows it as "posted on …", and a typo fix must not move
		// the outage's write-up to today.
		if existing.PostmortemPublishedAt != nil {
			publishedAt = existing.PostmortemPublishedAt
		} else {
			now := s.now()
			publishedAt = &now
		}
	}

	updated, err := s.store.SetIncidentPostmortem(r.Context(), existing.ID, body, publishedAt)
	if storeError(w, err) {
		return
	}
	// Announced like every other change to an incident: a status page open in a
	// browser must not keep showing an incident with no retrospective after one
	// was published. announceIncident applies the public projection itself.
	//
	// Only when the PUBLIC view actually changed, though. publicCopy strips an
	// unpublished retrospective, so a draft save broadcasts an identical
	// incident to every anonymous visitor of the page and makes them re-render
	// for nothing — a poke on every keystroke-driven autosave, telling them the
	// draft exists. Publishing changed it; unpublishing changed it; editing a
	// draft did not.
	wasPublic := existing.PostmortemPublishedAt != nil
	// The guard is evaluated BEFORE the query, not inside the if-statement's
	// initialiser. Written the other way round, the page read still ran on every
	// keystroke-driven draft save — which is the cost the guard exists to avoid.
	if publishedAt == nil && !wasPublic {
		// Nothing an anonymous visitor can see has changed.
	} else if page, perr := s.store.GetStatusPage(r.Context(), updated.StatusPageID); perr == nil {
		verb := "Postmortem withdrawn"
		if publishedAt != nil {
			verb = "Postmortem published"
		}
		s.announceIncident(page, updated, verb)
	}
	pr := principalFrom(r)
	s.audit(r.Context(), &pr.userID, "incident.postmortem", "incident", &updated.ID, clientIP(r),
		map[string]any{"published": publishedAt != nil, "bytes": len(body)})
	writeJSON(w, http.StatusOK, updated)
}

// announceIncident broadcasts an incident change to public WS subscribers
// (best-effort, non-blocking on failures).
//
// The projection happens HERE, not at the call sites. status-page:{slug} is a
// public topic — anonymous visitors of the status page subscribe to it — and
// every one of the five callers passes the incident it happens to be holding.
// One of them forgetting to strip an unpublished retrospective would broadcast a
// draft to the internet, so no caller is given the chance: publicCopy is applied
// on the way out, once.
func (s *server) announceIncident(page StatusPage, in Incident, verb string) {
	if s.wsPub != nil {
		s.wsPub.PublishStatusPage(page.Slug, "incident", map[string]any{
			"verb":     verb,
			"incident": in.publicCopy(),
			"at":       s.now().UnixMilli(),
		})
	}
}
