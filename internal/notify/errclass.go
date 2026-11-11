package notify

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
)

// ErrUnknownProvider is the delivery error for a channel whose provider type
// is not registered in this build.
var ErrUnknownProvider = errors.New("unknown provider type")

// The coarse failure classes ErrorClass reports. They are part of the API's
// answer to a test send, so they are stable strings rather than Go types.
const (
	// ErrorClassTimeout: the provider did not answer within the send budget.
	ErrorClassTimeout = "timeout"
	// ErrorClassNetwork: the provider could not be reached at all — DNS, a
	// refused connection, a TLS failure.
	ErrorClassNetwork = "network"
	// ErrorClassConfig: the channel cannot be sent through as configured.
	ErrorClassConfig = "config"
	// ErrorClassRejected: the provider answered and refused the message.
	ErrorClassRejected = "rejected"
)

// ErrorClass sorts a delivery error into one of the ErrorClass* buckets.
//
// It exists so a caller that has to show a failure to somebody — the test-send
// endpoint — can say what KIND of failure it was without echoing the provider's
// raw error, which is written for the server log and can quote whatever the
// remote end sent back.
func ErrorClass(err error) string {
	var nerr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded),
		errors.As(err, &nerr) && nerr.Timeout():
		return ErrorClassTimeout
	case errors.Is(err, ErrUnknownProvider):
		return ErrorClassConfig
	}
	var uerr *url.Error
	var oerr *net.OpError
	var derr *net.DNSError
	if errors.As(err, &uerr) || errors.As(err, &oerr) || errors.As(err, &derr) {
		return ErrorClassNetwork
	}
	return ErrorClassRejected
}

// configURLs returns every absolute URL among a channel config's string
// values, at any depth.
//
// It is what lets the dispatcher redact a URL a provider formatted into its own
// error message rather than returning net/http's *url.Error. Collected from the
// config instead of from the message, because the config says exactly which
// strings were in play and a pattern match over free text would not.
func configURLs(cfg json.RawMessage) []string {
	if len(cfg) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(cfg, &v) != nil {
		return nil
	}
	var out []string
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case string:
			if u, err := url.Parse(t); err == nil && u.Scheme != "" && u.Host != "" {
				out = append(out, t)
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(v)
	return out
}
