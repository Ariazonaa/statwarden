// Package redact strips credentials out of the errors that outbound HTTP
// integrations return, before those errors reach a log line, the delivery
// audit log or an API response.
//
// The credential is rarely in a header. Telegram puts the bot token in the
// request path, Slack, Discord and Teams put the webhook secret there, an ntfy
// topic IS the path, and some integrations take an API key as a query
// parameter. net/http reports every failed request as a *url.Error, whose
// message quotes the full request URL — so a timeout to a chat service wrote the
// bot token into the log, into notification_deliveries.error, and back to
// whoever pressed "send test". Only scheme and host are kept: they are what an
// operator needs to tell which integration failed, and they are not secret.
package redact

import (
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// removed stands in for whatever was cut from a URL, so a reader can see that
// something was there.
const removed = "[redacted]"

// URL reduces raw to scheme://host, dropping userinfo, path, query and
// fragment. A value that does not parse as an absolute URL is replaced
// entirely: it cannot be split safely, and it may well be the secret itself.
func URL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "[redacted URL]"
	}
	safe := u.Scheme + "://" + u.Host
	if u.User != nil || strings.Trim(u.Path, "/") != "" || u.RawQuery != "" ||
		u.Fragment != "" || u.Opaque != "" {
		safe += "/" + removed
	}
	return safe
}

// Error returns err with every request URL it mentions reduced by URL.
//
// The URLs come from two places: each *url.Error in the chain, which is how
// net/http reports a failed request or an unparsable one, and extra — URLs the
// caller knows were in play, for a provider that formatted one into its own
// message. Error returns err itself when there was nothing to remove.
//
// The result keeps err as its cause, so errors.Is and errors.As still answer
// "was this a timeout". Only the message is rewritten, which is the part that
// gets written down; a caller that digs the original *url.Error back out has
// asked for it.
func Error(err error, extra ...string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	candidates := append(urlsIn(err), extra...)
	// Longest first: a URL that is a prefix of another would otherwise be
	// replaced inside it and leave the longer one's tail — its secret part —
	// behind.
	slices.SortFunc(candidates, func(a, b string) int { return len(b) - len(a) })
	out := msg
	for _, raw := range candidates {
		if raw != "" {
			out = replaceURL(out, raw)
		}
	}
	if out == msg {
		return err
	}
	return &redacted{msg: out, cause: err}
}

// replaceURL swaps raw for its redacted form in s, both as written and in the
// Go-quoted form *url.Error.Error uses (%q), which differs from the raw text
// whenever the URL holds a character that needs escaping.
func replaceURL(s, raw string) string {
	safe := URL(raw)
	if safe == raw {
		return s
	}
	if q := strconv.Quote(raw); len(q) >= 2 {
		s = strings.ReplaceAll(s, q[1:len(q)-1], safe)
	}
	return strings.ReplaceAll(s, raw, safe)
}

// urlsIn collects the URL of every *url.Error in err's tree.
func urlsIn(err error) []string {
	var out []string
	var walk func(error)
	walk = func(e error) {
		switch x := e.(type) {
		case nil:
		case *url.Error:
			out = append(out, x.URL)
			walk(x.Err)
		case interface{ Unwrap() error }:
			walk(x.Unwrap())
		case interface{ Unwrap() []error }:
			for _, inner := range x.Unwrap() {
				walk(inner)
			}
		}
	}
	walk(err)
	return out
}

// redacted is an error whose message has been cleaned but whose chain is
// intact.
type redacted struct {
	msg   string
	cause error
}

func (r *redacted) Error() string { return r.msg }
func (r *redacted) Unwrap() error { return r.cause }
