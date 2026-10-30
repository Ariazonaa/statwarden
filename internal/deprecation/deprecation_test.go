package deprecation

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestMarkSetsTheStandardHeaders(t *testing.T) {
	h := http.Header{}
	Mark(h)

	if got := h.Get("Deprecation"); got != "true" {
		t.Errorf("Deprecation = %q, want %q", got, "true")
	}
	if got := h.Get("Link"); !strings.Contains(got, `rel="deprecation"`) {
		t.Errorf("Link = %q, want a rel=\"deprecation\" link", got)
	}
	if got := h.Get("Link"); !strings.Contains(got, "deployment.md") {
		t.Errorf("Link = %q, should point at the migration notes", got)
	}
}

// The Sunset value has to be an IMF-fixdate, which is what RFC 8594 requires and
// what a client will try to parse. A hand-written string whose weekday disagreed
// with its date would parse as garbage or not at all, which is why the header is
// formatted from a time.Time rather than written out.
func TestSunsetHeaderIsAParsableHTTPDate(t *testing.T) {
	h := http.Header{}
	Mark(h)

	raw := h.Get("Sunset")
	parsed, err := http.ParseTime(raw)
	if err != nil {
		t.Fatalf("Sunset = %q is not a valid HTTP date: %v", raw, err)
	}
	if !parsed.Equal(Sunset) {
		t.Fatalf("Sunset header = %s, want %s", parsed, Sunset)
	}
	if !strings.HasSuffix(raw, "GMT") {
		t.Errorf("Sunset = %q, want an IMF-fixdate ending in GMT", raw)
	}
}

// A tripwire, on purpose.
//
// Announcing a sunset date and then sailing past it is worse than never
// announcing one: the header keeps promising a removal that is not coming, and
// every caller who migrated on schedule did so for nothing. When this fails, the
// decision in #84 is due — either remove the query parameters (the point of the
// exercise) or move the date and say why.
func TestSunsetHasNotPassed(t *testing.T) {
	if !time.Now().Before(Sunset) {
		t.Fatalf("the announced sunset (%s) has passed: either remove the ?token= "+
			"and ?password= parameters now, or move the date deliberately — leaving "+
			"it in the past makes the header a promise the code is not keeping",
			Sunset.Format(time.RFC3339))
	}
}
