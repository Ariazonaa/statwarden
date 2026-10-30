package api

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strconv"
	"time"

	"github.com/Ariazonaa/statwarden/internal/auth"
	"github.com/Ariazonaa/statwarden/internal/domain"
)

// monitorSecretPlaceholder is the sentinel a redacted secret field is replaced
// with in read responses. A write that submits it back for a secret field
// means "leave the stored value unchanged" — handleUpdateMonitor restores the
// stored value rather than persisting the placeholder.
const monitorSecretPlaceholder = "__redacted__"

// secretConfig is the optional interface a monitor checker implements to name
// the JSON config fields that carry credentials. Each monitor type declares it
// in its config.go; the API reaches it through the registry, so it never has
// to import the individual type packages.
type secretConfig interface {
	Secrets() []string
}

// monitorSecretFields returns the secret JSON field names for a monitor type,
// or nil when the type declares none.
func (s *server) monitorSecretFields(typ string) []string {
	c, ok := s.monReg.Get(typ)
	if !ok {
		return nil
	}
	sc, ok := c.(secretConfig)
	if !ok {
		return nil
	}
	return sc.Secrets()
}

// decryptMonitorConfig opens a sealed config envelope back to plaintext. With
// no SecretsBox (DEV_MODE) or a plaintext config it returns the input
// unchanged, mirroring the notification read path.
func (s *server) decryptMonitorConfig(cfg json.RawMessage) json.RawMessage {
	if s.box == nil || len(cfg) == 0 {
		return cfg
	}
	if plain, err := s.box.DecryptConfig(cfg); err == nil {
		return plain
	}
	return cfg
}

// redactMonitorConfig makes a PLAINTEXT monitor config safe to hand out, so read
// APIs never give a viewer or a read-only key the stored credentials.
//
// Two different treatments, because two different kinds of field:
//   - every secret field is replaced with the placeholder wholesale. Empty and
//     absent ones are left untouched — there is nothing to hide, and masking
//     them would falsely imply a value is set.
//   - "url" keeps its shape and loses only an inline password, because it is
//     also the monitor's identity on screen (see maskURLPassword).
func (s *server) redactMonitorConfig(typ string, cfg json.RawMessage) json.RawMessage {
	if len(cfg) == 0 {
		return cfg
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(cfg, &m); err != nil {
		return cfg
	}
	placeholder, _ := json.Marshal(monitorSecretPlaceholder)
	changed := false
	for _, f := range s.monitorSecretFields(typ) {
		raw, ok := m[f]
		if !ok || isEmptyJSON(raw) {
			continue
		}
		m[f] = placeholder
		changed = true
	}
	// url is deliberately NOT a secret field — it is what the monitor is, and
	// blanking it would leave the reader with no idea what is being checked — but
	// it can still carry a password inline. The write path refuses new ones; this
	// covers the rows written before it did. Unconditional on type, because
	// "url" means the same thing in every checker that has one.
	if masked, ok := maskConfigURL(m["url"]); ok {
		m["url"] = masked
		changed = true
	}
	if !changed {
		return cfg
	}
	out, err := json.Marshal(m)
	if err != nil {
		return cfg
	}
	return out
}

// maskConfigURL masks the password in a config's "url" value, reporting whether
// anything changed. A missing, non-string or credential-free url is left alone,
// so a config that never carried one is returned byte-for-byte.
func maskConfigURL(raw json.RawMessage) (json.RawMessage, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var target string
	if err := json.Unmarshal(raw, &target); err != nil || target == "" {
		return nil, false
	}
	masked := maskURLPassword(target)
	if masked == target {
		return nil, false
	}
	out, err := json.Marshal(masked)
	if err != nil {
		return nil, false
	}
	return out, true
}

// maskURLPassword replaces the password in a URL's userinfo with the redaction
// placeholder, leaving everything else — scheme, username, host, path, query —
// readable.
//
// Only the password. The username stays because this value IS the monitor's
// identity on screen; a wholesale redaction the way proxy_url gets one would
// leave a reader unable to tell which target a row even describes. It is a
// deliberately smaller claim than proxy_url's: this hides the secret, it does
// not hide that a credential is configured or who it belongs to.
//
// A URL that does not parse is returned unchanged rather than blanked: the write
// path rejects those, so an unparseable value here is legacy data, and showing
// it is what lets an operator recognise and fix it. It cannot have been split
// into userinfo by a parser that refused to parse it.
func maskURLPassword(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	if _, hasPassword := u.User.Password(); !hasPassword {
		return raw
	}
	u.User = url.UserPassword(u.User.Username(), monitorSecretPlaceholder)
	return u.String()
}

// targetFields name the config keys that decide WHERE a check connects.
//
// They are the fields that make a preserved credential dangerous: a caller who
// cannot read a stored password can still submit the placeholder for it, and if
// they may also change the destination in the same request, the server obligingly
// sends that password somewhere they control. Redaction then protects the value
// on screen and hands it over on the wire.
//
// Matched by name across every checker rather than declared per type. A new
// checker that invents its own name for "where to connect" is not covered, which
// is why redactedSecretsMovedTarget errs towards refusing: an unrecognised
// change plus a placeholder is still a request to re-type the credential, and
// the cost of being wrong is one extra password prompt.
var targetFields = []string{
	"url", "host", "port", "endpoint", "broker", "server",
	"target", "resolver", "database", "proxy_url", "topic", "container",
}

// movedTarget reports whether a write changes where the monitor connects.
func movedTarget(oldCfg, newCfg map[string]json.RawMessage) bool {
	for _, f := range targetFields {
		o, hadOld := oldCfg[f]
		n, hasNew := newCfg[f]
		if hadOld != hasNew {
			return true
		}
		if hadOld && !bytes.Equal(o, n) {
			return true
		}
	}
	return false
}

// keepRedactedSecrets restores stored secret values for the fields a write left
// as the redaction placeholder, so editing a monitor without re-typing its
// credentials does not wipe them. oldCfg is the stored config in PLAINTEXT. A
// placeholder for a field with no stored value drops the field entirely.
//
// It returns ok=false when the write submits a placeholder AND moves the
// target, which the caller must refuse rather than quietly accept.
func (s *server) keepRedactedSecrets(typ string, newCfg, oldCfg json.RawMessage) (json.RawMessage, bool) {
	fields := s.monitorSecretFields(typ)
	if len(fields) == 0 || len(newCfg) == 0 {
		return newCfg, true
	}
	var nm map[string]json.RawMessage
	if err := json.Unmarshal(newCfg, &nm); err != nil {
		return newCfg, true
	}
	var om map[string]json.RawMessage
	_ = json.Unmarshal(oldCfg, &om)
	moved := movedTarget(om, nm)
	changed := false
	for _, f := range fields {
		raw, ok := nm[f]
		if !ok || !isPlaceholder(raw) {
			continue
		}
		// A placeholder means "keep what is stored". Honouring it while the
		// caller ALSO changes where the check connects would send a credential
		// they cannot read to a destination they chose. Refused, and said so:
		// dropping the field silently would leave a monitor whose credential
		// has quietly become empty, which fails later and elsewhere.
		if moved && len(om) > 0 {
			return newCfg, false
		}
		if ov, has := om[f]; has {
			nm[f] = ov
		} else {
			delete(nm, f)
		}
		changed = true
	}
	if !changed {
		return newCfg, true
	}
	out, err := json.Marshal(nm)
	if err != nil {
		return newCfg, true
	}
	return out, true
}

// placeholderFields lists the secret fields a config submits as the redaction
// sentinel. Empty when there are none.
func (s *server) placeholderFields(typ string, cfg json.RawMessage) []string {
	fields := s.monitorSecretFields(typ)
	if len(fields) == 0 || len(cfg) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(cfg, &m); err != nil {
		return nil
	}
	var out []string
	for _, f := range fields {
		if raw, ok := m[f]; ok && isPlaceholder(raw) {
			out = append(out, f)
		}
	}
	return out
}

// isPlaceholder reports whether a raw JSON value is the redaction sentinel
// string.
func isPlaceholder(raw json.RawMessage) bool {
	var s string
	return json.Unmarshal(raw, &s) == nil && s == monitorSecretPlaceholder
}

// isEmptyJSON reports whether a raw JSON value is null or the empty string,
// neither of which carries a secret worth masking.
func isEmptyJSON(raw json.RawMessage) bool {
	switch string(bytes.TrimSpace(raw)) {
	case "null", `""`, "":
		return true
	}
	return false
}

// roleOf normalises a role string to an auth.Role, defaulting unknown values
// to viewer (least privilege).
func roleOf(s string) auth.Role {
	r := auth.Role(s)
	if !r.Valid() {
		return auth.RoleViewer
	}
	return r
}

// monitorFromInput builds a domain.Monitor from a create/update input,
// widening the per-second integer columns to time.Duration as the engine
// expects.
func monitorFromInput(id int64, in MonitorInput) domain.Monitor {
	m := domain.Monitor{
		ID:             id,
		Name:           in.Name,
		Type:           in.Type,
		Config:         json.RawMessage(in.Config),
		Interval:       time.Duration(in.Interval) * time.Second,
		MaxRetries:     in.MaxRetries,
		RecoveryChecks: in.RecoveryChecks,
		RetryInterval:  time.Duration(in.RetryInterval) * time.Second,
		ResendInterval: time.Duration(in.ResendInterval) * time.Minute,
		UpsideDown:     in.UpsideDown,
		Active:         in.Active,
		Status:         domain.StatusPending,
		ParentID:       in.ParentID,
		GroupID:        in.GroupID,
		Description:    in.Description,
		LinkURL:        in.LinkURL,
	}
	if in.Timeout != nil {
		m.Timeout = time.Duration(*in.Timeout) * time.Second
	}
	if !in.Active {
		m.Status = domain.StatusPaused
	}
	if len(m.Config) == 0 {
		m.Config = json.RawMessage(`{}`)
	}
	return m
}

// monitorView is the JSON shape of a monitor returned by the API: durations are
// re-narrowed to the integer seconds/minutes the client sends and stores.
//
// It is the READ counterpart of monitorRequest and must stay a superset of it:
// tag_ids and notification_ids were write-only for a long time, so a client
// that had just saved a monitor read its own associations back as undefined.
// The trailing block is derived state (MonitorStats) rather than columns; it is
// filled by decorateMonitor and is absent from the write DTO on purpose.
type monitorView struct {
	ID              int64           `json:"id"`
	Name            string          `json:"name"`
	Type            string          `json:"type"`
	Config          json.RawMessage `json:"config"`
	Interval        int             `json:"interval_seconds"`
	Timeout         *int            `json:"timeout_seconds"`
	MaxRetries      int             `json:"max_retries"`
	RecoveryChecks  int             `json:"recovery_checks"`
	RetryInterval   int             `json:"retry_interval_seconds"`
	ResendInterval  int             `json:"resend_interval_minutes"`
	UpsideDown      bool            `json:"upside_down"`
	Active          bool            `json:"active"`
	Status          string          `json:"status"`
	ParentID        *int64          `json:"parent_id"`
	GroupID         *int64          `json:"group_id"`
	PushToken       string          `json:"push_token,omitempty"`
	Description     string          `json:"description"`
	LinkURL         string          `json:"link_url"`
	TagIDs          []int64         `json:"tag_ids"`
	NotificationIDs []int64         `json:"notification_ids"`
	// URL is the check target lifted out of Config for display. Config stays
	// the single source of truth — this field is read-only and is ignored on
	// write — but every consumer would otherwise re-implement the same
	// per-type guess, so it is resolved once, here.
	URL       string    `json:"url"`
	CreatedAt time.Time `json:"created_at"`

	LatencyMs     *int       `json:"latency_ms"`
	LastCheckedAt *time.Time `json:"last_checked_at"`
	Uptime24h     *float64   `json:"uptime_24h"`
	Uptime30d     *float64   `json:"uptime_30d"`
	Uptime1y      *float64   `json:"uptime_1y"`
	Sparkline     []int      `json:"sparkline"`
	RecentBeats   []string   `json:"recent_beats"`

	// SchedulingError is set when the write succeeded but the scheduler refused
	// the monitor, so the row says active while nothing is checking it. Absent on
	// every normal response.
	//
	// handleResumeMonitor states the principle — answering success here "is how a
	// monitor comes to look healthy in the UI while nothing watches it" — and
	// reports a 500, which is safe because resuming is idempotent. Create cannot
	// do that: the row already exists, so a 5xx invites a retry that produces a
	// duplicate monitor. Carrying the reason in the body says the same thing
	// without lying about whether the resource was created.
	SchedulingError string `json:"scheduling_error,omitempty"`
}

// safeMonitorView is toMonitorView plus the two redactions every read path owes
// the caller, in the right order.
//
// It exists because toMonitorView copies the config and the push token
// VERBATIM, and forgetting either afterwards is a silent leak rather than a
// compile error — which is exactly what happened when the recycle-bin endpoint
// was added and called toMonitorView directly. toMonitorView is the raw mapper
// and is not safe to serialise on its own.
//
// One handler is still allowed to use it: handleCreateMonitor, which reveals the
// push token exactly once and has already opened the config itself. That is the
// ONLY exception, it is commented at the call site, and anything else reaching
// for the raw mapper is a bug — so if you are here because you added a handler,
// the answer is safeMonitorView (or decorateMonitors, which wraps it).
//
// Order matters: the config is unsealed FIRST so redactMonitorConfig can see the
// field names it is redacting (it works on plaintext), and the push token comes
// out unconditionally — it is the credential that authorises heartbeats for this
// monitor, revealed once at creation and never again through a read.
func (s *server) safeMonitorView(m domain.Monitor) monitorView {
	m.Config = s.decryptMonitorConfig(m.Config)
	v := toMonitorView(m)
	v.Config = s.redactMonitorConfig(m.Type, v.Config)
	v.PushToken = ""
	return v
}

func toMonitorView(m domain.Monitor) monitorView {
	v := monitorView{
		ID:              m.ID,
		Name:            m.Name,
		Type:            m.Type,
		Config:          m.Config,
		Interval:        int(m.Interval / time.Second),
		MaxRetries:      m.MaxRetries,
		RecoveryChecks:  recoveryChecksOrDefault(m.RecoveryChecks),
		RetryInterval:   int(m.RetryInterval / time.Second),
		ResendInterval:  int(m.ResendInterval / time.Minute),
		UpsideDown:      m.UpsideDown,
		Active:          m.Active,
		Status:          string(m.Status),
		ParentID:        m.ParentID,
		GroupID:         m.GroupID,
		PushToken:       m.PushToken,
		Description:     m.Description,
		LinkURL:         m.LinkURL,
		TagIDs:          []int64{},
		NotificationIDs: []int64{},
		URL:             monitorTargetURL(m.Config),
		CreatedAt:       m.CreatedAt,
		Sparkline:       []int{},
		RecentBeats:     []string{},
	}
	if m.Timeout > 0 {
		t := int(m.Timeout / time.Second)
		v.Timeout = &t
	}
	return v
}

// monitorTargetURL lifts the check target out of a monitor config. HTTP-family
// checks carry a full "url"; the socket-family ones carry "host" (plus an
// optional "port"), which is rendered host:port. Anything else has no
// meaningful single target and yields "".
func monitorTargetURL(config json.RawMessage) string {
	var c struct {
		URL  string `json:"url"`
		Host string `json:"host"`
		Port int    `json:"port"`
	}
	if err := json.Unmarshal(config, &c); err != nil {
		return ""
	}
	if c.URL != "" {
		// The view is built from the PLAINTEXT config, before redactMonitorConfig
		// runs on config, so this second copy of the target needs the same
		// masking or the credential simply leaves by the other door.
		return maskURLPassword(c.URL)
	}
	if c.Host == "" {
		return ""
	}
	if c.Port > 0 {
		return c.Host + ":" + strconv.Itoa(c.Port)
	}
	return c.Host
}

// applyMonitorLinks copies a monitor's association ids onto its view, keeping
// the JSON arrays empty rather than null so the client never has to null-check.
func applyMonitorLinks(v monitorView, l MonitorLinks) monitorView {
	if l.TagIDs != nil {
		v.TagIDs = l.TagIDs
	}
	if l.NotificationIDs != nil {
		v.NotificationIDs = l.NotificationIDs
	}
	return v
}

// applyMonitorStats copies the heartbeat-derived summary onto a monitor view.
func applyMonitorStats(v monitorView, st MonitorStats) monitorView {
	v.LatencyMs = st.LatencyMs
	v.LastCheckedAt = st.LastCheckedAt
	v.Uptime24h = st.Uptime24h
	v.Uptime30d = st.Uptime30d
	v.Uptime1y = st.Uptime1y
	if st.Sparkline != nil {
		v.Sparkline = st.Sparkline
	}
	if st.RecentBeats != nil {
		v.RecentBeats = st.RecentBeats
	}
	return v
}
