package importexport

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// KumaResult is the outcome of mapping a Uptime-Kuma export.
type KumaResult struct {
	Monitors      []MonitorSpec      `json:"monitors"`
	Notifications []NotificationSpec `json:"notifications"`
	// Tags are the source tags carried over (populated by the DB importer;
	// the JSON export path does not include them).
	Tags []TagSpec `json:"tags,omitempty"`
	// Warnings collects human-readable notes about monitors/fields that were
	// skipped or only partially mapped.
	Warnings []string `json:"warnings,omitempty"`
}

// kumaExport is the subset of a Uptime-Kuma backup JSON we read. Kuma has used
// both "monitorList"/"notificationList" (older) and "monitors"/"notifications"
// keys; we accept either.
type kumaExport struct {
	Version          string          `json:"version"`
	MonitorList      json.RawMessage `json:"monitorList"`
	Monitors         json.RawMessage `json:"monitors"`
	NotificationList json.RawMessage `json:"notificationList"`
	Notifications    json.RawMessage `json:"notifications"`
}

type kumaMonitor struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	URL      string `json:"url"`
	Hostname string `json:"hostname"`
	Port     int    `json:"port"`
	Interval int    `json:"interval"`
	Retries  int    `json:"maxretries"`
	Method   string `json:"method"`
	Keyword  string `json:"keyword"`
	Active   *bool  `json:"active"`
	// UpsideDown is Kuma's "upsideDown" invert flag.
	UpsideDown bool `json:"upsideDown"`
	// DNS resolve type / record type.
	DNSResolveType string `json:"dns_resolve_type"`
	// IgnoreTLS skips certificate verification (Kuma "ignoreTls").
	IgnoreTLS bool `json:"ignoreTls"`
	// AcceptedStatusCodes lists the HTTP codes/ranges counting as UP
	// (Kuma "accepted_statuscodes", e.g. ["200-299","401"]).
	AcceptedStatusCodes []string `json:"accepted_statuscodes"`
	// PushToken is the token of a Kuma push monitor. Statwarden mints its own on
	// create, so this is informational only.
	PushToken string `json:"pushToken"`
	// Tags carried from the source (populated by the DB importer).
	Tags []string `json:"-"`
	// Notifications are the channel names alerting on this monitor
	// (populated by the DB importer from monitor_notification).
	Notifications []string `json:"-"`
}

type kumaNotification struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Active *bool  `json:"active"`
}

// monitorTypeMap maps Kuma monitor types to Statwarden registry types. Types not
// present here are reported as unmapped.
var monitorTypeMap = map[string]string{
	"http":         "http",
	"keyword":      "http",
	"json-query":   "http",
	"port":         "tcp",
	"tcp":          "tcp",
	"ping":         "ping",
	"dns":          "dns",
	"push":         "push",
	"grpc-keyword": "grpc",
	"mqtt":         "mqtt",
	"docker":       "docker",
	"redis":        "redis",
	"postgres":     "postgres",
	"mysql":        "mysql",
	"mongodb":      "mongo",
	"radius":       "radius",
	"steam":        "steam",
}

// notificationTypeMap maps Kuma notification provider names to Statwarden provider
// names. Unmapped providers are imported as inactive generic webhooks stubs is
// avoided — instead they are reported as warnings.
var notificationTypeMap = map[string]string{
	"slack":       "slack",
	"discord":     "discord",
	"telegram":    "telegram",
	"teams":       "teams",
	"webhook":     "webhook",
	"smtp":        "smtp",
	"ntfy":        "ntfy",
	"pushover":    "pushover",
	"gotify":      "gotify",
	"matrix":      "matrix",
	"mattermost":  "mattermost",
	"rocket.chat": "rocketchat",
	"signal":      "signal",
	"PagerDuty":   "pagerduty",
	"Opsgenie":    "opsgenie",
	"octopush":    "", // unsupported
}

// MapKuma parses a Uptime-Kuma export and maps it onto Statwarden specs, on a
// best-effort basis. It never fails on individual unmappable monitors; those
// are skipped and reported in Warnings.
func MapKuma(raw []byte) (KumaResult, error) {
	var exp kumaExport
	if err := json.Unmarshal(raw, &exp); err != nil {
		return KumaResult{}, fmt.Errorf("kuma import: invalid JSON: %w", err)
	}
	monitorsRaw := exp.MonitorList
	if len(monitorsRaw) == 0 {
		monitorsRaw = exp.Monitors
	}
	notifsRaw := exp.NotificationList
	if len(notifsRaw) == 0 {
		notifsRaw = exp.Notifications
	}

	var res KumaResult
	res.Monitors, res.Warnings = mapKumaMonitors(monitorsRaw)
	notifs, notifWarn := mapKumaNotifications(notifsRaw)
	res.Notifications = notifs
	res.Warnings = append(res.Warnings, notifWarn...)
	return res, nil
}

func mapKumaMonitors(raw json.RawMessage) ([]MonitorSpec, []string) {
	if len(raw) == 0 {
		return nil, nil
	}
	// Kuma exports monitors either as an array or as an object keyed by id.
	var list []kumaMonitor
	if err := json.Unmarshal(raw, &list); err != nil {
		var byID map[string]kumaMonitor
		if err2 := json.Unmarshal(raw, &byID); err2 != nil {
			return nil, []string{"could not parse Kuma monitor list"}
		}
		keys := make([]string, 0, len(byID))
		for k := range byID {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			list = append(list, byID[k])
		}
	}

	return mapKumaMonitorList(list)
}

// mapKumaMonitorList maps parsed Kuma monitors onto Statwarden specs. Both the JSON
// export path and the SQLite database path feed through here, so the field
// translation lives in exactly one place.
func mapKumaMonitorList(list []kumaMonitor) ([]MonitorSpec, []string) {
	var out []MonitorSpec
	var warnings []string
	for _, km := range list {
		utype, ok := monitorTypeMap[km.Type]
		if !ok {
			warnings = append(warnings, fmt.Sprintf("monitor %q: unsupported Kuma type %q, skipped", km.Name, km.Type))
			continue
		}
		spec := MonitorSpec{
			Name:            km.Name,
			Type:            utype,
			IntervalSeconds: orDefault(km.Interval, 60),
			MaxRetries:      km.Retries,
			RetryInterval:   orDefault(km.Interval, 60),
			UpsideDown:      km.UpsideDown,
			Active:          km.Active == nil || *km.Active,
			Tags:            km.Tags,
			Notifications:   km.Notifications,
		}
		cfg, unmapped := kumaMonitorConfig(km, utype)
		spec.Config = cfg
		spec.Unmapped = unmapped
		out = append(out, spec)
	}
	return out, warnings
}

// kumaMonitorConfig builds the Statwarden jsonb config for a mapped monitor and
// reports Kuma fields that were not carried over.
func kumaMonitorConfig(km kumaMonitor, utype string) (json.RawMessage, []string) {
	var unmapped []string
	m := map[string]any{}
	switch utype {
	case "http":
		m["url"] = km.URL
		if km.Method != "" && km.Method != "GET" {
			m["method"] = km.Method
		}
		if km.Keyword != "" {
			m["keyword"] = km.Keyword
		}
		if len(km.AcceptedStatusCodes) > 0 {
			m["accepted_status_codes"] = km.AcceptedStatusCodes
		}
		if km.IgnoreTLS {
			m["ignore_tls_errors"] = true
		}
	case "tcp":
		m["host"] = firstNonEmpty(km.Hostname, km.URL)
		m["port"] = km.Port
	case "ping":
		m["host"] = firstNonEmpty(km.Hostname, km.URL)
	case "dns":
		m["host"] = firstNonEmpty(km.Hostname, km.URL)
		if km.DNSResolveType != "" {
			m["record_type"] = km.DNSResolveType
		}
	case "push":
		m["expected_interval_seconds"] = orDefault(km.Interval, 60)
		m["grace_period_seconds"] = 0
	default:
		// Generic best-effort: carry hostname/port/url when present.
		if km.Hostname != "" {
			m["host"] = km.Hostname
		}
		if km.Port != 0 {
			m["port"] = km.Port
		}
		if km.URL != "" {
			m["url"] = km.URL
		}
		unmapped = append(unmapped, "config: mapped generically; review required")
	}
	raw, _ := json.Marshal(m)
	return raw, unmapped
}

func mapKumaNotifications(raw json.RawMessage) ([]NotificationSpec, []string) {
	if len(raw) == 0 {
		return nil, nil
	}
	var list []kumaNotification
	if err := json.Unmarshal(raw, &list); err != nil {
		var byID map[string]kumaNotification
		if err2 := json.Unmarshal(raw, &byID); err2 != nil {
			return nil, []string{"could not parse Kuma notification list"}
		}
		for _, v := range byID {
			list = append(list, v)
		}
	}
	var out []NotificationSpec
	var warnings []string
	for _, kn := range list {
		utype, ok := notificationTypeMap[kn.Type]
		if !ok || utype == "" {
			warnings = append(warnings, fmt.Sprintf("notification %q: unsupported Kuma provider %q, skipped", kn.Name, kn.Type))
			continue
		}
		out = append(out, NotificationSpec{
			Name:     kn.Name,
			Type:     utype,
			Active:   kn.Active == nil || *kn.Active,
			Config:   json.RawMessage(`{}`),
			Unmapped: []string{"config: Kuma stores channel secrets separately; re-enter credentials"},
		})
	}
	return out, warnings
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
