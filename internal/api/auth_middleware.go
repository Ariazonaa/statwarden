package api

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/Ariazonaa/statwarden/internal/auth"
)

// Role thresholds used by the route table.
var (
	roleWriter = auth.RoleUser  // create/edit monitors, tags, notifications, …
	roleAdmin  = auth.RoleAdmin // users, settings
)

// SecretsBox is the notification-config sealer the API needs. *secrets.Box
// satisfies it. When nil, configs are stored as plaintext (DEV_MODE).
type SecretsBox interface {
	EncryptConfig(cfg json.RawMessage) (json.RawMessage, error)
	DecryptConfig(cfg json.RawMessage) (json.RawMessage, error)
}

// authenticate resolves the request principal from an API-key bearer token or
// a session cookie and attaches it to the context. It never rejects: it only
// identifies. requireAuth / requireRole enforce access.
func (s *server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := s.resolvePrincipal(r); p != nil {
			if p.slid {
				s.reissueSessionCookies(w, r)
			}
			r = r.WithContext(withPrincipal(r.Context(), p))
		}
		next.ServeHTTP(w, r)
	})
}

// reissueSessionCookies re-sends the auth cookies with a fresh Max-Age after
// the server has extended the session.
//
// Sliding expiry was only half implemented: the row's expires_at moved on every
// request, and the browser was never told. So the cookie was deleted at the
// Max-Age set when the user logged in, and an active user was signed out mid-use
// while the server still considered the session perfectly valid — the one
// outcome "sliding" is supposed to prevent.
//
// Both values are taken from the request, so nothing is minted here: the same
// session token and the same CSRF token, with a new lifetime. Re-issuing the
// CSRF cookie with its existing value matters, because it shares the session's
// Max-Age and would otherwise expire underneath a session that has not.
func (s *server) reissueSessionCookies(w http.ResponseWriter, r *http.Request) {
	ttl := s.effectiveSessionTTL(r.Context())
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		http.SetCookie(w, s.authCookie(sessionCookie, c.Value, true, ttl))
	}
	if c, err := r.Cookie(csrfCookie); err == nil && c.Value != "" {
		http.SetCookie(w, s.authCookie(csrfCookie, c.Value, false, ttl))
	}
}

func (s *server) resolvePrincipal(r *http.Request) *principal {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return s.principalFromAPIKey(r, strings.TrimSpace(strings.TrimPrefix(h, "Bearer ")))
	}
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		return s.principalFromSession(r, c.Value)
	}
	return nil
}

func (s *server) principalFromAPIKey(r *http.Request, token string) *principal {
	if token == "" {
		return nil
	}
	key, err := s.store.GetAPIKeyByHash(r.Context(), auth.HashToken(token))
	if err != nil {
		return nil
	}
	if key.ExpiresAt != nil && s.now().After(*key.ExpiresAt) {
		return nil
	}
	u, err := s.store.GetUserByID(r.Context(), key.UserID)
	if err != nil || !u.IsActive {
		return nil
	}
	// Best-effort last-used tracking; a write failure must not fail the request.
	_ = s.store.TouchAPIKey(r.Context(), key.ID, s.now())
	return &principal{kind: principalAPIKey, userID: u.ID, role: u.Role, scopes: key.Scopes}
}

func (s *server) principalFromSession(r *http.Request, token string) *principal {
	id := auth.HashToken(token)
	sess, err := s.store.GetSession(r.Context(), id)
	if err != nil {
		return nil
	}
	now := s.now()
	if now.After(sess.ExpiresAt) {
		_ = s.store.DeleteSession(r.Context(), id)
		return nil
	}
	u, err := s.store.GetUserByID(r.Context(), sess.UserID)
	if err != nil || !u.IsActive {
		return nil
	}
	// Sliding expiry: extend the window on activity, throttled to once a minute.
	// The TTL is resolved here rather than captured at startup so a shortened
	// lifetime reaches an active session within a minute of its next request.
	// The once-a-minute throttle already bounds the extra settings read.
	slid := false
	if now.Sub(sess.LastSeenAt) > time.Minute {
		_ = s.store.TouchSession(r.Context(), id, now, now.Add(s.effectiveSessionTTL(r.Context())))
		// Tell the caller so the cookies can be re-sent with the new lifetime.
		// Throttled to the same once-a-minute cadence as the write, so a busy
		// client is not handed a Set-Cookie on every single response.
		slid = true
	}
	return &principal{kind: principalSession, userID: u.ID, role: u.Role, sessionID: id, slid: slid}
}

// grantsScope reports whether the caller may hand scope to a new API key. A key
// is never allowed to mint a key more powerful than itself, and a cookie session
// is bounded by its role. Without this cap a read-only key could POST
// /me/api-keys asking for write or admin scope and walk straight out of its own
// restrictions — the self-service mint route requires no scope of its own.
func (p *principal) grantsScope(scope string) bool {
	switch p.kind {
	case principalAPIKey:
		// hasScope already treats an admin-scoped key as holding every scope.
		return p.hasScope(scope)
	case principalSession:
		switch scope {
		case ScopeWrite:
			return p.role.AtLeast(roleWriter)
		case ScopeAdmin:
			return p.role.AtLeast(roleAdmin)
		default: // ScopeRead
			return true
		}
	}
	return false
}

// requireAuth rejects anonymous requests. Read-scoped API keys satisfy it.
func (s *server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := principalFrom(r)
		if p == nil {
			writeProblemDetail(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if p.kind == principalAPIKey && !p.hasScope(ScopeRead) && !p.hasScope(ScopeWrite) {
			writeProblemDetail(w, http.StatusForbidden, "API key lacks the required scope")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireSession rejects API-key principals outright.
//
// It guards account-credential management: e-mail, password, 2FA and the
// session and API-key lists. Those routes sit outside requireRole — a viewer
// owns their own profile — so the only gate they had was requireAuth, which a
// READ-scoped key satisfies. Combined with the CSRF exemption for API keys
// (there is no cookie to double-submit), that turned a stolen read-only key
// into a full account takeover: change the address, trigger a password reset to
// it, then disable 2FA and mint further keys. The documented contract is "read
// gates GET endpoints", and this is what makes that true.
//
// Requiring an interactive session rather than merely a write scope is
// deliberate. An API key is for automation; rotating its owner's second factor
// is not automation, and a key that can mint keys is a persistence primitive.
func (s *server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := principalFrom(r)
		if p == nil {
			writeProblemDetail(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if p.kind != principalSession {
			writeProblemDetail(w, http.StatusForbidden,
				"account credentials can only be managed from an interactive session, not with an API key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireWriteScope gates a self-service mutation that is not credential
// management (currently the avatar). A session always passes; an API key needs
// the write scope, so a read-only key cannot mutate anything at all.
func (s *server) requireWriteScope(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := principalFrom(r)
		if p == nil {
			writeProblemDetail(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if p.kind == principalAPIKey && !p.hasScope(ScopeWrite) {
			writeProblemDetail(w, http.StatusForbidden, "API key lacks the required scope")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireRole guards mutating routes: the principal's role must meet min and,
// for API keys, the matching scope (write for user-level, admin for admin).
func (s *server) requireRole(min auth.Role) func(http.Handler) http.Handler {
	scope := ScopeWrite
	if min == auth.RoleAdmin {
		scope = ScopeAdmin
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := principalFrom(r)
			if p == nil {
				writeProblemDetail(w, http.StatusUnauthorized, "authentication required")
				return
			}
			if !p.role.AtLeast(min) {
				writeProblemDetail(w, http.StatusForbidden, "insufficient role")
				return
			}
			if p.kind == principalAPIKey && !p.hasScope(scope) {
				writeProblemDetail(w, http.StatusForbidden, "API key lacks the required scope")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// csrf enforces double-submit CSRF protection on cookie-session mutations. The
// browser echoes the non-HttpOnly csrf cookie in the X-CSRF-Token header; a
// cross-site page can send the cookie but cannot read it to set the header.
// API-key principals (no cookie) are exempt.
func (s *server) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := principalFrom(r)
		if p != nil && p.kind == principalAPIKey {
			next.ServeHTTP(w, r)
			return
		}
		cookie, err := r.Cookie(csrfCookie)
		header := r.Header.Get(csrfHeader)
		if err != nil || cookie.Value == "" || header == "" ||
			subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(header)) != 1 {
			writeProblemDetail(w, http.StatusForbidden, "missing or invalid CSRF token")
			return
		}
		next.ServeHTTP(w, r)
	})
}
