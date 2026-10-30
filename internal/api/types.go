package api

import (
	"encoding/json"
	"time"

	"github.com/Ariazonaa/statwarden/internal/auth"
)

// User mirrors a users row minus the password hash and TOTP secret, which
// never leave the persistence layer except where a flow explicitly needs them.
type User struct {
	ID          int64     `json:"id"`
	Email       string    `json:"email"`
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name"`
	Role        auth.Role `json:"role"`
	TOTPEnabled bool      `json:"totp_enabled"`
	AvatarPath  string    `json:"avatar_path,omitempty"`
	Locale      string    `json:"locale,omitempty"`
	IsActive    bool      `json:"is_active"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Credentials is the persistence-internal view a login flow needs: the fields
// User omits for safety. It is never serialised to a client.
type Credentials struct {
	User
	PasswordHash string
	TOTPSecret   string
	BackupCodes  []string
}

// Session is a server-side session row. ID is the SHA-256 of the opaque cookie
// token, never the token itself.
type Session struct {
	ID         string    `json:"id"`
	UserID     int64     `json:"user_id"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	IP         string    `json:"ip,omitempty"`
	UserAgent  string    `json:"user_agent,omitempty"`
}

// APIKey is a scoped programmatic-access credential. KeyHash is the stored
// SHA-256 of the secret; the secret itself is shown only once at creation.
type APIKey struct {
	ID         int64      `json:"id"`
	UserID     int64      `json:"user_id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	KeyHash    string     `json:"-"`
	Scopes     []string   `json:"scopes"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}

// API-key scopes. read gates GET endpoints; write gates mutations; admin gates
// user/settings administration.
const (
	ScopeRead  = "read"
	ScopeWrite = "write"
	ScopeAdmin = "admin"
)

// Tag mirrors a tags row.
type Tag struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Color     string    `json:"color"`
	CreatedAt time.Time `json:"created_at"`
}

// MonitorGroup mirrors a monitor_groups row: an organisational folder in the
// monitor list. A monitor belongs to at most one group, which is what makes a
// folder a folder and not a tag. It carries no runtime meaning — the engine
// reads monitors.parent_id for alert suppression and never touches this.
type MonitorGroup struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	SortOrder int       `json:"sort_order"`
	CreatedAt time.Time `json:"created_at"`
}

// Notification mirrors a notifications row. Config secrets are encrypted at the
// persistence boundary; the API never returns secret fields verbatim (see
// redactConfig).
type Notification struct {
	ID        int64           `json:"id"`
	Name      string          `json:"name"`
	Type      string          `json:"type"`
	Config    json.RawMessage `json:"config"`
	IsDefault bool            `json:"is_default"`
	Active    bool            `json:"active"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// MaintenanceWindow mirrors a maintenance_windows row.
type MaintenanceWindow struct {
	ID          int64           `json:"id"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Active      bool            `json:"active"`
	Strategy    string          `json:"strategy"`
	StartsAt    *time.Time      `json:"starts_at,omitempty"`
	EndsAt      *time.Time      `json:"ends_at,omitempty"`
	Recurrence  json.RawMessage `json:"recurrence,omitempty"`
	Timezone    string          `json:"timezone"`
	// AppliesToAll is the window's SCOPE, stored explicitly (migration 0019)
	// rather than inferred from MonitorIDs being empty.
	//
	// It is derived from the request at write time — an empty monitor_ids still
	// means "every monitor", so the API contract is unchanged — but once stored
	// it can only be changed by another write. maintenance_monitors cascades on
	// a monitor delete, so the old inference let the retention sweep silently
	// widen a one-machine window to the whole fleet, on a timer, weeks after
	// anybody touched it.
	AppliesToAll bool      `json:"applies_to_all"`
	MonitorIDs   []int64   `json:"monitor_ids"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// StatusPage mirrors a status_pages row plus its layout (groups and monitor
// placements). PasswordHash never leaves the persistence layer; HasPassword is
// the derived boolean the API exposes instead.
type StatusPage struct {
	ID          int64  `json:"id"`
	Slug        string `json:"slug"`
	Title       string `json:"title"`
	Description string `json:"description"`
	FooterText  string `json:"footer_text"`
	Theme       string `json:"theme"`
	// LogoPath is server-owned: it is written only by the logo upload and
	// delete endpoints, never by a client PUT. toPage does not map it and
	// neither store's update touches the column.
	LogoPath     string `json:"logo_path,omitempty"`
	CustomDomain string `json:"custom_domain,omitempty"`
	Locale       string `json:"locale,omitempty"`
	// AccentColor is the page's brand colour as lowercase #rrggbb, or empty to
	// use the product accent. It is applied on the public page as an inline
	// override of the --color-accent token family.
	AccentColor   string  `json:"accent_color,omitempty"`
	Published     bool    `json:"published"`
	AllowIndexing bool    `json:"allow_indexing"`
	HasPassword   bool    `json:"has_password"`
	ViewCount     int64   `json:"view_count"`
	MonitorIDs    []int64 `json:"monitor_ids"`
	// Groups and Monitors carry the richer layout used by the public page and
	// the management screen. MonitorIDs stays populated (flat, in display
	// order) for backward compatibility.
	Groups   []StatusPageGroup        `json:"groups,omitempty"`
	Monitors []StatusPageMonitorEntry `json:"monitors,omitempty"`

	// PasswordHash is the argon2id hash gating access; empty means public. It
	// is never serialised.
	PasswordHash string `json:"-"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// StatusPageGroup is a named block of monitors on a status page.
type StatusPageGroup struct {
	ID           int64  `json:"id"`
	StatusPageID int64  `json:"status_page_id"`
	Name         string `json:"name"`
	SortOrder    int    `json:"sort_order"`
}

// StatusPageMonitorEntry places a monitor on a status page: which group it
// belongs to, its order, and whether its uptime percentage is public. GroupName
// is an input convenience (groups are matched/created by name on save) and is
// also filled on read.
type StatusPageMonitorEntry struct {
	MonitorID  int64  `json:"monitor_id"`
	GroupID    *int64 `json:"group_id,omitempty"`
	GroupName  string `json:"group_name,omitempty"`
	SortOrder  int    `json:"sort_order"`
	ShowUptime bool   `json:"show_uptime"`
}

// StatusPageGroupOrder is one group's position in a layout reorder, together
// with the ordered monitors it should contain.
type StatusPageGroupOrder struct {
	GroupID    int64   `json:"group_id"`
	MonitorIDs []int64 `json:"monitor_ids"`
}

// StatusPageLayoutOrder is a COMPLETE ordering of a status page's layout: the
// ungrouped monitors first, then every group in display order with its own
// ordered monitors. It must name every group and every monitor currently on the
// page exactly once — a partial or duplicate-bearing ordering is rejected by the
// store, which also makes it an optimistic concurrency check (group ids are
// recreated on every page save).
type StatusPageLayoutOrder struct {
	Ungrouped []int64                `json:"ungrouped"`
	Groups    []StatusPageGroupOrder `json:"groups"`
}

// Incident is a status_pages incident with its lifecycle status, affected
// monitors and (optionally) its update timeline.
type Incident struct {
	ID           int64            `json:"id"`
	StatusPageID int64            `json:"status_page_id"`
	Title        string           `json:"title"`
	Body         string           `json:"body"`
	Severity     string           `json:"severity"`
	Status       string           `json:"status"`
	Pinned       bool             `json:"pinned"`
	StartedAt    time.Time        `json:"started_at"`
	ResolvedAt   *time.Time       `json:"resolved_at,omitempty"`
	CreatedBy    *int64           `json:"created_by,omitempty"`
	MonitorIDs   []int64          `json:"monitor_ids"`
	Updates      []IncidentUpdate `json:"updates,omitempty"`

	// Postmortem is the retrospective written after the incident closed
	// (Markdown). It is server-owned in the same sense as StatusPage.LogoPath:
	// UpdateIncident does not read it and neither store's UPDATE names the
	// column, so a full-replace PUT of the incident cannot wipe a retrospective
	// its client never loaded. SetIncidentPostmortem is the only writer.
	Postmortem string `json:"postmortem,omitempty"`
	// PostmortemPublishedAt is when the retrospective became visible to
	// anonymous visitors; nil means it is a draft, and nil is exactly what makes
	// publicCopy blank the text out. This field itself is never stripped — for a
	// draft it is already nil, so there is nothing to hide.
	PostmortemPublishedAt *time.Time `json:"postmortem_published_at,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// publicCopy is the incident as an anonymous visitor may see it.
//
// The public status-page payload embeds this type verbatim, so every field added
// to Incident is published by default — which is the wrong default for a draft
// retrospective. A draft is written over days, is wrong in its first pass and
// names internal systems; it must not be one JSON field away from the open
// internet. Publication is an explicit act, and this is where that is enforced.
func (in Incident) publicCopy() Incident {
	if in.PostmortemPublishedAt == nil {
		in.Postmortem = ""
	}
	// Internal user ids do not go to anonymous visitors.
	//
	// CreatedBy is stamped with the acting operator's id on every incident and
	// every timeline entry, and this function stripped only the draft
	// retrospective — so the public payload carried "created_by": 3 throughout,
	// enough to count staff accounts and correlate which one handles which
	// outage. The client's own PublicIncident type does not even declare the
	// field, which is what "published by default" costs.
	in.CreatedBy = nil
	// The Updates slice is COPIED before it is edited.
	//
	// The receiver is by value, so `in.CreatedBy = nil` is contained — but
	// `in.Updates` is a slice header sharing the caller's backing array, so
	// writing through it edited the caller's own incident. Four handlers announce
	// and then serialise the same variable, so the AUTHENTICATED response lost
	// created_by on every timeline entry: the same entity came back with the
	// field from the list endpoint and without it from create, update, timeline
	// and postmortem. It would also corrupt any store that ever returned its own
	// backing array.
	if len(in.Updates) > 0 {
		updates := make([]IncidentUpdate, len(in.Updates))
		copy(updates, in.Updates)
		for i := range updates {
			updates[i].CreatedBy = nil
		}
		in.Updates = updates
	}
	return in
}

// IncidentUpdate is one chronological post on an incident's timeline.
type IncidentUpdate struct {
	ID         int64     `json:"id"`
	IncidentID int64     `json:"incident_id"`
	Status     string    `json:"status"`
	Body       string    `json:"body"`
	CreatedBy  *int64    `json:"created_by,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// Subscriber is a status-page e-mail subscriber. The tokens never leave the
// persistence layer except in the links the mailer builds.
type Subscriber struct {
	ID               int64      `json:"id"`
	StatusPageID     int64      `json:"status_page_id"`
	Email            string     `json:"email"`
	Verified         bool       `json:"verified"`
	VerifyToken      string     `json:"-"`
	UnsubscribeToken string     `json:"-"`
	CreatedAt        time.Time  `json:"created_at"`
	VerifiedAt       *time.Time `json:"verified_at,omitempty"`
}

// Incident lifecycle statuses and severities.
const (
	IncidentInvestigating = "investigating"
	IncidentIdentified    = "identified"
	IncidentMonitoring    = "monitoring"
	IncidentResolved      = "resolved"
)

// Setting is one settings row.
type Setting struct {
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// AuditEntry is one audit_log row.
type AuditEntry struct {
	ID         int64           `json:"id"`
	UserID     *int64          `json:"user_id,omitempty"`
	Action     string          `json:"action"`
	EntityType string          `json:"entity_type,omitempty"`
	EntityID   *int64          `json:"entity_id,omitempty"`
	Details    json.RawMessage `json:"details,omitempty"`
	IP         string          `json:"ip,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

// HeartbeatView is one heartbeat as returned by the read endpoints.
type HeartbeatView struct {
	MonitorID int64           `json:"monitor_id"`
	CheckedAt time.Time       `json:"checked_at"`
	Status    string          `json:"status"`
	LatencyMs *int            `json:"latency_ms,omitempty"`
	Message   string          `json:"message,omitempty"`
	Important bool            `json:"important"`
	Retries   int             `json:"retries"`
	Extra     json.RawMessage `json:"extra,omitempty"`
}

// Page is a list-endpoint pagination window.
type Page struct {
	Limit  int
	Offset int
}
