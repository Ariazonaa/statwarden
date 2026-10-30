/**
 * The dashboard's model of the Statwarden REST API resources (see
 * docs/architecture.md "REST API").
 *
 * These are NOT the wire shapes. The Go API speaks snake_case with int64 ids;
 * this module is camelCase with string ids, and every field is translated by an
 * explicit <entity>FromWire / <entity>ToWire mapper in ./client.ts. Ids are
 * strings here because they are only ever used as identity (map keys, `key`
 * expressions, equality) — never arithmetic — and a string survives an int64
 * that JavaScript's number cannot represent exactly. The mappers are the only
 * place the two representations meet: nothing outside ./client.ts may compare a
 * modelled id to a raw wire number.
 *
 * The mock backend in ./mock.ts produces these same shapes, so the UI is
 * developed without a live server. `export type Api = typeof mockApi` pins the
 * METHODS but not the SHAPES, so a mock that drifts from a mapper is invisible
 * to the compiler: when changing a mapper here, change the mock in the same
 * commit.
 */
import type { MonitorStatus } from '$lib/design-system';
import type { MessageKey } from '$lib/i18n';

export type { MonitorStatus };

export type Role = 'admin' | 'user' | 'viewer';

export interface User {
	id: string;
	/**
	 * The account's real, unique login name (server `username`). A sign-in accepts
	 * either this or the email; both are matched case-insensitively server-side.
	 */
	username: string;
	email: string;
	role: Role;
	avatarUrl?: string;
	totpEnabled: boolean;
	/** Inverse of the server's `is_active`. */
	disabled: boolean;
	createdAt: string;
	/**
	 * NOT SERVED: the users table has no last_login_at column, so userFromWire
	 * always leaves this undefined and the mock does not invent one. Adding it
	 * is a schema change, not a mapping fix.
	 */
	lastLoginAt?: string;
}

/**
 * An SSO provider the server has configured, as returned by the unauthenticated
 * GET /auth/oidc/providers. Deliberately carries nothing but the label and the
 * routing id: the issuer, client id and client secret live in server config and
 * are never served, so the login screen cannot leak them. An empty list means
 * SSO is switched off and the login screen renders no SSO section at all.
 */
export interface OidcProvider {
	/** Path segment for /api/v1/auth/oidc/{id}/start. */
	id: string;
	displayName: string;
}

/**
 * Proof that the caller is still the account owner rather than merely the
 * holder of a session cookie, required by the calls that change how an account
 * can be logged into: minting an API key, enrolling an authenticator, linking a
 * single sign-on identity, regenerating backup codes.
 *
 * Each of those turns a borrowed session — an unlocked laptop, a stolen cookie
 * — into standing access that outlives the session and survives the owner
 * changing their password. `totpCode` is required when the account has 2FA
 * enabled and may also be a backup code, which is then consumed.
 *
 * An account provisioned through single sign-on has no password and, unless it
 * enrolled one, no second factor; the server skips the check for it and records
 * that it did.
 */
export interface StepUpConfirmation {
	password: string;
	totpCode?: string;
}

/**
 * The pending TOTP enrolment secret, as POST /me/totp/enroll returns it before
 * activation. `otpauthUri` is the otpauth:// URI the enrolment QR code encodes;
 * `secret` is the same key in base32 for manual entry. Neither is served again
 * once 2FA is verified — the row keeps only the secret and never re-exposes it.
 */
export interface TotpEnrollment {
	secret: string;
	otpauthUri: string;
}

/**
 * An external identity (OIDC single sign-on) linked to the caller's account, as
 * GET /me/oidc/identities lists it. The join key the server authenticates on is
 * (issuer, subject); `email` and `displayName` are display-only claims the
 * provider last asserted and may be stale. Never carries a token or secret.
 */
export interface OidcIdentity {
	id: string;
	/** Local provider id — the {provider} path segment, e.g. "keycloak". */
	provider: string;
	issuer: string;
	subject: string;
	email?: string;
	displayName?: string;
	createdAt: string;
	/** Absent until the identity has been used to sign in. */
	lastLoginAt?: string;
}

export interface Session {
	/**
	 * DIVERGES: the server stores the SHA-256 of the cookie token as the row id
	 * and deliberately blanks it on read, so a real session arrives with id ''
	 * and cannot be addressed individually — DELETE /me/sessions/{id} only
	 * accepts the literal 'others'. The mock still mints per-session ids because
	 * the settings screen keys its list on them and offers a per-row revoke.
	 * Closing this needs either an opaque server-side handle or a single
	 * "revoke all other sessions" control; it is not a mapping fix.
	 */
	id: string;
	userAgent: string;
	ip: string;
	current: boolean;
	createdAt: string;
	lastSeenAt: string;
}

export interface ApiKey {
	id: string;
	name: string;
	scopes: string[];
	prefix: string;
	/** Only returned once, at creation time. */
	token?: string;
	lastUsedAt?: string;
	createdAt: string;
	/** Absent means the key never expires. */
	expiresAt?: string;
}

/**
 * How a field's value is carried on the wire when it is NOT the scalar its
 * widget natively produces. A text/textarea input always yields a string, but
 * several Go config fields want an array or an object; `shape` names the
 * conversion that ./registry.ts `configFromForm` applies on the way out and
 * `configToForm` reverses on the way in. A field without a `shape` is sent as
 * the widget's own scalar.
 */
export type FieldShape =
	/** `[]string` — entries split on newlines and commas. */
	| 'stringlist'
	/** `map[string]string` — one `Name: value` per line. */
	| 'keyvalue'
	/** httpcheck.StatusCodeSet — `200-299, 301` becomes `["200-299", 301]`. */
	| 'statuscodes'
	/** An arbitrary JSON object typed into a textarea (webpush subscriptions). */
	| 'json';

export interface FieldOption {
	value: string;
	/**
	 * Literal, untranslated label — correct for protocol tokens (GET, AAAA,
	 * authPriv, verify-full) which are wire values, not prose.
	 */
	label: string;
	/** Set instead of leaning on `label` when the option name reads as prose. */
	labelKey?: MessageKey;
}

/**
 * A field in a monitor-type or notification-type config schema. `key` is the
 * Go struct's json tag verbatim — it is the wire name, never a display name.
 */
export interface FieldSpec {
	key: string;
	label: MessageKey;
	type: 'text' | 'number' | 'textarea' | 'boolean' | 'select' | 'password' | 'url';
	shape?: FieldShape;
	/** A literal example value, never prose — so it is deliberately not a MessageKey. */
	placeholder?: string;
	hint?: MessageKey;
	required?: boolean;
	default?: string | number | boolean;
	options?: FieldOption[];
	/**
	 * Only show (and only validate) when another field holds one of the given
	 * values. `in` covers the cases a single `equals` cannot, such as SNMP's
	 * auth_protocol being needed for both authNoPriv and authPriv.
	 */
	showIf?: { key: string; equals?: string | number | boolean; in?: (string | number | boolean)[] };
	min?: number;
	max?: number;
}

export interface MonitorTypeSpec {
	/** The registry name the Go server registers the checker under. */
	type: string;
	label: MessageKey;
	family: 'http' | 'network' | 'service' | 'database' | 'push';
	description: MessageKey;
	fields: FieldSpec[];
}

export interface NotificationTypeSpec {
	/** The registry name the Go server registers the provider under. */
	type: string;
	label: MessageKey;
	description: MessageKey;
	fields: FieldSpec[];
}

export interface Tag {
	id: string;
	name: string;
	color: string;
}

/**
 * An organisational folder in the monitor list (monitor_groups). A monitor
 * belongs to at most ONE group — that single assignment is what separates a
 * folder from a tag, which is many-to-many. Purely visual: the engine reads
 * `parentId` for alert suppression and never looks at this.
 */
export interface MonitorGroup {
	id: string;
	name: string;
	/** Position in the monitor list; ties fall back to id on both stores. */
	sortOrder: number;
}

export interface Monitor {
	id: string;
	name: string;
	type: string;
	/**
	 * The push credential, present ONLY in the create response.
	 *
	 * The server reveals it once and blanks it on every later read, so this is
	 * `undefined` everywhere except the object returned by createMonitor. A push
	 * monitor is useless without it — /push/{token} is the whole point of the
	 * type — so the create screen has to surface it while it can.
	 */
	pushToken?: string;
	/**
	 * Set when the row was written but the engine refused to schedule it.
	 *
	 * The monitor exists and reads as active, and nothing is checking it. The
	 * server builds this deliberately rather than only logging it, so a screen
	 * that drops it renders a healthy-looking card for a monitor that is not
	 * being monitored.
	 */
	schedulingError?: string;
	/**
	 * Read-only display target. `config` is the source of truth; the server
	 * lifts the target out of it (monitorTargetURL in internal/api/convert.go)
	 * so every consumer does not re-implement the same per-type guess. Never
	 * sent on write — monitorToWire drops it.
	 */
	url?: string;
	status: MonitorStatus;
	/** Whether the monitor is actively scheduled (false = paused). */
	active: boolean;
	interval: number;
	timeout: number;
	maxRetries: number;
	/**
	 * Consecutive successful checks a DOWN monitor needs before it counts as UP
	 * again — the way-back counterpart of maxRetries. 1 is the old behaviour
	 * (one success recovers); higher values stop a monitor that answers once
	 * between failures from sending a recovery and a new outage per bounce.
	 */
	recoveryChecks: number;
	retryInterval: number;
	/**
	 * How often a DOWN alert repeats, in minutes; 0 never repeats. Modelled — and
	 * edited by MonitorForm — so an update round-trips it: the server's monitor
	 * write is full-replace, so a field the model drops is a field the next save
	 * silently zeroes.
	 */
	resendInterval: number;
	upsideDown: boolean;
	tagIds: string[];
	notificationIds: string[];
	/**
	 * The monitor this one depends on. The server refuses a self-reference and
	 * any cycle (A→B→A and longer) with a 400 naming the field, because the
	 * engine walks this chain to suppress a child's alerts while its parent is
	 * DOWN — a loop there is a live hazard, not untidy data.
	 */
	parentId?: string;
	/** The organisational folder this monitor is filed under, if any. */
	groupId?: string;
	description?: string;
	externalUrl?: string;
	config: Record<string, unknown>;
	/** Latest measured latency in ms. */
	latencyMs?: number;
	/**
	 * Availability as a PERCENTAGE (0..100), undefined when the window holds no
	 * conclusive heartbeat. The wire carries a 0..1 fraction (matching
	 * report.UptimeResult) and a null for "no data"; monitorFromWire scales and
	 * widens it, so nothing outside the mapper multiplies by 100 again.
	 */
	uptime24h?: number;
	uptime30d?: number;
	uptime1y?: number;
	/** Small trailing latency series for the card sparkline, oldest first. */
	sparkline: number[];
	/**
	 * Status of the newest checks (at most 50), oldest first: the per-check bar
	 * of the monitor list. Unlike `sparkline` it keeps the checks that measured
	 * no latency, so a DOWN check is never missing from it.
	 */
	recentBeats: MonitorStatus[];
	lastCheckedAt?: string;
	createdAt: string;
}

/**
 * A monitor's embeddable SVG badge, as POST /monitors/{id}/badge returns it. The
 * token is an opaque, revocable handle: the two URLs are the public
 * …/badge/{token}/status.svg and …/badge/{token}/uptime.svg endpoints, absolute
 * when the server has a public base URL configured and root-relative otherwise.
 * DELETE /monitors/{id}/badge revokes the token and both URLs stop resolving.
 */
export interface MonitorBadge {
	token: string;
	statusUrl: string;
	uptimeUrl: string;
}

/**
 * The actions POST /monitors/bulk understands. `tag` and `notify` ADD to the
 * monitor's set rather than replacing it — matching AddMonitorTags and
 * AddMonitorNotifications on the server. `schedule` rewrites the check timing.
 */
export type BulkMonitorAction =
	'pause' | 'resume' | 'delete' | 'tag' | 'group' | 'notify' | 'schedule';

/** One monitor's outcome. `error` is a short server-side reason, absent on OK. */
export interface BulkMonitorResult {
	monitorId: string;
	ok: boolean;
	error?: string;
}

/**
 * The whole batch's outcome. The server answers 200 even when some ids failed —
 * a stale id in a selection must not discard the rest of the intent — so a
 * caller has to read `failed`/`results` rather than trusting the absence of a
 * thrown error.
 */
export interface BulkMonitorResponse {
	action: BulkMonitorAction;
	succeeded: number;
	failed: number;
	results: BulkMonitorResult[];
}

/**
 * One bulk action over an explicit id list. `tagIds` is required by `tag`;
 * `groupId` is used by `group`, where `null` is meaningful — it clears the
 * folder — and so is distinct from omitting the key. `notificationIds` is
 * required by `notify`; `schedule` carries the check timing, where `interval`
 * is required and the rest are optional overrides that leave a monitor's stored
 * value untouched when omitted.
 */
export interface BulkMonitorInput {
	action: BulkMonitorAction;
	monitorIds: string[];
	tagIds?: string[];
	groupId?: string | null;
	notificationIds?: string[];
	schedule?: BulkScheduleInput;
}

/**
 * The check-timing values the `schedule` action writes. `interval` is required
 * and in seconds; `timeout` and `retryInterval` are seconds too, and every
 * optional field left `undefined` keeps each monitor's current value rather than
 * clearing it — matching the server's COALESCE write.
 */
export interface BulkScheduleInput {
	interval: number;
	timeout?: number;
	maxRetries?: number;
	retryInterval?: number;
}

export interface Heartbeat {
	monitorId: string;
	status: MonitorStatus;
	latencyMs: number | null;
	message?: string;
	checkedAt: string;
}

/**
 * One state transition. The API has no dedicated event resource: the event log
 * IS the important=true slice of heartbeats, so eventsFromWire reads
 * GET /monitors/{id}/heartbeats?important=true and derives `from` and
 * `durationSec` from the neighbouring transitions.
 */
export interface StateEvent {
	id: string;
	monitorId: string;
	from: MonitorStatus | null;
	to: MonitorStatus;
	message?: string;
	/** Duration in seconds the previous state lasted. */
	durationSec?: number;
	at: string;
}

/**
 * One entry of the instance-wide event log (GET /monitors/events): a state
 * transition of any live monitor. Unlike StateEvent it carries no `from` or
 * duration — the neighbouring entries belong to other monitors, so there is no
 * neighbour to derive them from.
 */
export interface MonitorEvent {
	id: string;
	monitorId: string;
	status: MonitorStatus;
	message?: string;
	at: string;
}

/** One page of a paged list, with the total across all pages. */
export interface Page<T> {
	items: T[];
	total: number;
}

export interface Notification {
	id: string;
	name: string;
	type: string;
	isDefault: boolean;
	enabled: boolean;
	config: Record<string, unknown>;
	createdAt: string;
}

export type MaintenanceStrategy = 'single' | 'recurring';

/**
 * Recurrence payload for `strategy: 'recurring'`. The shape is fixed by the
 * server's validator (internal/maintenance/recurrence.go), which decodes
 * strictly and rejects unknown fields — so these names are not free to change
 * on this side alone:
 *
 * - `weekdays` are `time.Weekday` values, 0 = Sunday … 6 = Saturday, no dupes.
 * - `start` is `HH:MM` wall clock in the window's own `timezone`, not UTC.
 * - `durationMinutes` is elapsed time from that start (wire: duration_minutes),
 *   so an occurrence may cross midnight; the server caps it at 7 days.
 */
export interface MaintenanceRecurrence {
	weekdays: number[];
	start: string;
	durationMinutes: number;
}

/**
 * `startsAt`/`endsAt` are the window itself for strategy `single`. For
 * `recurring` they are optional bounds on the series and are empty when the
 * series runs indefinitely.
 */
export interface MaintenanceWindow {
	id: string;
	title: string;
	description?: string;
	monitorIds: string[];
	strategy: MaintenanceStrategy;
	startsAt: string;
	endsAt: string;
	recurrence?: MaintenanceRecurrence;
	timezone: string;
	active: boolean;
}

/**
 * One audit_log row. `actor` and `target` are the server's numeric user_id and
 * entity_id rendered as strings — the API does not join display names, so the
 * activity screen shows ids. Resolving them to names needs a server-side join,
 * not a mapper change.
 */
export interface AuditEntry {
	id: string;
	actor: string;
	action: string;
	target: string;
	targetType: string;
	at: string;
}

/**
 * Build metadata from GET /version (internal/api/handlers.go handleVersion),
 * shown in the settings' "About" section. `version` is "dev" for an unstamped
 * build — the ldflags that set it are only passed by the release workflow.
 */
export interface AppVersion {
	version: string;
	commit: string;
	buildDate: string;
	goVersion: string;
}

/**
 * The settings screen's flat view of the server's key/value settings rows. Each
 * field maps to one row: GET /settings returns every row and settingsFromWire
 * folds them into this object; updateSettings PUTs the changed keys one at a
 * time (PUT /settings/{key}), which is the only write the server offers.
 */
export interface Settings {
	instanceName: string;
	baseUrl: string;
	retentionDays: number;
	defaultInterval: number;
	certExpiryThresholds: number[];
	/** How long a sign-in stays valid, in days. Server clamps to 1..365. */
	sessionTtlDays: number;
	/**
	 * Collector endpoint that receives one JSON POST per audit entry (SIEM, log
	 * shipper). Empty disables forwarding, which is the default. The server
	 * accepts only an absolute http/https URL.
	 */
	auditWebhookUrl: string;
	/**
	 * Receives one JSON POST per monitor state change (automation platforms:
	 * n8n, Node-RED, …). Empty disables it; same URL rules as the audit feed.
	 */
	outboundWebhookUrl: string;
	/**
	 * Whether each feed has an HMAC signing secret. READ-ONLY: the secrets
	 * themselves are write-only (api.setWebhookSecret) and never come back from
	 * the server, which reports only whether one is set.
	 */
	auditWebhookSecretSet: boolean;
	outboundWebhookSecretSet: boolean;
}

/** The two outgoing webhook feeds, each with its own URL and signing secret. */
export type WebhookFeed = 'audit' | 'outbound';

/**
 * One monitor's TLS certificate as GET /report/certificates returns it
 * (internal/report.CertRow). Only monitors whose last check inspected a
 * certificate appear — a plain TCP monitor has none.
 *
 * `daysRemaining` is what the UI sorts and colours on. It goes NEGATIVE once the
 * certificate has expired, which is why `expired` is carried separately rather
 * than derived: a reader should not have to know that -3 means "three days too
 * late" to notice something is wrong.
 */
export interface CertificateRow {
	monitorId: string;
	monitorName: string;
	subject: string;
	issuer: string;
	validTo: string;
	daysRemaining: number;
	expired: boolean;
}

// ---- Configuration import / export ----------------------------------------

/**
 * What an import created, as POST /import and POST /import/uptime-kuma answer
 * (internal/api/importexport.go importResult). Counts are of entities actually
 * created; `warnings` lists every spec that was skipped (a name/slug conflict,
 * an unknown type) with the server's reason, and is [] when nothing was skipped.
 */
export interface ImportResult {
	monitors: number;
	notifications: number;
	tags: number;
	statusPages: number;
	warnings: string[];
}

/**
 * The configuration bundle GET /export downloads and POST /import consumes.
 *
 * It is a WIRE shape, not a model: snake_case keys straight from
 * internal/importexport.Bundle, because the file the user re-uploads is exactly
 * what the server emitted. It therefore rides through importConfig() UNMAPPED —
 * the only request body in this module passed through verbatim — so these names
 * must track the Go json tags, not the camelCase model.
 */
export interface ConfigMonitorSpec {
	name: string;
	type: string;
	config?: Record<string, unknown>;
	interval_seconds: number;
	timeout_seconds?: number;
	max_retries: number;
	recovery_checks?: number;
	retry_interval_seconds: number;
	resend_interval_minutes: number;
	upside_down: boolean;
	active: boolean;
	description?: string;
	link_url?: string;
	tags?: string[];
}

export interface ConfigNotificationSpec {
	name: string;
	type: string;
	config?: Record<string, unknown>;
	is_default: boolean;
	active: boolean;
}

export interface ConfigTagSpec {
	name: string;
	color: string;
}

export interface ConfigStatusPageSpec {
	slug: string;
	title: string;
	description?: string;
	footer_text?: string;
	theme?: string;
	published: boolean;
	/** Monitor names, resolved against the monitors created in the same import. */
	monitors?: string[];
}

export interface ConfigBundle {
	version: number;
	exported_at: string;
	monitors: ConfigMonitorSpec[];
	notifications: ConfigNotificationSpec[];
	tags: ConfigTagSpec[];
	status_pages: ConfigStatusPageSpec[];
}

/** RFC-7807 problem+json — surfaced as ApiError.problem. */
export interface Problem {
	type?: string;
	title: string;
	status: number;
	detail?: string;
	instance?: string;
}

// ---- Status pages ---------------------------------------------------------

export type StatusPageTheme = 'light' | 'dark' | 'auto';

export interface StatusPageMonitorRef {
	monitorId: string;
	/**
	 * Server-assigned, read-only: groups are written by name on save, but the
	 * response fills the id back in. Partition by this rather than by groupName —
	 * two groups may share a name, and a rename would silently orphan monitors.
	 */
	groupId?: string;
	groupName?: string;
	showUptime: boolean;
	sortOrder?: number;
}

export interface StatusPageGroupInput {
	/**
	 * Server-assigned; absent for groups being created client-side. The server
	 * recreates group rows on every page save, so an id is only stable until the
	 * next updateStatusPage call — which is what makes it a usable concurrency
	 * token for a layout reorder.
	 */
	id?: string;
	name: string;
	sortOrder?: number;
}

/** A complete re-statement of a page's display order (PATCH .../layout). */
export interface StatusPageLayoutOrder {
	ungrouped: string[];
	groups: { groupId: string; monitorIds: string[] }[];
}

/** A managed status page (dashboard side). */
export interface StatusPage {
	id: string;
	slug: string;
	title: string;
	description: string;
	footerText: string;
	theme: StatusPageTheme;
	/** Public page language (BCP-47, e.g. "de"); empty follows the default. */
	locale?: string;
	/** Brand colour as #rrggbb; empty follows the product accent. */
	accentColor?: string;
	/** Server-owned; set only via uploadStatusPageLogo/deleteStatusPageLogo. Read-only from the editor's point of view. */
	logoPath?: string;
	customDomain?: string;
	published: boolean;
	allowIndexing: boolean;
	hasPassword: boolean;
	viewCount: number;
	monitorIds: string[];
	groups: StatusPageGroupInput[];
	monitors: StatusPageMonitorRef[];
	createdAt: string;
	updatedAt: string;
}

/**
 * What the editor submits: the page fields it owns plus the write-only password
 * controls the response never echoes back. This is the parameter type of the
 * camel→snake wire mapper, so neither the create nor the update path needs a
 * cast. `logoPath` is present but server-owned and is never sent.
 */
export type StatusPageInput = Partial<StatusPage> & {
	password?: string;
	clearPassword?: boolean;
};

export type IncidentStatus = 'investigating' | 'identified' | 'monitoring' | 'resolved';
export type IncidentSeverity = 'minor' | 'major' | 'critical';

export interface IncidentUpdate {
	id: string;
	status: IncidentStatus;
	body: string;
	createdAt: string;
}

export interface Incident {
	id: string;
	statusPageId: string;
	title: string;
	body: string;
	severity: IncidentSeverity;
	status: IncidentStatus;
	pinned: boolean;
	startedAt: string;
	resolvedAt?: string;
	monitorIds: string[];
	updates: IncidentUpdate[];
	/**
	 * The retrospective written after the incident closed (Markdown), or '' when
	 * none exists. Server-owned in the same sense as StatusPage.logoPath: it is
	 * written only by setIncidentPostmortem, and updateIncident neither sends nor
	 * clears it.
	 */
	postmortem: string;
	/**
	 * When the retrospective was published; undefined means it is still a draft.
	 * The public page carries `postmortem` only when this is set — the server
	 * strips an unpublished one before the payload leaves.
	 */
	postmortemPublishedAt?: string;
}

/** The body of a postmortem write. */
export interface PostmortemInput {
	body: string;
	/**
	 * Publish to the public status page. Requires a non-empty body and a resolved
	 * incident; the server answers 409 otherwise.
	 */
	published: boolean;
}

// ---- Public status page (matches the Go PublicPageView JSON, snake_case) ---

/**
 * One uptime bar. `start` is the bucket boundary (RFC3339, UTC) and the
 * unambiguous key/label for any granularity — hourly, daily or monthly — while
 * `date` is kept as its UTC day for the legacy 90-day heatmap. Which unit a
 * bucket represents is carried once, page-level, as `PublicPage.bucket`.
 */
export interface PublicDayBar {
	start: string;
	date: string;
	status: 'up' | 'down' | 'degraded' | 'none';
	uptime: number;
}

export interface PublicMonitorView {
	id: number;
	name: string;
	status: MonitorStatus;
	show_uptime: boolean;
	uptime: number;
	heatmap: PublicDayBar[];
}

export interface PublicGroupView {
	/** Key rendered lists on this, never on name — group names are not unique. */
	id: number;
	name: string;
	monitors: PublicMonitorView[];
}

export interface PublicIncidentUpdate {
	status: IncidentStatus;
	body: string;
	created_at: string;
}

export interface PublicIncident {
	id: number;
	title: string;
	body: string;
	severity: IncidentSeverity;
	status: IncidentStatus;
	started_at: string;
	resolved_at?: string;
	updates: PublicIncidentUpdate[];
	/**
	 * The published retrospective, absent unless postmortem_published_at is set:
	 * the server strips a draft before this payload is built, so anything present
	 * here is public by an explicit act.
	 */
	postmortem?: string;
	postmortem_published_at?: string;
}

/**
 * One maintenance window as shown to anonymous visitors (matches the Go
 * PublicMaintenance JSON). Only windows in effect now or still ahead, and that
 * cover a monitor on this page, are ever included. `body` is the window's public
 * description (the Go field is `Body`, tagged `body` — NOT `description`);
 * `active` is true when now falls inside the window; `monitor_ids` are the
 * monitors it is scoped to ([] means every monitor on the page).
 */
export interface PublicMaintenance {
	id: number;
	title: string;
	body: string;
	starts_at?: string;
	ends_at?: string;
	active: boolean;
	monitor_ids: number[];
}

/**
 * The availability archive (GET /public/status/{slug}/history).
 *
 * `months`, `overall` and every monitor's `months` are PARALLEL arrays: index i
 * is the same calendar month in all three, so the client renders a table by
 * index without matching timestamps. A month with no conclusive data is -1, not
 * 0 and not 100 — an archive that invents availability for a month a monitor did
 * not exist in is worse than one that says nothing.
 *
 * The buckets reuse PublicDayBar: it is the same shape at a coarser granularity,
 * and its `start` is documented as the key for any bucket unit.
 */
export interface PublicHistoryMonitor {
	id: number;
	name: string;
	months: PublicDayBar[];
	uptime: number;
}

export interface PublicHistory {
	slug: string;
	title: string;
	theme: string;
	logo_path?: string;
	accent_color?: string;
	locale?: string;
	allow_indexing: boolean;
	generated_at: string;
	/** Bucket starts, oldest first, RFC3339 UTC on the first of each month. */
	months: string[];
	overall: number[];
	monitors: PublicHistoryMonitor[];
}

export type OverallStatus = 'operational' | 'degraded' | 'major_outage' | 'maintenance';

/** Selectable uptime window on the public page. Mirrors the Go range tokens. */
export type PublicRange = '24h' | '7d' | '30d' | '90d' | '1y';

/** Granularity every uptime bar is aggregated to for the resolved range. */
export type PublicBucket = 'hour' | 'day' | 'month';

export interface PublicPage {
	slug: string;
	title: string;
	description: string;
	footer_text: string;
	theme: string;
	logo_path?: string;
	accent_color?: string;
	locale?: string;
	allow_indexing: boolean;
	overall_status: OverallStatus;
	groups: PublicGroupView[];
	ungrouped: PublicMonitorView[];
	active_incidents: PublicIncident[];
	recent_incidents: PublicIncident[];
	maintenance: PublicMaintenance[];
	/**
	 * The resolved uptime window and the granularity every monitor's bars are
	 * aggregated to. The page renders and labels the bars against these two, so a
	 * request that omitted the range still knows which window it got back.
	 */
	range: PublicRange;
	bucket: PublicBucket;
	generated_at: string;
}
