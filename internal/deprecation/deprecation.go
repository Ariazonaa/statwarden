// Package deprecation holds the sunset date for credentials passed in a URL and
// stamps the standard headers on any response that accepted one.
//
// It is a package rather than a helper in each caller because the date is the
// part that matters and there are two endpoints announcing it — the metrics
// scrape and the public status pages. Two copies of a date drift, and a sunset
// that two endpoints disagree about is worse than none: an operator reads one,
// plans for it, and is broken by the other.
package deprecation

import (
	"net/http"
	"time"
)

// Sunset is when `?token=` and `?password=` stop being accepted.
//
// Both are credentials in a URL, so they end up in browser history, `Referer`
// headers, reverse-proxy access logs and Prometheus's own target metadata —
// several systems keeping a copy of a secret, none of them the one that owns it.
// Removing them is the better posture and is not free: it breaks existing scrape
// configurations and any bookmarked or embedded status-page link that carries the
// password. Announcing the date first is what turns that from an outage into a
// migration, which is why the parameters still work today.
//
// Deliberately far out. An operator has to notice the header, find the callers
// and change them, and the callers are often config in someone else's repository.
var Sunset = time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)

// docsURL is where the migration is written down. Linked from the response so the
// header is actionable on its own, without the reader having to guess.
const docsURL = "https://github.com/Ariazonaa/statwarden/blob/main/docs/deployment.md#credentials-in-urls"

// Mark stamps the deprecation headers on a response that accepted a credential
// from the query string: `Deprecation`, RFC 8594 `Sunset`, and a `Link` to the
// migration notes.
//
// The request still succeeds. The headers are the whole mechanism — a machine
// client can act on them, and a human reading verbose curl output sees the date
// without having to be told.
//
// Call it before anything writes the response body or status.
func Mark(h http.Header) {
	h.Set("Deprecation", "true")
	// http.TimeFormat is the IMF-fixdate RFC 8594 requires, and it derives the
	// weekday rather than trusting a hand-written string to agree with the date.
	h.Set("Sunset", Sunset.Format(http.TimeFormat))
	h.Add("Link", "<"+docsURL+`>; rel="deprecation"`)
}
