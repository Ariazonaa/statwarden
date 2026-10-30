package api

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// The session lifetime has two sources, in this order:
//
//  1. the settings row session_ttl_days, when present and in range — an admin
//     can change it at runtime through the settings UI;
//  2. Options.SessionTTL, fed from SESSION_TTL_DAYS, otherwise.
//
// The environment therefore supplies the default and the database overrides it.
// A missing, malformed or out-of-range row falls back to (2) rather than
// failing: this function runs on the login path and inside the sliding-expiry
// touch, so a transient database error must never be able to invalidate
// authentication.

// parseSessionTTLDays interprets a stored session_ttl_days value. It reports
// ok=false for anything that is not an integral number of days inside the
// permitted range, which is what both the write-side validator and the read-side
// resolver need — sharing one parser is what keeps them from disagreeing.
func parseSessionTTLDays(raw json.RawMessage) (time.Duration, bool) {
	var days float64
	if err := json.Unmarshal(raw, &days); err != nil {
		return 0, false
	}
	// Reject a fractional value rather than truncating it: 0.5 would silently
	// become 0 days, i.e. an instantly-expired session.
	if days != float64(int64(days)) {
		return 0, false
	}
	ttl := time.Duration(int64(days)) * 24 * time.Hour
	if ttl < minSessionTTL || ttl > maxSessionTTL {
		return 0, false
	}
	return ttl, true
}

// effectiveSessionTTL resolves the session lifetime for one request.
//
// The clamp is applied on every read and not only at write time on purpose: the
// settings table is also writable directly in psql, and a database restored from
// elsewhere never passed through the write-side validator. A resolved TTL of
// zero or less would mint sessions whose ExpiresAt is already in the past,
// which principalFromSession deletes on sight — locking every user out,
// including the admin who would have to undo it.
func (s *server) effectiveSessionTTL(ctx context.Context) time.Duration {
	fallback := s.sessionTTL
	if fallback < minSessionTTL {
		fallback = defaultSessionTTL
	}
	if s.store == nil {
		return fallback
	}
	setting, err := s.store.GetSetting(ctx, settingSessionTTLDays)
	if err != nil {
		// ErrNotFound is the normal case (no override configured). Anything else
		// is a store problem, and the fallback is the safe answer either way.
		if !errors.Is(err, ErrNotFound) {
			s.logger.Warn("read session TTL setting", "err", err)
		}
		return fallback
	}
	ttl, ok := parseSessionTTLDays(setting.Value)
	if !ok {
		return fallback
	}
	return ttl
}
