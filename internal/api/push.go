package api

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/Ariazonaa/statwarden/internal/monitor/push"
)

// maxPushMessageBytes bounds the free-text msg of a push beat. It is written by
// an unauthenticated caller into every heartbeat row, the live feed and alert
// bodies; a status note does not need more.
const maxPushMessageBytes = 2048

// isPushToken reports whether s has the shape push.GenerateToken mints: 32
// lowercase hex characters. Anything else cannot match a stored hash.
func isPushToken(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// pushMessage makes a caller-supplied msg storable and bounded. PostgreSQL text
// refuses NUL and invalid UTF-8, so either one turned the beat into a 500 and
// the watchdog then saw no beat at all. The cut lands on a rune boundary.
func pushMessage(raw string) string {
	msg := strings.ToValidUTF8(strings.ReplaceAll(raw, "\x00", ""), "�")
	if len(msg) <= maxPushMessageBytes {
		return msg
	}
	cut := maxPushMessageBytes
	for cut > 0 && !utf8.RuneStart(msg[cut]) {
		cut--
	}
	return msg[:cut]
}

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
	// Checked BEFORE the limiter, because the per-token limiter keys on this
	// string: unchecked, every distinct path became its own map entry for a full
	// window, and an anonymous caller could grow that map with keys as long as
	// the request line allows. A malformed token is answered like an unknown one.
	if !isPushToken(token) {
		writeProblemDetail(w, http.StatusNotFound, "unknown push token")
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
	msg := q.Get("msg")
	if msg == "" {
		msg = q.Get("message")
	}
	beat := push.Beat{
		Status:  strings.ToLower(q.Get("status")),
		Message: pushMessage(msg),
	}
	if raw := q.Get("ping"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			// latency_ms is an int4 column and the recorder widens ping to a
			// Duration in milliseconds; past MaxInt32 both wrapped silently.
			n = min(n, math.MaxInt32)
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
