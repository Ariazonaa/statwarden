package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Ariazonaa/statwarden/internal/auth"

	"github.com/Ariazonaa/statwarden/internal/httpx"
)

// WSHub is the WebSocket upgrade surface the router mounts on /ws. *ws.Hub
// satisfies it. The api layer resolves session authentication before handing
// the connection to the hub, which then enforces the public-topic rule for
// unauthenticated clients.
type WSHub interface {
	// auth is nil for an anonymous connection. A non-nil auth both marks the
	// connection as authenticated and lets the hub re-prove that session for as
	// long as the socket stays open.
	ServeWS(w http.ResponseWriter, r *http.Request, auth func() bool)
}

// wsAuthCheckTimeout bounds one background session re-check for an open socket.
const wsAuthCheckTimeout = 5 * time.Second

// handleWS upgrades a connection to the WebSocket hub. Authentication is by
// session cookie only (an API key in a query string would leak in logs); an
// anonymous connection is still accepted but limited to public status-page
// topics by the hub.
func (s *server) handleWS(w http.ResponseWriter, r *http.Request) {
	// Reject cross-origin handshakes before adopting any identity. The upgrade
	// carries the caller's cookies, so a page on another origin could otherwise
	// open a socket as the logged-in user and read their private topics (a
	// cross-site WebSocket hijack). The gorilla upgrader's CheckOrigin is
	// permissive, so the origin is enforced here instead.
	if !s.wsOriginAllowed(r) {
		writeProblemDetail(w, http.StatusForbidden, "cross-origin WebSocket connection rejected")
		return
	}
	// The session is resolved once here and then RE-resolved by the hub for as
	// long as the socket lives. Checking only at the handshake meant a logout, an
	// expiry or an admin deactivating the account left an already-open connection
	// reading private monitor and dashboard events until the browser happened to
	// disconnect — potentially for days.
	var revalidate func() bool
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if p := s.principalFromSession(r, c.Value); p != nil {
			token := c.Value
			revalidate = func() bool {
				// context.Background, not r.Context(): the request context is
				// cancelled the moment this handler returns, which is immediately
				// after the upgrade.
				ctx, cancel := context.WithTimeout(context.Background(), wsAuthCheckTimeout)
				defer cancel()
				return s.sessionStillValid(ctx, token)
			}
		}
	}
	s.hub.ServeWS(w, r, revalidate)
}

// sessionStillValid re-resolves a session token WITHOUT the sliding-expiry
// touch principalFromSession performs.
//
// The distinction is deliberate: an open WebSocket would otherwise renew the
// session on a timer, so a forgotten browser tab would keep an account signed
// in indefinitely. A liveness re-check is not user activity.
func (s *server) sessionStillValid(ctx context.Context, token string) bool {
	sess, err := s.store.GetSession(ctx, auth.HashToken(token))
	if err != nil {
		return false
	}
	if s.now().After(sess.ExpiresAt) {
		return false
	}
	u, err := s.store.GetUserByID(ctx, sess.UserID)
	return err == nil && u.IsActive
}

// wsOriginAllowed validates the handshake's Origin header against the configured
// public origin AND any explicitly allowed extra origins — the latter is how a
// deployment reached under more than one hostname lets live updates work on all
// of them. Every reference origin is configured, never read from a request
// header, so this stays a defence against cross-site WebSocket hijacking.
//
// A request with no Origin header is allowed: non-browser clients (CLIs,
// server-to-server) do not send one, and a cross-site hijack requires a browser,
// which always does. When nothing is configured at all the request's own Host is
// used as the reference so a same-origin connection still works in development.
func (s *server) wsOriginAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	refs := make([]string, 0, 1+len(s.allowedOrigins))
	if s.baseURL != "" {
		refs = append(refs, s.baseURL)
	}
	refs = append(refs, s.allowedOrigins...)
	if len(refs) == 0 {
		return strings.EqualFold(u.Host, r.Host)
	}
	// Scheme and port are part of the comparison, not just the host.
	//
	// Comparing hosts alone accepted Origin: http://statwarden.example against
	// PUBLIC_BASE_URL=https://statwarden.example. Without strict HSTS already in
	// force in the victim's browser, an attacker who can answer on plain HTTP
	// for that name — a captive portal, a poisoned DNS answer, any position on
	// the network — gets a page with a trusted origin and opens the socket from
	// it. The point of checking Origin at all is that it cannot be forged; a
	// check that accepts a different scheme forges it for free.
	for _, ref := range refs {
		if ru, err := url.Parse(ref); err == nil && ru.Host != "" && httpx.SameOrigin(u, ru) {
			return true
		}
	}
	return false
}
