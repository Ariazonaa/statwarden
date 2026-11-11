// Package importexport defines Statwarden's configuration export/import bundle and
// the Uptime-Kuma importer. The bundle types are transport-neutral so the API
// layer can round-trip monitors, notifications, tags and status pages; the
// Kuma importer maps a Kuma export JSON onto the same neutral monitor and
// notification specs on a best-effort basis, reporting fields it could not map.
package importexport

import "encoding/json"

// BundleVersion is the current export schema version.
//
// 2 added the status-page protection flag and layout, and monitor tags,
// channels, parent and folder. It was bumped rather
// than left additive on purpose: a version-1 reader ignores has_password and
// would import a protected page as public, so an older server has to refuse
// these bundles. Version-1 bundles are still read.
const BundleVersion = 2

// Bundle is a full configuration export.
type Bundle struct {
	Version       int                `json:"version"`
	ExportedAt    string             `json:"exported_at"`
	Monitors      []MonitorSpec      `json:"monitors"`
	Notifications []NotificationSpec `json:"notifications"`
	Tags          []TagSpec          `json:"tags"`
	StatusPages   []StatusPageSpec   `json:"status_pages"`
	// MonitorGroups are the organisational folders MonitorSpec.Group names.
	MonitorGroups []MonitorGroupSpec `json:"monitor_groups,omitempty"`
}

// MonitorGroupSpec is a portable monitor folder.
type MonitorGroupSpec struct {
	Name      string `json:"name"`
	SortOrder int    `json:"sort_order"`
}

// MonitorSpec is a portable monitor definition.
type MonitorSpec struct {
	Name            string          `json:"name"`
	Type            string          `json:"type"`
	Config          json.RawMessage `json:"config,omitempty"`
	IntervalSeconds int             `json:"interval_seconds"`
	Timeout         *int            `json:"timeout_seconds,omitempty"`
	MaxRetries      int             `json:"max_retries"`
	// RecoveryChecks is omitted from bundles written before it existed; the
	// importer then stores the single-success default.
	RecoveryChecks int      `json:"recovery_checks,omitempty"`
	RetryInterval  int      `json:"retry_interval_seconds"`
	ResendInterval int      `json:"resend_interval_minutes"`
	UpsideDown     bool     `json:"upside_down"`
	Active         bool     `json:"active"`
	Description    string   `json:"description,omitempty"`
	LinkURL        string   `json:"link_url,omitempty"`
	Tags           []string `json:"tags,omitempty"`
	// Notifications names the channels that alert on this monitor. applyBundle
	// resolves them to ids and wires them up (Kuma import carries the source
	// monitor↔notification assignment).
	Notifications []string `json:"notifications,omitempty"`
	// Parent names the monitor this one depends on, and Group the folder it is
	// filed under. Both are names, resolved on import like Tags.
	Parent string `json:"parent,omitempty"`
	Group  string `json:"group,omitempty"`
	// Unmapped lists source fields that could not be translated (Kuma import).
	Unmapped []string `json:"unmapped,omitempty"`
}

// NotificationSpec is a portable notification-channel definition.
type NotificationSpec struct {
	Name      string          `json:"name"`
	Type      string          `json:"type"`
	Config    json.RawMessage `json:"config,omitempty"`
	IsDefault bool            `json:"is_default"`
	Active    bool            `json:"active"`
	Unmapped  []string        `json:"unmapped,omitempty"`
}

// TagSpec is a portable tag definition.
type TagSpec struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

// StatusPageSpec is a portable status-page definition.
type StatusPageSpec struct {
	Slug        string `json:"slug"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	FooterText  string `json:"footer_text,omitempty"`
	Theme       string `json:"theme,omitempty"`
	AccentColor string `json:"accent_color,omitempty"`
	Locale      string `json:"locale,omitempty"`
	Published   bool   `json:"published"`
	// HasPassword records that the page was password-protected. The hash itself
	// is never exported, so the importer cannot restore the protection and
	// creates such a page unpublished instead of public.
	HasPassword   bool                  `json:"has_password,omitempty"`
	AllowIndexing bool                  `json:"allow_indexing"`
	Monitors      []string              `json:"monitors,omitempty"` // monitor names, in display order
	Groups        []StatusPageGroupSpec `json:"groups,omitempty"`
	// Layout places each monitor in a group and carries its uptime visibility.
	// Monitors stays alongside it so a reader that predates Layout still gets
	// the page's monitors; when Layout is present it wins.
	Layout []StatusPageEntrySpec `json:"layout,omitempty"`
}

// StatusPageGroupSpec is a named block of monitors on a status page.
type StatusPageGroupSpec struct {
	Name      string `json:"name"`
	SortOrder int    `json:"sort_order"`
}

// StatusPageEntrySpec places one monitor (by name) on a status page.
type StatusPageEntrySpec struct {
	Monitor    string `json:"monitor"`
	Group      string `json:"group,omitempty"` // group name; empty is ungrouped
	SortOrder  int    `json:"sort_order"`
	ShowUptime bool   `json:"show_uptime"`
}
