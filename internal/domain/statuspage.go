package domain

import "regexp"

// statusPageSlugRe is the format a status-page slug must match: 1–64 characters
// of lowercase letters, digits, hyphen or underscore.
//
// The alphabet is the one usernameRe already argues for in internal/api: the
// slug is served back verbatim in contexts that each parse it differently, so it
// stays to an unambiguous, URL- and shell-safe set. Concretely it reaches the
// URL path /status/{slug}, the Path attribute of the page unlock cookie, and the
// WebSocket topic name status-page:{slug}. Uppercase is rejected rather than
// folded for the same reason a username is: folding would silently move the
// page's public URL, a 400 says so.
//
// The bound matters as much as the charset. The hub allocates a subscriber map
// per distinct topic name, and an unbounded slug meant an anonymous socket could
// mint arbitrarily many of them.
var statusPageSlugRe = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// ValidStatusPageSlug reports whether s is a well-formed status-page slug.
//
// It lives in domain rather than next to the other API validators because two
// packages must agree on the answer and neither may import the other: internal/api
// enforces it on write, and internal/ws enforces it on the topic name a client
// sends. A hub that trusted the API's enforcement instead of checking would be
// trusting a string that arrives on an unauthenticated socket.
func ValidStatusPageSlug(s string) bool {
	return statusPageSlugRe.MatchString(s)
}
