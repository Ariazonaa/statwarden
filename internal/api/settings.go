package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/go-chi/chi/v5"
)

func (s *server) handleListSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := s.store.ListSettings(r.Context())
	if storeError(w, err) {
		return
	}
	for i := range settings {
		settings[i] = maskSecretSetting(settings[i])
	}
	writeJSON(w, http.StatusOK, settings)
}

// settingValidators gates the keys whose value the server itself interprets.
// A key without an entry is stored as-is, which is the existing behaviour for
// everything the frontend owns; only keys that can break the running system by
// holding nonsense are checked here.
//
// Validation at write time is not sufficient on its own — the settings table is
// also writable directly in psql — so each of these has a matching clamp on the
// read path. This layer exists to give an admin an error instead of a silently
// ignored value.
var settingValidators = map[string]func(json.RawMessage) error{
	settingSessionTTLDays: func(raw json.RawMessage) error {
		if _, ok := parseSessionTTLDays(raw); !ok {
			return fmt.Errorf("must be a whole number of days between %d and %d",
				int(minSessionTTL.Hours()/24), int(maxSessionTTL.Hours()/24))
		}
		return nil
	},
	settingRetentionDays: func(raw json.RawMessage) error {
		if _, ok := parseRetentionDays(raw); !ok {
			return fmt.Errorf("must be a whole number of days between %d and %d",
				minRetentionDays, maxRetentionDays)
		}
		return nil
	},
	SettingAuditWebhookURL:       validateWebhookURL,
	SettingOutboundWebhookURL:    validateWebhookURL,
	SettingAuditWebhookSecret:    validateWebhookSecret,
	SettingOutboundWebhookSecret: validateWebhookSecret,
}

// Webhook URL settings. Both are operator-supplied endpoints the server POSTs
// JSON to: one per monitor state change (internal/outbound), one per audit entry
// (internal/siem). They are listed together because they are the same kind of
// value and are validated by the same function — leaving one of a matched pair
// unchecked is how the two drift.
//
// They are exported, unlike the other setting keys here, because the process
// wiring in cmd/statwarden reads them to build the two delivery queues. A key name
// spelled one way here and another way there is a feature that is silently off
// forever: the settings screen writes a row nothing ever reads, and there is no
// error anywhere to notice.
const (
	SettingAuditWebhookURL    = "audit_webhook_url"
	SettingOutboundWebhookURL = "outbound_webhook_url"
)

// Webhook signing secrets, one per feed: the shared key a receiver uses to
// verify the X-Statwarden-Signature HMAC on each POST (internal/webhookq). Separate
// keys rather than one, because the two feeds usually land in different systems
// owned by different people, and one leaked key must not forge the other feed.
//
// They are write-only through the API: sealed with the secrets box on write
// (the same ENCRYPTION_KEY envelope as monitor and channel credentials) and
// reported by the settings list only as set or not — see maskSecretSetting.
const (
	SettingAuditWebhookSecret    = "audit_webhook_secret"
	SettingOutboundWebhookSecret = "outbound_webhook_secret"
)

// Bounds for a webhook signing secret. The floor rules out the short strings
// people type as a placeholder, not entropy as such — a generated secret is the
// expected input, and the settings screen offers to generate one.
const (
	minWebhookSecretLen = 16
	maxWebhookSecretLen = 256
)

// isSecretSetting reports whether a key holds a secret that must never be read
// back through the API.
func isSecretSetting(key string) bool {
	return key == SettingAuditWebhookSecret || key == SettingOutboundWebhookSecret
}

// maskSecretSetting replaces a secret row's value with whether it is set. The
// ciphertext is not a secret in the cryptographic sense, but returning it would
// still hand an admin session — or anything that can ride one — material that
// has no use in a browser, and "is a secret configured?" is all the screen needs.
func maskSecretSetting(st Setting) Setting {
	if !isSecretSetting(st.Key) {
		return st
	}
	set := len(st.Value) > 0 && string(st.Value) != `""` && string(st.Value) != "null"
	st.Value = json.RawMessage(strconv.FormatBool(set))
	return st
}

// validateWebhookSecret accepts an empty string (signing off) or a string of
// minWebhookSecretLen..maxWebhookSecretLen printable, non-space characters.
func validateWebhookSecret(raw json.RawMessage) error {
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return fmt.Errorf("must be a string")
	}
	return CheckWebhookSecret(v)
}

// CheckWebhookSecret is the read-path clamp for the signing secrets and the body
// of the write validator, for the same reason CheckWebhookURL exists: the
// settings table is writable in psql too.
func CheckWebhookSecret(v string) error {
	if v == "" {
		return nil
	}
	if n := len(v); n < minWebhookSecretLen || n > maxWebhookSecretLen {
		return fmt.Errorf("must be between %d and %d characters", minWebhookSecretLen, maxWebhookSecretLen)
	}
	for _, r := range v {
		if r <= ' ' || r == 0x7f {
			return fmt.Errorf("must not contain spaces or control characters")
		}
	}
	return nil
}

// validateWebhookURL accepts an empty string (the disabled state) or an absolute
// http/https URL with a host.
//
// It is a shape check, not a safety check, and deliberately so. This is
// self-hosted software configured by an admin, who can already point the
// notification webhook provider anywhere; refusing private addresses here would
// break the common deployment where the collector is another container on the
// same network, without preventing anything that role cannot already do. What it
// does prevent is a value that can never work: net/http rejects a non-http
// scheme at send time, so without this the operator saves a `file://` or a bare
// hostname, sees no error, and only finds out that nothing is being forwarded
// when they go looking for events that never arrived.
func validateWebhookURL(raw json.RawMessage) error {
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return fmt.Errorf("must be a string")
	}
	return CheckWebhookURL(v)
}

// CheckWebhookURL is the READ-PATH clamp for the two webhook URL settings, and
// the body of the write-path validator above.
//
// Every other validated setting in this file has one — it is the invariant
// stated at the top of settingValidators — and these two were the pair that did
// not, which is the shape of the drift that comment warns about. The settings
// table is writable directly in psql, so write-time validation is a courtesy to
// an admin using the UI, not a guarantee. Without the clamp a row containing
// `file:///etc/passwd` or a bare hostname reached net/http once per event, and
// the operator saw a delivery error per state change rather than one line
// saying the setting is wrong.
//
// An empty string is the disabled state and is valid.
func CheckWebhookURL(v string) error {
	if v == "" {
		return nil
	}
	u, err := url.Parse(v)
	if err != nil {
		return fmt.Errorf("must be a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("must be an http:// or https:// URL")
	}
	if u.Host == "" {
		return fmt.Errorf("must include a host")
	}
	return nil
}

// settingRetentionDays is the settings row an admin uses to set the heartbeat
// retention window at runtime; the heartbeat-pruning job reads it each pass and
// deletes beats older than it. The bounds mirror config.MinHeartbeatRetentionDays
// / config.MaxHeartbeatRetentionDays and, like the session-TTL row, are enforced
// both here on write and again where the job reads the value, because the
// settings table is also writable directly in psql.
const (
	settingRetentionDays = "retention_days"
	minRetentionDays     = 1
	maxRetentionDays     = 3650 // ~10 years
)

// parseRetentionDays interprets a stored retention_days value. It reports
// ok=false for anything that is not a whole number of days inside the permitted
// range — the same check the write validator and (in spirit) the pruner's clamp
// apply, so the surfaces cannot disagree about what a valid window is.
func parseRetentionDays(raw json.RawMessage) (int, bool) {
	var days float64
	if err := json.Unmarshal(raw, &days); err != nil {
		return 0, false
	}
	// Reject a fractional value rather than truncating it: 0.5 would silently
	// become 0 days, i.e. "delete every heartbeat".
	if days != float64(int64(days)) {
		return 0, false
	}
	d := int(days)
	if d < minRetentionDays || d > maxRetentionDays {
		return 0, false
	}
	return d, true
}

// RuntimeRetentionDays reads the operator's heartbeat-retention override.
//
// This is the READ half that the settings screen has been promising since the
// row existed. `retention_days` was validated on write, stored, mapped by the
// client and rendered as a pick-list whose help text says "Lowering this deletes
// older data on the next run and cannot be undone" — and nothing anywhere read
// it back. retention.Options.DaysFunc, the hook built for exactly this, was set
// by no caller, so the pruner used HEARTBEAT_RETENTION_DAYS forever and an
// admin's choice did nothing at all, in either direction.
//
// It returns (0, nil) when there is genuinely no override — the key is absent,
// or holds a value outside the permitted bounds, in which case the env default
// is the right answer. It returns an ERROR when the row could not be read at
// all, which is NOT the same thing and must not be flattened into it.
//
// The difference is destructive. The whole point of the control is to run a
// LONGER window than the env default, so falling back on a failed read means
// deleting everything between the two — an operator on HEARTBEAT_RETENTION_DAYS=90
// who raised retention to a year loses 275 days of history to one timed-out
// query, irreversibly, with the log line reporting 90 as if it were configured.
// The caller skips the pass instead.
func RuntimeRetentionDays(ctx context.Context, store Store) (int, error) {
	s, err := store.GetSetting(ctx, settingRetentionDays)
	if errors.Is(err, ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	days, ok := parseRetentionDays(s.Value)
	if !ok {
		// Stored but unusable (a psql write outside the bounds, a non-number).
		// The env default is the safe reading of "no valid override".
		return 0, nil
	}
	return days, nil
}

func (s *server) handleSetSetting(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	if key == "" {
		writeProblemDetail(w, http.StatusBadRequest, "key is required")
		return
	}
	var value json.RawMessage
	if !decodeJSON(w, r, &value) {
		return
	}
	if validate, ok := settingValidators[key]; ok {
		if err := validate(value); err != nil {
			writeProblemDetail(w, http.StatusBadRequest, key+": "+err.Error())
			return
		}
	}
	if isSecretSetting(key) && s.box != nil && string(value) != `""` {
		// Sealed at rest like every other credential. Fail closed: a seal error
		// must not fall through to storing the key in plaintext.
		sealed, err := s.box.EncryptConfig(value)
		if err != nil {
			writeProblemDetail(w, http.StatusInternalServerError, "could not seal the secret")
			return
		}
		value = sealed
	}
	setting, err := s.store.SetSetting(r.Context(), key, value)
	if storeError(w, err) {
		return
	}
	p := principalFrom(r)
	s.audit(r.Context(), &p.userID, "setting.update", "setting", nil, clientIP(r), map[string]string{"key": key})
	writeJSON(w, http.StatusOK, maskSecretSetting(setting))
}
