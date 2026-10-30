package api

import (
	"context"
	"errors"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
)

// Compile-time assertions that both store implementations satisfy Store.
var (
	_ Store = (*MemStore)(nil)
	_ Store = (*PgStore)(nil)
)

// ErrNotFound is returned by store reads when the row does not exist. Handlers
// translate it to a 404 problem+json response.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned on a uniqueness violation (duplicate email, tag name,
// status-page slug, …). Handlers translate it to 409.
var ErrConflict = errors.New("conflict")

// ErrParentCycle is returned by the store when a write would leave the monitor
// parent graph containing a cycle. The API validates for this first and gives a
// friendlier message; this is the check inside the writing transaction, which
// is the one two concurrent requests cannot slip past.
var ErrParentCycle = errors.New("parent cycle")

// Store is the API's full persistence surface. It is deliberately one
// interface so a single in-memory implementation backs both the handler tests
// and database-less DEV_MODE, while PgStore backs production. The sub-groups
// below are documentation only; callers use the whole interface.
type Store interface {
	userStore
	sessionStore
	apiKeyStore
	badgeTokenStore
	identityStore
	passwordResetStore
	monitorStore
	monitorGroupStore
	tagStore
	notificationStore
	maintenanceStore
	statusPageStore
	settingStore
	auditStore
	heartbeatStore
}

// MonitorInput is the mutable subset of a monitor a create/update carries.
type MonitorInput struct {
	Name       string
	Type       string
	Config     []byte
	Interval   int // seconds
	Timeout    *int
	MaxRetries int
	// RecoveryChecks is the number of consecutive successes a DOWN monitor
	// needs to count as UP; 0 is stored as 1.
	RecoveryChecks  int
	RetryInterval   int // seconds
	ResendInterval  int // minutes
	UpsideDown      bool
	Active          bool
	ParentID        *int64
	GroupID         *int64
	Description     string
	LinkURL         string
	TagIDs          []int64
	NotificationIDs []int64
	CreatedBy       *int64
}

// MonitorFilter narrows a monitor list query.
type MonitorFilter struct {
	Status string
	Type   string
	TagID  *int64
	Search string
	// GroupID filters to one organisational folder. Use GroupUnassigned to ask
	// for the monitors filed in no folder at all, which a nil pointer cannot
	// express (nil means "do not filter by folder").
	GroupID *int64
}

// GroupUnassigned is the MonitorFilter.GroupID sentinel selecting monitors with
// no folder. Group ids are always positive, so 0 is unambiguous.
const GroupUnassigned int64 = 0

// MonitorLinks are a monitor's association rows: the tags and notification
// channels attached to it. They are written by MonitorInput and read back here,
// so a client that just saved a monitor sees the same ids it sent.
type MonitorLinks struct {
	TagIDs          []int64
	NotificationIDs []int64
}

// MonitorSchedule is the check-timing subset the bulk "schedule" action writes.
// Interval is required; the pointer fields are optional overrides applied only
// when non-nil, so a bulk schedule can change just the cadence without
// flattening a mixed selection's timeout, retry count and retry interval. It is
// the narrow write the bulk endpoint uses instead of round-tripping a full
// monitor through UpdateMonitor, the same reason SetMonitorGroup exists.
type MonitorSchedule struct {
	Interval      int  // seconds, >= 1
	Timeout       *int // seconds; nil leaves the column unchanged
	MaxRetries    *int // nil leaves the column unchanged
	RetryInterval *int // seconds; nil leaves the column unchanged
}

// MonitorStats is the heartbeat-derived summary the dashboard renders next to
// each monitor. Every field is nil/empty when the monitor has no heartbeat in
// the window; the uptime fractions are 0..1, matching report.UptimeResult.
type MonitorStats struct {
	LatencyMs     *int
	LastCheckedAt *time.Time
	Uptime24h     *float64
	Uptime30d     *float64
	Uptime1y      *float64
	// Sparkline is a short trailing latency series, oldest first.
	Sparkline []int
	// RecentBeats is the status of the newest recentBeatPoints heartbeats,
	// oldest first — the per-check bar every monitor-list row renders. Unlike
	// Sparkline it keeps the beats without a latency: a DOWN beat usually has
	// none, and dropping it would erase exactly the outage the bar is for.
	RecentBeats []string
}

// StatsScope selects how far back MonitorStats aggregates, because the two
// callers want genuinely different things and the difference is expensive.
//
// The monitor LIST renders 24 h and 30 d per row and has never shown a yearly
// figure. Computing one anyway meant every dashboard load read a year of
// heartbeats for every monitor — measured on a real instance: 39 monitors,
// ~49 000 beats each, 6 991 ms for one request, with the whole UI waiting on it
// because the layout gates `ready` on exactly this call.
type StatsScope int

const (
	// StatsList is the monitor list: 24 h and 30 d, no yearly window.
	StatsList StatsScope = iota
	// StatsDetail is a single monitor's page, which does show the year.
	StatsDetail
)

// sparklinePoints is the length of the trailing latency series MonitorStats
// carries. It matches the width the dashboard card renders.
const sparklinePoints = 40

// recentBeatPoints is the length of MonitorStats.RecentBeats. Fifty is what the
// monitor detail page's bar shows at full width; the list row shows the newest
// ones that fit and clips the rest.
const recentBeatPoints = 50

type userStore interface {
	CountUsers(ctx context.Context) (int, error)
	CreateUser(ctx context.Context, email, username, passwordHash, displayName string, role string) (User, error)
	// CreateUserWithIdentity provisions a single sign-on account: the user row
	// and its external identity in one transaction.
	//
	// Done as two calls, a failure or a crash between them left a user with no
	// password and no identity — an account nobody can log into that
	// nevertheless holds the e-mail address and username, so every later
	// attempt by the same person is refused as "address already taken". The
	// account cannot recover itself and the operator has no obvious reason to
	// look for it.
	CreateUserWithIdentity(ctx context.Context, email, username, displayName, role string, id ExternalIdentity) (User, error)
	// StoredTOTPSecrets lists every non-empty totp_secret with its user id, for
	// the one-shot re-seal at startup. Migration 0013 could not encrypt
	// existing rows — the key lives in this process, never in the database —
	// and relied on each account's NEXT write to re-seal it. An account with
	// 2FA enabled that nobody touches has no next write, so its secret would
	// sit in the clear indefinitely, which is the situation the migration
	// exists to end.
	StoredTOTPSecrets(ctx context.Context) (map[int64]string, error)
	// UpdateTOTPSecret replaces only the secret column, leaving the enabled
	// flag and the backup codes untouched. SetUserTOTP would need both, and the
	// re-seal has no business knowing either.
	UpdateTOTPSecret(ctx context.Context, userID int64, secret string) error
	GetUserByID(ctx context.Context, id int64) (User, error)
	GetCredentialsByEmail(ctx context.Context, email string) (Credentials, error)
	// GetCredentialsByLogin resolves an account by a login identifier that may be
	// either its e-mail address or its username, matched case-insensitively. It is
	// the only lookup the password login path uses, so a user can sign in with
	// whichever they remember.
	GetCredentialsByLogin(ctx context.Context, login string) (Credentials, error)
	GetCredentialsByID(ctx context.Context, id int64) (Credentials, error)
	ListUsers(ctx context.Context) ([]User, error)
	UpdateUser(ctx context.Context, id int64, email, username, displayName, role string, isActive bool) (User, error)
	UpdateUserPassword(ctx context.Context, id int64, passwordHash string) error
	// ChangePassword sets a new password AND revokes what the old one could
	// still reach, in one transaction: every session except keepSessionID, and
	// every outstanding reset token.
	//
	// One call rather than three, because the three used to be separate and the
	// two after the password write were best-effort. A user who changes their
	// password because they think someone else has it is told the other
	// sessions are gone; if the delete failed and the error was dropped, they
	// were told something untrue at the one moment it mattered. Outstanding
	// reset tokens are the same problem a step removed: a mail sitting in an
	// old inbox stayed redeemable for its full hour after the account had
	// deliberately been secured.
	ChangePassword(ctx context.Context, id int64, passwordHash, keepSessionID string) error
	// InvalidatePasswordResets marks every outstanding reset token for a user
	// used. Changing the e-mail address calls it: the tokens were mailed to the
	// PREVIOUS address, which is exactly the address being moved away from.
	InvalidatePasswordResets(ctx context.Context, userID int64) error
	SetUserTOTP(ctx context.Context, id int64, secret string, enabled bool, backupCodes []string) error
	// SetUserAvatar replaces only avatar_path, leaving e-mail, role and every
	// other column untouched. UpdateUser is full-replace, so the avatar endpoint
	// would otherwise have to round-trip the whole user just to store a file
	// name, and avatar_path would stop being server-owned. It mirrors
	// SetStatusPageLogo, including the second result: the path this write
	// replaced, read atomically with the write. Cleanup must delete THAT file
	// and never a value the caller read before consuming the request body, or
	// two overlapping uploads orphan the loser's file. Pass "" to clear.
	SetUserAvatar(ctx context.Context, id int64, avatarPath string) (User, string, error)
	DeleteUser(ctx context.Context, id int64) error
}

type sessionStore interface {
	CreateSession(ctx context.Context, s Session) error
	GetSession(ctx context.Context, id string) (Session, error)
	TouchSession(ctx context.Context, id string, lastSeen, expires time.Time) error
	DeleteSession(ctx context.Context, id string) error
	ListSessionsByUser(ctx context.Context, userID int64) ([]Session, error)
	DeleteSessionsByUser(ctx context.Context, userID int64, exceptID string) error
}

type apiKeyStore interface {
	CreateAPIKey(ctx context.Context, k APIKey) (APIKey, error)
	GetAPIKeyByHash(ctx context.Context, hash string) (APIKey, error)
	ListAPIKeys(ctx context.Context, userID int64) ([]APIKey, error)
	TouchAPIKey(ctx context.Context, id int64, at time.Time) error
	DeleteAPIKey(ctx context.Context, id, userID int64) error
	// DeleteAPIKeysByUser revokes every key an account holds and reports how
	// many there were. Account recovery uses it: a key outlives sessions by
	// design, so a password reset that only kills sessions leaves the account
	// reachable by whoever prompted the reset.
	DeleteAPIKeysByUser(ctx context.Context, userID int64) (int, error)
}

// badgeTokenStore persists the opaque, revocable token that addresses a
// monitor's public SVG badge. The token replaces the monitor's sequential id in
// the badge URL so the public badge endpoints cannot be walked to enumerate
// every monitor in the instance.
type badgeTokenStore interface {
	// EnsureBadgeToken returns the monitor's badge token, minting candidate as
	// the token when the monitor has none yet. It is idempotent: an existing
	// token is returned unchanged, so a monitor's badge URL stays stable across
	// repeated calls. candidate is used only on first mint.
	EnsureBadgeToken(ctx context.Context, monitorID int64, candidate string) (string, error)
	// DeleteBadgeToken revokes a monitor's badge token so its badge URL stops
	// resolving. It is idempotent — revoking a monitor that has no token is not
	// an error — so a caller need not first check whether one exists.
	DeleteBadgeToken(ctx context.Context, monitorID int64) error
	// MonitorIDByBadgeToken resolves an opaque badge token to its monitor id,
	// returning ErrNotFound for an unknown (or revoked) token. This is the only
	// way the public badge endpoints reach a monitor.
	MonitorIDByBadgeToken(ctx context.Context, token string) (int64, error)
}

// ExternalIdentity links a local user to an account at an OIDC provider. The
// join key is (Issuer, Subject) and nothing else: Email is refreshed on every
// login for display only and must NEVER be used as a lookup key, or a hostile
// or compromised IdP could take over any local account by asserting its address.
type ExternalIdentity struct {
	ID          int64      `json:"id"`
	UserID      int64      `json:"-"`
	Provider    string     `json:"provider"`
	Issuer      string     `json:"issuer"`
	Subject     string     `json:"subject"`
	Email       string     `json:"email,omitempty"`
	DisplayName string     `json:"display_name,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
}

type identityStore interface {
	// GetIdentity is the ONLY way an assertion resolves to a local account.
	GetIdentity(ctx context.Context, issuer, subject string) (ExternalIdentity, error)
	CreateIdentity(ctx context.Context, in ExternalIdentity) (ExternalIdentity, error)
	ListIdentitiesByUser(ctx context.Context, userID int64) ([]ExternalIdentity, error)
	// TouchIdentity refreshes the display-only claims and the login timestamp.
	TouchIdentity(ctx context.Context, id int64, at time.Time, email, displayName string) error
	// DeleteIdentity scopes the delete by userID in the statement, like
	// DeleteAPIKey, so unlinking cannot reach another user's row.
	DeleteIdentity(ctx context.Context, id, userID int64) error
}

// PasswordReset is one outstanding password-reset token. TokenHash is the
// SHA-256 of the mailed token; the plaintext exists only in the handler
// variable and the mail body, and is never stored, logged or audited.
type PasswordReset struct {
	ID          int64      `json:"id"`
	UserID      int64      `json:"user_id"`
	TokenHash   string     `json:"-"`
	ExpiresAt   time.Time  `json:"expires_at"`
	UsedAt      *time.Time `json:"used_at,omitempty"`
	RequestedIP string     `json:"requested_ip,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

type passwordResetStore interface {
	CreatePasswordReset(ctx context.Context, r PasswordReset) (PasswordReset, error)
	// ConsumePasswordReset redeems a token and writes the new password hash in
	// ONE transaction, then marks the user's other outstanding tokens used so a
	// stale mail in an old inbox cannot be replayed after recovery. Splitting the
	// mark and the write across two calls would let a token be redeemed twice
	// under concurrency.
	//
	// Unknown, expired and already-used tokens all return ErrNotFound. That is
	// deliberate and load-bearing: the handler answers with one generic 400 for
	// every failure mode, so a caller cannot learn that a token ever existed.
	// Session invalidation stays with the caller (DeleteSessionsByUser).
	ConsumePasswordReset(ctx context.Context, tokenHash, passwordHash string, now time.Time) (PasswordReset, error)
}

type monitorStore interface {
	CreateMonitor(ctx context.Context, in MonitorInput) (domain.Monitor, error)
	GetMonitor(ctx context.Context, id int64) (domain.Monitor, error)
	ListMonitors(ctx context.Context, f MonitorFilter, p Page) ([]domain.Monitor, int, error)
	UpdateMonitor(ctx context.Context, id int64, in MonitorInput) (domain.Monitor, error)
	DeleteMonitor(ctx context.Context, id int64) error
	SetMonitorActive(ctx context.Context, id int64, active bool) error
	// MonitorLinks returns the tag and notification ids of every requested
	// monitor in ONE round trip. The list endpoint decorates a whole page of
	// monitors with it, so a per-monitor read here would be an N+1.
	MonitorLinks(ctx context.Context, ids []int64) (map[int64]MonitorLinks, error)
	// MonitorStats returns the heartbeat-derived summary of every requested
	// monitor. Like MonitorLinks it is deliberately batched: the dashboard asks
	// for it once per list request, never once per monitor. Monitors without
	// heartbeats are simply absent from the map.
	MonitorStats(ctx context.Context, ids []int64, now time.Time, scope StatsScope) (map[int64]MonitorStats, error)
	// SetMonitorGroup files a monitor under an organisational folder, or under
	// none when groupID is nil. It is the narrow write the bulk endpoint uses:
	// UpdateMonitor is full-replace and would need the caller to round-trip
	// every other column just to move a monitor between folders.
	SetMonitorGroup(ctx context.Context, id int64, groupID *int64) error
	// AddMonitorTags attaches tags to a monitor WITHOUT removing the ones it
	// already carries — unlike UpdateMonitor, which replaces the whole set.
	// Bulk tagging is additive on purpose: the caller selected monitors with
	// differing tags and asked to add one, not to flatten them to a shared set.
	// Already-present tags are silently kept, so the call is idempotent.
	AddMonitorTags(ctx context.Context, id int64, tagIDs []int64) error
	// AddMonitorNotifications attaches notification channels to a monitor WITHOUT
	// removing the ones it already carries, mirroring AddMonitorTags. Bulk
	// attaching channels is additive for the same reason: the caller asked to
	// add a channel to a mixed selection, not to replace each monitor's set.
	// Already-present channels are silently kept, so the call is idempotent.
	AddMonitorNotifications(ctx context.Context, id int64, notificationIDs []int64) error
	// SetMonitorSchedule rewrites a monitor's check timing from a MonitorSchedule
	// without disturbing any other column. It is the narrow write the bulk
	// endpoint uses: UpdateMonitor is full-replace and would need the caller to
	// round-trip every field just to change the interval. A nil override leaves
	// that column exactly as it was.
	SetMonitorSchedule(ctx context.Context, id int64, sched MonitorSchedule) error

	// --- recycle bin ---
	//
	// DeleteMonitor is a SOFT delete: the row is marked and its heartbeats,
	// tags, channel links and status-page placements stay. Every read above
	// excludes a binned monitor, so from any other caller's point of view it is
	// simply gone; the four methods here are the only way back to it.
	//
	// A restored monitor comes back PAUSED. Neither store can know what `active`
	// was before the delete, and guessing "on" would restart checks and alerts
	// for something that has been gone for a week. See migration 0018.

	// GetDeletedMonitor reads one binned monitor. ErrNotFound for a live one:
	// the two sets are disjoint, and a caller that means "restore this" must not
	// be answered about a monitor that was never deleted.
	GetDeletedMonitor(ctx context.Context, id int64) (domain.Monitor, error)
	// ListDeletedMonitors returns the bin, most recently deleted first.
	ListDeletedMonitors(ctx context.Context) ([]domain.Monitor, error)
	// RestoreMonitor un-bins a monitor and returns it, paused.
	RestoreMonitor(ctx context.Context, id int64) (domain.Monitor, error)
	// PurgeMonitor hard-deletes a BINNED monitor, taking its heartbeats with it.
	// It refuses a live one, so the only route to an irreversible delete runs
	// through the bin and there is always a window to undo a mistake.
	PurgeMonitor(ctx context.Context, id int64) error
}

// NOTE: the grace-period sweep that empties the bin on a timer is NOT here. It
// lives in internal/retention, which already owns the loop, the metrics and the
// logging for every "delete things older than N days" policy in the product —
// and putting it here would have meant a second implementation of one rule, in
// a store whose two halves must be kept in lockstep by hand.

// monitorGroupStore is the CRUD surface for organisational folders. Deleting a
// group must leave its monitors alive with group_id cleared (the schema says
// ON DELETE SET NULL); both implementations are held to that.
type monitorGroupStore interface {
	CreateMonitorGroup(ctx context.Context, name string, sortOrder int) (MonitorGroup, error)
	ListMonitorGroups(ctx context.Context) ([]MonitorGroup, error)
	GetMonitorGroup(ctx context.Context, id int64) (MonitorGroup, error)
	UpdateMonitorGroup(ctx context.Context, id int64, name string, sortOrder int) (MonitorGroup, error)
	DeleteMonitorGroup(ctx context.Context, id int64) error
}

type tagStore interface {
	CreateTag(ctx context.Context, name, color string) (Tag, error)
	ListTags(ctx context.Context) ([]Tag, error)
	GetTag(ctx context.Context, id int64) (Tag, error)
	UpdateTag(ctx context.Context, id int64, name, color string) (Tag, error)
	DeleteTag(ctx context.Context, id int64) error
}

type notificationStore interface {
	CreateNotification(ctx context.Context, n Notification) (Notification, error)
	ListNotifications(ctx context.Context) ([]Notification, error)
	GetNotification(ctx context.Context, id int64) (Notification, error)
	UpdateNotification(ctx context.Context, id int64, n Notification) (Notification, error)
	DeleteNotification(ctx context.Context, id int64) error
}

type maintenanceStore interface {
	CreateMaintenance(ctx context.Context, m MaintenanceWindow) (MaintenanceWindow, error)
	ListMaintenance(ctx context.Context) ([]MaintenanceWindow, error)
	GetMaintenance(ctx context.Context, id int64) (MaintenanceWindow, error)
	UpdateMaintenance(ctx context.Context, id int64, m MaintenanceWindow) (MaintenanceWindow, error)
	DeleteMaintenance(ctx context.Context, id int64) error
}

type statusPageStore interface {
	CreateStatusPage(ctx context.Context, p StatusPage) (StatusPage, error)
	ListStatusPages(ctx context.Context) ([]StatusPage, error)
	GetStatusPage(ctx context.Context, id int64) (StatusPage, error)
	GetStatusPageBySlug(ctx context.Context, slug string) (StatusPage, error)
	GetStatusPageByDomain(ctx context.Context, host string) (StatusPage, error)
	UpdateStatusPage(ctx context.Context, id int64, p StatusPage) (StatusPage, error)
	// SetStatusPageLogo replaces only the logo path, leaving layout, password
	// and every other column untouched. UpdateStatusPage is full-replace and
	// would recreate every group and placement, so the logo endpoints use this
	// narrow write instead. It is the only writer of logo_path after creation.
	// The second result is the path this write replaced, read atomically with
	// the write: cleanup must delete that file and never a value the caller
	// read earlier, or two overlapping uploads orphan the loser's file.
	SetStatusPageLogo(ctx context.Context, id int64, logoPath string) (StatusPage, string, error)
	DeleteStatusPage(ctx context.Context, id int64) error
	IncrementStatusPageViews(ctx context.Context, id int64) error
	// ReorderStatusPageLayout rewrites group and monitor positions from a
	// complete ordering. The order must name every group and every monitor
	// currently on the page exactly once; anything else is ErrConflict, which
	// makes a reorder computed against a stale layout fail instead of applying
	// partially.
	ReorderStatusPageLayout(ctx context.Context, pageID int64, order StatusPageLayoutOrder) (StatusPage, error)
	// StatusPageSlugsForMonitor lists the slugs of PUBLISHED status pages that
	// display a monitor. It backs the WebSocket poke that makes a public page
	// refresh when one of its monitors changes state: those pages listen on
	// status-page:{slug}, never on the private monitor:/dashboard: topics, so
	// without this lookup a monitor going down never reaches them.
	StatusPageSlugsForMonitor(ctx context.Context, monitorID int64) ([]string, error)

	// Incidents.
	CreateIncident(ctx context.Context, in Incident) (Incident, error)
	ListIncidents(ctx context.Context, pageID int64) ([]Incident, error)
	GetIncident(ctx context.Context, id int64) (Incident, error)
	// UpdateIncident is full-replace over the incident's editable fields. It
	// deliberately does NOT touch postmortem/postmortem_published_at: those are
	// written only by SetIncidentPostmortem, for the reason StatusPage.LogoPath
	// is written only by SetStatusPageLogo — an editor that loaded the incident
	// before the retrospective existed would otherwise erase it by saving a
	// title change.
	UpdateIncident(ctx context.Context, id int64, in Incident) (Incident, error)
	DeleteIncident(ctx context.Context, id int64) error
	AddIncidentUpdate(ctx context.Context, u IncidentUpdate) (IncidentUpdate, error)
	// SetIncidentPostmortem replaces the retrospective and its publication
	// timestamp, leaving every other column untouched. An empty body clears the
	// column; publishedAt nil is the draft state.
	SetIncidentPostmortem(ctx context.Context, id int64, body string, publishedAt *time.Time) (Incident, error)

	// Subscribers (double-opt-in).
	CreateSubscriber(ctx context.Context, sub Subscriber) (Subscriber, error)
	ListSubscribers(ctx context.Context, pageID int64, verifiedOnly bool) ([]Subscriber, error)
	GetSubscriberByVerifyToken(ctx context.Context, token string) (Subscriber, error)
	GetSubscriberByUnsubscribeToken(ctx context.Context, token string) (Subscriber, error)
	VerifySubscriber(ctx context.Context, id int64) (Subscriber, error)
	DeleteSubscriber(ctx context.Context, id int64) error
}

type settingStore interface {
	GetSetting(ctx context.Context, key string) (Setting, error)
	ListSettings(ctx context.Context) ([]Setting, error)
	SetSetting(ctx context.Context, key string, value []byte) (Setting, error)
}

type auditStore interface {
	// InsertAudit records one entry and returns it with the id the store
	// assigned. The id is what lets the SIEM forwarder emit a row a collector
	// can deduplicate and line up against the CSV export; before this returned
	// anything, every forwarded event carried id 0.
	InsertAudit(ctx context.Context, e AuditEntry) (AuditEntry, error)
	ListAudit(ctx context.Context, p Page) ([]AuditEntry, int, error)
	// ExportAudit returns entries whose created_at falls in [from, to], NEWEST
	// FIRST, at most limit of them. A zero `from` means "since the beginning";
	// `to` is always meaningful (the caller passes now when it was not given).
	//
	// It is separate from ListAudit rather than a widened Page because the two
	// have opposite jobs: ListAudit pages a screen and caps at 200 rows,
	// ExportAudit fills a file and is bounded only by the export cap. Sharing
	// one method would have made the screen's cap the export's cap.
	//
	// The limit is a hard cap, not a page size: the caller asks for one more
	// row than it will emit to learn whether the window was truncated.
	ExportAudit(ctx context.Context, from, to time.Time, limit int) ([]AuditEntry, error)
}

type heartbeatStore interface {
	// ListHeartbeats returns a monitor's most recent heartbeats, newest first.
	// importantOnly restricts the result to state transitions, which is the
	// event log the monitor detail page renders.
	ListHeartbeats(ctx context.Context, monitorID int64, limit int, importantOnly bool) ([]HeartbeatView, error)
	// ListHeartbeatWindow returns at most limit heartbeats from [from, to],
	// newest first, spread EVENLY across the window rather than taken from its
	// newest end.
	//
	// Both properties matter. The handler used to read the newest N beats and
	// filter them by time afterwards, so the cap bit before the window did: on a
	// 60-second monitor the offered "1 year" view really showed about the last
	// 35 days, silently. Selecting inside the window fixes the truncation, and
	// striding across it is what keeps a wide window bounded without going back
	// to showing only its tail.
	ListHeartbeatWindow(ctx context.Context, monitorID int64, from, to time.Time, limit int, importantOnly bool) ([]HeartbeatView, error)
	LatestHeartbeat(ctx context.Context, monitorID int64) (HeartbeatView, bool, error)
	// ListImportantHeartbeats returns the state transitions of every LIVE
	// monitor (soft-deleted ones excluded), newest first, one page at a time,
	// plus the total across all pages. It is the instance-wide event log of the
	// dashboard home; the per-monitor log is ListHeartbeats with importantOnly.
	ListImportantHeartbeats(ctx context.Context, p Page) ([]HeartbeatView, int, error)
}
