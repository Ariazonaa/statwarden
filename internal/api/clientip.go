package api

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// realIP resolves the client address once per request and stashes it on the
// context, where clientIP reads it back. It replaces chi's middleware.RealIP,
// which rewrites RemoteAddr from the *leftmost* X-Forwarded-For entry
// unconditionally -- a value any client can set, which would hand every
// attacker a fresh rate-limit and lockout bucket per request.
func (s *server) realIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := s.resolveClientIP(r)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientIPKey, ip)))
	})
}

// resolveClientIP determines the address to attribute a request to.
//
// X-Forwarded-For is consulted only when the immediate peer -- the untamperable
// RemoteAddr -- is itself inside a trusted CIDR. The chain is then walked from
// the right: each trusted proxy appends the peer it saw, so the rightmost entry
// that is not itself a trusted proxy is the last address a proxy we trust
// actually observed. Everything to its left was supplied by the client and is
// worthless. With no trusted proxies configured the peer address is always used
// verbatim, which is the safe default for a direct-to-internet deployment.
func (s *server) resolveClientIP(r *http.Request) string {
	peer := remoteIP(r)
	if len(s.trustedProxies) == 0 {
		return peer
	}
	addr, err := parseIP(peer)
	if err != nil || !inAnyPrefix(addr, s.trustedProxies) {
		return peer
	}
	// Multiple X-Forwarded-For headers concatenate in order, so walk the
	// headers and their comma-separated entries back to front together.
	headers := r.Header.Values("X-Forwarded-For")
	for i := len(headers) - 1; i >= 0; i-- {
		parts := strings.Split(headers[i], ",")
		for j := len(parts) - 1; j >= 0; j-- {
			hop, err := parseIP(strings.TrimSpace(parts[j]))
			if err != nil {
				// A malformed hop makes everything further left unverifiable;
				// fall back to the closest address we know to be genuine.
				return peer
			}
			if inAnyPrefix(hop, s.trustedProxies) {
				continue
			}
			return hop.Unmap().String()
		}
	}
	// Every hop was a trusted proxy (or there was no header at all): the peer
	// is the best answer available.
	return peer
}

// remoteIP returns the peer address of the connection without its port.
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// parseIP accepts a bare address, an IPv6 zone-scoped address or a host:port
// pair (some proxies append ports to X-Forwarded-For entries).
func parseIP(raw string) (netip.Addr, error) {
	if addr, err := netip.ParseAddr(raw); err == nil {
		return addr.WithZone(""), nil
	}
	host, _, err := net.SplitHostPort(raw)
	if err != nil {
		return netip.Addr{}, err
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, err
	}
	return addr.WithZone(""), nil
}

// inAnyPrefix reports whether addr falls inside one of the prefixes. IPv4
// addresses arriving in their IPv4-mapped IPv6 form are unmapped first so they
// match plain IPv4 CIDRs.
func inAnyPrefix(addr netip.Addr, prefixes []netip.Prefix) bool {
	addr = addr.Unmap()
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
