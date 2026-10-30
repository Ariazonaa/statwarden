package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/Ariazonaa/statwarden/internal/monitor/push"
)

// handlePush records an incoming push heartbeat for a passive monitor. It is
// public and rate-limited: the observed service (cron job, backup script, …)
// calls it on a schedule. Status/message/ping are read from query parameters,
// mirroring Uptime-Kuma's push URL contract.
func (s *server) handlePush(w http.ResponseWriter, r *http.Request) {
	if s.push == nil {
		writeProblemDetail(w, http.StatusServiceUnavailable, "push endpoint not available")
		return
	}
	token := chi.URLParam(r, "token")
	if token == "" {
		writeProblemDetail(w, http.StatusBadRequest, "missing push token")
		return
	}
	// Two brakes, and the order matters. The per-IP one is a coarse flood guard
	// set far above any real workload; the per-token one is the actual limit,
	// because a token is one monitor and therefore one reporting cadence.
	//
	// This used to be a single per-IP check on the 60/min public-page budget,
	// which throttled the ordinary case of many push monitors reporting from one
	// egress address. The rejected beats never reach the recorder, so the
	// watchdog trips healthy monitors DOWN — the rate limit manufactured the
	// outage it was supposed to have nothing to do with.
	if !s.pushIPLimiter.allow("push:ip:"+clientIP(r)) ||
		!s.pushLimiter.allow("push:tok:"+token) {
		writeProblemDetail(w, http.StatusTooManyRequests, "too many push requests, slow down")
		return
	}

	q := r.URL.Query()
	beat := push.Beat{
		Status:  strings.ToLower(q.Get("status")),
		Message: q.Get("msg"),
	}
	if beat.Message == "" {
		beat.Message = q.Get("message")
	}
	if raw := q.Get("ping"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			beat.Ping = &n
		}
	}

	m, err := s.push.Record(r.Context(), token, beat)
	if err != nil {
		if errors.Is(err, push.ErrNotFound) {
			writeProblemDetail(w, http.StatusNotFound, "unknown push token")
			return
		}
		writeProblemDetail(w, http.StatusInternalServerError, "record heartbeat")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"monitor_id": m.ID,
		"status":     string(m.Status),
	})
}

// handleRegeneratePushToken issues a fresh push token for a monitor, revoking
// the previous one. It returns the new token and the ready-to-use push URL.
func (s *server) handleRegeneratePushToken(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	m, err := s.store.GetMonitor(r.Context(), id)
	if storeError(w, err) {
		return
	}
	// Minting is restricted to active push monitors: a non-push monitor has no
	// use for a token, and a paused one must not hand out a fresh secret.
	if m.Type != "push" {
		writeProblemDetail(w, http.StatusConflict, "push tokens are only available for push monitors")
		return
	}
	if !m.Active {
		writeProblemDetail(w, http.StatusConflict, "cannot mint a push token for a paused monitor")
		return
	}
	setter, ok := s.store.(pushTokenStore)
	if !ok {
		writeProblemDetail(w, http.StatusNotImplemented, "push tokens not supported by this store")
		return
	}
	token, err := push.GenerateToken()
	if err != nil {
		writeProblemDetail(w, http.StatusInternalServerError, "generate token")
		return
	}
	if err := setter.SetPushToken(r.Context(), id, token); storeError(w, err) {
		return
	}
	p := principalFrom(r)
	s.audit(r.Context(), &p.userID, "monitor.push_token", "monitor", &id, clientIP(r), nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"push_token": token,
		"push_url":   s.pushURL(token),
	})
}

// pushURL renders the absolute push URL for a token when a public base URL is
// configured, otherwise a relative path.
func (s *server) pushURL(token string) string {
	path := "/api/v1/push/" + token
	if s.baseURL == "" {
		return path
	}
	return strings.TrimRight(s.baseURL, "/") + path
}
