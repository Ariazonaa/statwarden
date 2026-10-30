package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Ariazonaa/statwarden/internal/auth"
	"github.com/Ariazonaa/statwarden/internal/badge"
	"github.com/Ariazonaa/statwarden/internal/report"
)

// writeSVG writes an SVG badge with short caching (badges change slowly and are
// embedded widely).
func writeSVG(w http.ResponseWriter, svg string) {
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "max-age=60, public")
	// The monitor name reaches this string, and SVG served under image/svg+xml
	// executes script when navigated to directly — so the escaping is the whole
	// defence, and it lives at the top of badge.render: both label and value go
	// through escape() (& < > " ') before any Fprintf. Taint analysis cannot see
	// through that. If render ever stops escaping, this becomes stored XSS on a
	// same-origin URL.
	_, _ = w.Write([]byte(svg)) //nolint:gosec // G705: badge.render escapes label and value
}

// badgeMonitorID resolves the opaque badge token in the request path to the
// monitor it addresses. The badge endpoints are addressed by this unguessable,
// revocable token rather than by the monitor's sequential id, so they cannot be
// walked to enumerate every monitor in the instance. An unknown or revoked
// token is a 404, indistinguishable from a token that never existed.
func (s *server) badgeMonitorID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	token := chi.URLParam(r, "token")
	if token == "" {
		writeProblemDetail(w, http.StatusNotFound, "unknown badge")
		return 0, false
	}
	id, err := s.store.MonitorIDByBadgeToken(r.Context(), token)
	if storeError(w, err) {
		return 0, false
	}
	return id, true
}

// handleStatusBadge renders a monitor's current status as an SVG badge. Public,
// addressed by an opaque badge token.
func (s *server) handleStatusBadge(w http.ResponseWriter, r *http.Request) {
	id, ok := s.badgeMonitorID(w, r)
	if !ok {
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
	label := r.URL.Query().Get("label")
	if label == "" {
		label = "status"
	}
	writeSVG(w, badge.Status(label, status))
}

// handleUptimeBadge renders a monitor's uptime percentage over a range as an
// SVG badge. Public, addressed by an opaque badge token. Falls back to an "n/a"
// badge when reporting is unavailable or has no data.
func (s *server) handleUptimeBadge(w http.ResponseWriter, r *http.Request) {
	id, ok := s.badgeMonitorID(w, r)
	if !ok {
		return
	}
	if _, err := s.store.GetMonitor(r.Context(), id); storeError(w, err) {
		return
	}
	label := r.URL.Query().Get("label")
	if label == "" {
		label = "uptime"
	}
	pct := -1.0
	if s.reporter != nil {
		// A badge is an embedded image on someone else's page, so a bad ?from=
		// falls back to the default window rather than answering an error: a
		// problem+json body would render as a broken image and tell the viewer
		// nothing. The other report endpoints, which have a real client, do
		// return 400.
		rng, err := s.parseReportRange(r)
		if err != nil || rng.From.IsZero() {
			rng = report.Range{From: s.now().Add(-30 * 24 * time.Hour), To: s.now()}
		}
		if res, err := s.reporter.Uptime(r.Context(), id, rng); err == nil && res.Uptime >= 0 {
			pct = res.Uptime * 100
		}
	}
	writeSVG(w, badge.Uptime(label, pct))
}

// badgeURL renders the absolute badge URL for a token and kind ("status" or
// "uptime") when a public base URL is configured, otherwise a relative path.
func (s *server) badgeURL(token, kind string) string {
	path := "/api/v1/badge/" + token + "/" + kind + ".svg"
	if s.baseURL == "" {
		return path
	}
	return strings.TrimRight(s.baseURL, "/") + path
}

// badgeResponse renders the token plus both ready-to-embed badge URLs.
func (s *server) badgeResponse(token string) map[string]any {
	return map[string]any{
		"badge_token": token,
		"status_url":  s.badgeURL(token, "status"),
		"uptime_url":  s.badgeURL(token, "uptime"),
	}
}

// handleCreateMonitorBadge mints (or returns the existing) opaque badge token
// for a monitor and answers with the embeddable badge URLs. Writer, CSRF. It is
// idempotent: a monitor already carrying a badge keeps the same token, so the
// URLs stay stable.
func (s *server) handleCreateMonitorBadge(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	if _, err := s.store.GetMonitor(r.Context(), id); storeError(w, err) {
		return
	}
	candidate, err := auth.NewToken()
	if err != nil {
		writeProblemDetail(w, http.StatusInternalServerError, "generate token")
		return
	}
	token, err := s.store.EnsureBadgeToken(r.Context(), id, candidate)
	if storeError(w, err) {
		return
	}
	p := principalFrom(r)
	s.audit(r.Context(), &p.userID, "monitor.badge_token", "monitor", &id, clientIP(r), nil)
	writeJSON(w, http.StatusOK, s.badgeResponse(token))
}

// handleRevokeMonitorBadge revokes a monitor's badge token so its badge URLs
// stop resolving. Writer, CSRF. Idempotent.
func (s *server) handleRevokeMonitorBadge(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	if _, err := s.store.GetMonitor(r.Context(), id); storeError(w, err) {
		return
	}
	if err := s.store.DeleteBadgeToken(r.Context(), id); storeError(w, err) {
		return
	}
	p := principalFrom(r)
	s.audit(r.Context(), &p.userID, "monitor.badge_revoke", "monitor", &id, clientIP(r), nil)
	w.WriteHeader(http.StatusNoContent)
}
