// Package httpx holds the HTTP behaviour that has to be identical everywhere
// Statwarden makes an outbound request with a credential attached: the monitor
// checkers, the notification providers and the CLI.
//
// It exists because the rule below was written three times, slightly
// differently, and one of the three was wrong.
package httpx

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SensitiveHeaders is net/http's own strip-on-redirect set.
//
// net/http removes these itself, but only when the redirect leaves the DOMAIN.
// It does not treat an https→http hop as leaving anything, so a target that
// answers a redirect to its own http:// URL is handed the credential in the
// clear — over the same network path the operator chose TLS to avoid. Listing
// them here lets a caller apply the stricter rule in LeavesOrigin.
var SensitiveHeaders = []string{
	"Authorization",
	"Www-Authenticate",
	"Cookie",
	"Cookie2",
	"Proxy-Authorization",
}

// LeavesOrigin reports whether following from → to takes the request outside
// the origin its credentials were configured for.
//
// Three rules. The host rule mirrors net/http's own treatment of
// Authorization: the same host, or a subdomain of it, stays trusted. Two are
// added on top, each covering a hop net/http considers harmless:
//
//   - a downgrade out of https, which sends the credential in the clear over
//     the very network path TLS was chosen to protect;
//   - a change of port, which is a different service on the same machine.
//     net/http ignores the port entirely, so :443 → :8080 counts as staying
//     home. Default ports are normalised, so https://h and https://h:443 are
//     the same origin.
func LeavesOrigin(from, to *url.URL) bool {
	if from == nil || to == nil {
		return true // unknown is not trusted
	}
	if strings.EqualFold(from.Scheme, "https") && !strings.EqualFold(to.Scheme, "https") {
		return true
	}
	if !SameOrSubdomain(to.Hostname(), from.Hostname()) {
		return true
	}
	// An upgrade into https is exempt from the port rule. "http://h → https://h"
	// is the single most common redirect on the web and it changes the
	// effective port from 80 to 443 by definition; refusing it would break
	// ordinary targets to prevent a hop that exposes nothing, since the
	// credential ends up on the more protected leg, not the less.
	if !strings.EqualFold(from.Scheme, "https") && strings.EqualFold(to.Scheme, "https") {
		return false
	}
	return effectivePort(from) != effectivePort(to)
}

// effectivePort is u's port, filling in the scheme default so that
// "https://h" and "https://h:443" compare equal.
func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	default:
		return ""
	}
}

// SameOrSubdomain reports whether host is parent, or a subdomain of it. It
// matches net/http's isDomainOrSubdomain, which is unexported.
func SameOrSubdomain(host, parent string) bool {
	host, parent = strings.ToLower(host), strings.ToLower(parent)
	if host == parent {
		return true
	}
	if len(host) <= len(parent) || !strings.HasSuffix(host, parent) {
		return false
	}
	// "evilexample.com" must not count as a subdomain of "example.com".
	return host[len(host)-len(parent)-1] == '.'
}

// StripCredentialsOnRedirect returns a http.Client.CheckRedirect that removes
// extra alongside net/http's own sensitive set whenever a hop leaves the
// origin, and stops after maxRedirects.
//
// extra is for headers the caller attached itself — an X-Api-Key, a webhook
// signing token — which net/http knows nothing about and therefore replays
// verbatim to whatever host answered the 3xx. A User-Agent in extra is ignored:
// it is not a credential and stripping it only makes requests harder to
// attribute.
func StripCredentialsOnRedirect(maxRedirects int, extra []string) func(*http.Request, []*http.Request) error {
	strip := make([]string, 0, len(SensitiveHeaders)+len(extra))
	strip = append(strip, SensitiveHeaders...)
	for _, name := range extra {
		if !strings.EqualFold(name, "User-Agent") {
			strip = append(strip, name)
		}
	}
	return func(req *http.Request, via []*http.Request) error {
		if maxRedirects > 0 && len(via) >= maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		if len(via) > 0 && LeavesOrigin(via[0].URL, req.URL) {
			for _, name := range strip {
				req.Header.Del(name)
			}
		}
		return nil
	}
}

// DefaultMaxRedirects bounds a delivery's redirect chain. Generous enough for
// the ordinary http→https and bare-domain→www hops a self-hosted endpoint sits
// behind, short enough that a loop is an error rather than a stall.
const DefaultMaxRedirects = 5

// NotifierClient returns an http.Client for POSTing to an endpoint whose URL
// somebody configured, with the credential-stripping redirect policy already in
// place. extra names any credential header the caller attaches itself, which
// net/http cannot know about (Gotify's X-Gotify-Key, for instance).
//
// It exists because every notification provider built its client the same way and
// left CheckRedirect nil, which takes net/http's default: follow up to ten hops,
// stripping the sensitive headers only when the DOMAIN changes. That leaves the
// gap this package was created for — an https→http hop on the same host is not a
// domain change, so a bearer token for a self-hosted Gotify, Matrix or ntfy
// instance was replayed over cleartext. Eighteen providers each getting that right
// by hand is eighteen chances to get it wrong, so the rule lives here and they
// call it.
//
// One caveat, stated rather than papered over: this strips HEADERS. A credential
// carried in the request BODY still travels if the endpoint answers 307 or 308,
// because net/http replays method and body verbatim and there is no header to
// remove. Providers that put a token in the body are bounded by
// DefaultMaxRedirects and by the endpoint being one the operator chose.
func NotifierClient(timeout time.Duration, extra ...string) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: StripCredentialsOnRedirect(DefaultMaxRedirects, extra),
	}
}

// SameOrigin reports whether two URLs are the same origin in the strict sense:
// same scheme, same host, same effective port. Unlike LeavesOrigin it grants no
// subdomain trust and no upgrade exemption — it is for deciding whether a
// browser-supplied Origin header matches a configured one, where "close enough"
// is what cross-site WebSocket hijacking is built on.
func SameOrigin(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	return strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectivePort(a) == effectivePort(b)
}
