package api

import (
	"context"
	"net/http"

	"github.com/Ariazonaa/statwarden/internal/auth"
)

// principalKind distinguishes how a request authenticated.
type principalKind int

const (
	principalNone    principalKind = iota
	principalSession               // cookie session
	principalAPIKey                // Authorization: Bearer
)

// principal is the authenticated identity attached to a request context by the
// authenticate middleware.
type principal struct {
	kind      principalKind
	userID    int64
	role      auth.Role
	sessionID string   // set for session principals
	scopes    []string // set for API-key principals
	// slid is true when this request extended the session's expiry, so the
	// middleware knows to re-send the cookies with the new lifetime. Without it
	// the extension only ever reached the database and the browser dropped the
	// cookie at its original Max-Age.
	slid bool
}

// hasScope reports whether an API-key principal carries scope. Session
// principals implicitly have every scope (their role is the real gate).
func (p *principal) hasScope(scope string) bool {
	if p.kind == principalSession {
		return true
	}
	for _, s := range p.scopes {
		if s == scope || s == auth.RoleAdmin.String() {
			return true
		}
	}
	return false
}

type ctxKey int

const (
	principalKey ctxKey = iota
	clientIPKey         // resolved client address, set by the realIP middleware
)

func withPrincipal(ctx context.Context, p *principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

// principalFrom returns the authenticated principal, or nil when the request is
// anonymous.
func principalFrom(r *http.Request) *principal {
	p, _ := r.Context().Value(principalKey).(*principal)
	return p
}
