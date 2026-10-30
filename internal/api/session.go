package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/Ariazonaa/statwarden/internal/auth"
	"github.com/Ariazonaa/statwarden/internal/siem"
)

// issueSession creates a server-side session for userID, sets the opaque
// HttpOnly session cookie and the readable double-submit CSRF cookie, and
// returns the CSRF token (also echoed in the login response body so SPAs can
// seed their header immediately).
func (s *server) issueSession(w http.ResponseWriter, r *http.Request, userID int64) (string, error) {
	token, err := auth.NewToken()
	if err != nil {
		return "", err
	}
	csrfToken, err := auth.NewToken()
	if err != nil {
		return "", err
	}
	now := s.now()
	// Resolved once and reused for the row and both cookies, so a setting that
	// changes mid-request cannot give the cookie and the session different lives.
	ttl := s.effectiveSessionTTL(r.Context())
	sess := Session{
		ID:         auth.HashToken(token),
		UserID:     userID,
		CreatedAt:  now,
		ExpiresAt:  now.Add(ttl),
		LastSeenAt: now,
		IP:         clientIP(r),
		UserAgent:  r.UserAgent(),
	}
	if err := s.store.CreateSession(r.Context(), sess); err != nil {
		return "", err
	}
	http.SetCookie(w, s.authCookie(sessionCookie, token, true, ttl))
	http.SetCookie(w, s.authCookie(csrfCookie, csrfToken, false, ttl))
	return csrfToken, nil
}

// authCookie builds an auth cookie with the hardened flag set. httpOnly is
// false only for the CSRF cookie, which the SPA must read to echo it back.
//
// The Max-Age is only a client-side hint; the server-side ExpiresAt is
// authoritative and is re-checked on every request, so a browser holding a
// cookie whose lifetime an admin has since shortened is harmless.
func (s *server) authCookie(name, value string, httpOnly bool, ttl time.Duration) *http.Cookie {
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: httpOnly,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(ttl.Seconds()),
	}
}

// ceremonyCookie builds a short-lived cookie carrying transient flow state (an
// OIDC state/PKCE bundle). It is separate from authCookie because that one's
// lifetime is the session TTL — a month — and a five-minute ceremony must not
// leave a month-long cookie behind.
//
// SameSite=Lax is required rather than merely chosen: the OIDC callback arrives
// as a top-level GET navigation from the identity provider, which Lax permits
// and Strict would strip the cookie from.
func (s *server) ceremonyCookie(name, value, path string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     path,
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
	}
}

// clearCeremonyCookie expires a flow cookie. It must be called on every terminal
// outcome, success and failure alike, so an abandoned ceremony cannot be resumed.
func (s *server) clearCeremonyCookie(w http.ResponseWriter, name, path string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     path,
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// clearCookie expires an auth cookie.
func (s *server) clearCookie(w http.ResponseWriter, name string, httpOnly bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		HttpOnly: httpOnly,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// clientIP returns the address the request is attributed to for rate limiting,
// lockout and audit purposes. The realIP middleware resolves it once (honouring
// TRUSTED_PROXIES) and puts it on the context; the fallback covers handlers
// exercised without that middleware and never trusts a proxy header.
func clientIP(r *http.Request) string {
	if ip, ok := r.Context().Value(clientIPKey).(string); ok && ip != "" {
		return ip
	}
	return remoteIP(r)
}

// audit writes an audit-log entry, best-effort: a logging failure must never
// fail the underlying action.
//
// It is also the single funnel through which the SIEM feed is fed. Forwarding
// happens AFTER the row is persisted and only when the write succeeded: the
// local audit_log is the record of truth, and a collector must not be told about
// an event this instance has no record of. The forward itself cannot fail here —
// auditSink.HandleAudit only queues — so a slow or dead collector never shows up
// as latency on the request being audited.
func (s *server) audit(ctx context.Context, userID *int64, action, entityType string, entityID *int64, ip string, details any) {
	e := AuditEntry{
		Action:     action,
		EntityType: entityType,
		EntityID:   entityID,
		IP:         ip,
		CreatedAt:  s.now(),
	}
	if userID != nil && *userID != 0 {
		e.UserID = userID
	}
	if details != nil {
		if raw, err := json.Marshal(details); err == nil {
			e.Details = raw
		}
	}
	stored, err := s.store.InsertAudit(ctx, e)
	if err != nil {
		s.logger.Error("audit log write failed", "action", action, "error", err)
		return
	}
	if s.auditSink == nil {
		return
	}
	s.auditSink.HandleAudit(ctx, siem.Event{
		ID:         stored.ID,
		Action:     stored.Action,
		EntityType: stored.EntityType,
		EntityID:   stored.EntityID,
		UserID:     stored.UserID,
		IP:         stored.IP,
		Details:    stored.Details,
		At:         stored.CreatedAt,
	})
}
