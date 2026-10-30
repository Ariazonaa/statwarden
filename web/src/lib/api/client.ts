/**
 * Typed API client. By default it talks to the in-memory mock backend
 * (./mock.ts) so the dashboard runs without the Go server. Set
 * `VITE_USE_MOCK=false` at build time to hit the real REST API at /api/v1
 * instead — every mock method below has a matching real-fetch path.
 *
 * Real requests send credentials (session cookie) and a custom CSRF header,
 * and translate RFC-7807 problem+json bodies into ApiError.
 */
import type {
	ApiKey,
	AppVersion,
	AuditEntry,
	CertificateRow,
	BulkMonitorAction,
	BulkMonitorInput,
	BulkMonitorResponse,
	ConfigBundle,
	Heartbeat,
	ImportResult,
	Incident,
	IncidentStatus,
	IncidentUpdate,
	MaintenanceWindow,
	Monitor,
	MonitorBadge,
	MonitorEvent,
	MonitorGroup,
	MonitorStatus,
	Notification,
	OidcIdentity,
	OidcProvider,
	Problem,
	Role,
	Session,
	Settings,
	WebhookFeed,
	StateEvent,
	StatusPage,
	StatusPageInput,
	Tag,
	StepUpConfirmation,
	TotpEnrollment,
	User
} from './types';
import { mockApi } from './mock';

/**
 * JSON.parse that returns undefined instead of throwing.
 *
 * A response body is not guaranteed to be JSON — an error can be produced by a
 * proxy in front of this app, long before any handler runs. The caller decides
 * what a missing body means from the status code.
 */
function parseJSONOrUndefined(text: string): unknown {
	if (!text) return undefined;
	try {
		return JSON.parse(text);
	} catch {
		return undefined;
	}
}

export const USE_MOCK = import.meta.env.VITE_USE_MOCK !== 'false';

const BASE = '/api/v1';

/**
 * The largest page the server will return (internal/api/respond.go's maxLimit).
 *
 * Asked for explicitly, because the default is 50 and every list screen here
 * renders whatever it is given without pagination.
 */
const maxListLimit = 200;

export class ApiError extends Error {
	problem: Problem;
	constructor(problem: Problem) {
		super(problem.detail ?? problem.title);
		this.name = 'ApiError';
		this.problem = problem;
	}
	get status() {
		return this.problem.status;
	}
}

/**
 * Status code of a rejected api call. The mock backend throws plain
 * problem-shaped objects rather than ApiError instances, so `e instanceof
 * ApiError` alone silently misses every mock failure — anything branching on a
 * status (a 409 stale-layout rollback, a 404) must read it through here.
 */
export function problemStatus(e: unknown): number | undefined {
	if (e instanceof ApiError) return e.status;
	if (typeof e === 'object' && e !== null && 'status' in e) {
		const status = (e as { status?: unknown }).status;
		if (typeof status === 'number') return status;
	}
	return undefined;
}

/**
 * The server's client-facing explanation of a rejected call — problem+json
 * `detail`, falling back to `title`. Same two-shape problem as problemStatus:
 * the mock throws plain problem-shaped objects, so reading `e.message` alone
 * misses every mock failure and `e.problem` misses it differently.
 *
 * Callers surface this instead of a generic "could not save": a 400 that names
 * the offending field (a parent_id cycle, say) is the only thing that tells the
 * operator WHICH input to change.
 */
export function problemMessage(e: unknown): string | undefined {
	const p = e instanceof ApiError ? e.problem : e;
	if (typeof p !== 'object' || p === null) return undefined;
	const { detail, title } = p as { detail?: unknown; title?: unknown };
	if (typeof detail === 'string' && detail) return detail;
	if (typeof title === 'string' && title) return title;
	return undefined;
}

type Method = 'GET' | 'POST' | 'PATCH' | 'PUT' | 'DELETE';

const CSRF_COOKIE = 'statwarden_csrf';

/**
 * Reads the double-submit CSRF token the server sets as a deliberately
 * non-HttpOnly cookie. Every mutating request must echo it in the X-CSRF-Token
 * header; a cross-site page can send the cookie but cannot read it. The value
 * is base64url, so it is never percent-encoded — and the server compares raw
 * bytes, so it must not be decoded here. Returns '' during SSR (no document),
 * which is harmless: mutations only ever run in the browser.
 */
export function csrfToken(): string {
	if (typeof document === 'undefined') return '';
	for (const part of document.cookie.split('; ')) {
		if (part.startsWith(`${CSRF_COOKIE}=`)) return part.slice(CSRF_COOKIE.length + 1);
	}
	return '';
}

async function http<T>(
	method: Method,
	path: string,
	body?: unknown,
	extraHeaders?: Record<string, string>
): Promise<T> {
	const res = await fetch(`${BASE}${path}`, {
		method,
		credentials: 'same-origin',
		headers: {
			'Content-Type': 'application/json',
			// CSRF: the server matches this against the statwarden_csrf cookie.
			'X-CSRF-Token': csrfToken(),
			...extraHeaders
		},
		body: body != null ? JSON.stringify(body) : undefined
	});
	if (res.status === 204) return undefined as T;
	const text = await res.text();
	// Parsed defensively, and AFTER the status is known.
	//
	// This used to be `text ? JSON.parse(text) : undefined` on the line above the
	// `!res.ok` branch, so any non-empty non-JSON error body threw a raw
	// SyntaxError instead of an ApiError — an nginx 502 page, a 413 from a proxy,
	// a gateway timeout. problemStatus() and problemMessage() both return
	// undefined for a SyntaxError, so every status-based branch in the app
	// silently missed: the 409 layout rollback, the 409 postmortem message, the
	// 403 purge message. The Problem fallback built for exactly this case was
	// unreachable code.
	const json = parseJSONOrUndefined(text);
	if (!res.ok) {
		const problem: Problem =
			json && typeof json === 'object' && 'title' in json
				? (json as Problem)
				: { title: res.statusText || 'Request failed', status: res.status };
		throw new ApiError(problem);
	}
	return json as T;
}

/**
 * Raw-body sibling of http(): POSTs bytes verbatim with an octet-stream
 * content type. Used for the Uptime-Kuma SQLite database import, where the
 * server reads the request body directly (a legacy JSON export also rides this
 * path — the server auto-detects by the SQLite magic bytes). CSRF, credentials
 * and the 204 / problem+json handling match http() exactly.
 */
async function raw<T>(path: string, data: ArrayBuffer): Promise<T> {
	const res = await fetch(`${BASE}${path}`, {
		method: 'POST',
		credentials: 'same-origin',
		headers: {
			'Content-Type': 'application/octet-stream',
			'X-CSRF-Token': csrfToken()
		},
		body: data
	});
	if (res.status === 204) return undefined as T;
	const text = await res.text();
	// Parsed defensively, and AFTER the status is known.
	//
	// This used to be `text ? JSON.parse(text) : undefined` on the line above the
	// `!res.ok` branch, so any non-empty non-JSON error body threw a raw
	// SyntaxError instead of an ApiError — an nginx 502 page, a 413 from a proxy,
	// a gateway timeout. problemStatus() and problemMessage() both return
	// undefined for a SyntaxError, so every status-based branch in the app
	// silently missed: the 409 layout rollback, the 409 postmortem message, the
	// 403 purge message. The Problem fallback built for exactly this case was
	// unreachable code.
	const json = parseJSONOrUndefined(text);
	if (!res.ok) {
		const problem: Problem =
			json && typeof json === 'object' && 'title' in json
				? (json as Problem)
				: { title: res.statusText || 'Request failed', status: res.status };
		throw new ApiError(problem);
	}
	return json as T;
}

/**
 * Multipart sibling of http(). Content-Type is deliberately NOT set: only the
 * browser can generate the multipart boundary, and naming the header by hand
 * produces a body the Go side cannot parse. Everything else — credentials, the
 * CSRF header, the 204 / problem+json handling — matches http() exactly.
 */
async function upload<T>(path: string, field: string, file: File): Promise<T> {
	const fd = new FormData();
	fd.append(field, file);
	const res = await fetch(`${BASE}${path}`, {
		method: 'POST',
		credentials: 'same-origin',
		headers: {
			// CSRF: the server matches this against the statwarden_csrf cookie.
			'X-CSRF-Token': csrfToken()
		},
		body: fd
	});
	if (res.status === 204) return undefined as T;
	const text = await res.text();
	// Parsed defensively, and AFTER the status is known.
	//
	// This used to be `text ? JSON.parse(text) : undefined` on the line above the
	// `!res.ok` branch, so any non-empty non-JSON error body threw a raw
	// SyntaxError instead of an ApiError — an nginx 502 page, a 413 from a proxy,
	// a gateway timeout. problemStatus() and problemMessage() both return
	// undefined for a SyntaxError, so every status-based branch in the app
	// silently missed: the 409 layout rollback, the 409 postmortem message, the
	// 403 purge message. The Problem fallback built for exactly this case was
	// unreachable code.
	const json = parseJSONOrUndefined(text);
	if (!res.ok) {
		const problem: Problem =
			json && typeof json === 'object' && 'title' in json
				? (json as Problem)
				: { title: res.statusText || 'Upload failed', status: res.status };
		throw new ApiError(problem);
	}
	return json as T;
}

// ---- same-origin file downloads -------------------------------------------

/**
 * A named report window, matching parseReportRange on the server
 * (internal/api/report.go): a `range` token, or an explicit RFC3339 `from`/`to`
 * pair which takes precedence when present.
 */
export type ReportRange = '24h' | '7d' | '30d' | '1y';
export interface ReportWindow {
	range?: ReportRange;
	from?: string;
	to?: string;
}

/**
 * Config export (GET /export) and the SLA reports (GET /report/sla.csv|.pdf) are
 * same-origin file DOWNLOADS, not JSON APIs: the server streams them with a
 * Content-Disposition filename and the browser saves them. There is nothing to
 * fetch() or map — a component renders `<a href download>` — so these helpers
 * only BUILD the URL (BASE-prefixed, with any range query). They deliberately do
 * NOT route through USE_MOCK: a mock build has no server to stream a file, so
 * these links resolve only against the real backend.
 */
export function configExportUrl(): string {
	return `${BASE}/export`;
}

/** URL for the SLA report in the given format over the given window. */
export function slaReportUrl(format: 'csv' | 'pdf', window?: ReportWindow): string {
	const qs = new URLSearchParams();
	// from/to win over range on the server, so mirror that precedence here and
	// never send both — a stray range would just be ignored, but sending only
	// what the caller meant keeps the request legible in the network log.
	if (window?.from) {
		qs.set('from', window.from);
		if (window.to) qs.set('to', window.to);
	} else if (window?.range) {
		qs.set('range', window.range);
	}
	const q = qs.toString();
	return `${BASE}/report/sla.${format}${q ? `?${q}` : ''}`;
}

/**
 * URL for the audit-log CSV export over an optional window.
 *
 * A same-origin download like the two above, so it only builds the URL. The
 * window is NOT the ReportWindow shape: the audit export has no named ranges and
 * no default window — omitting both bounds means "everything", which is what an
 * operator assembling evidence asks for. Bounds are RFC3339.
 *
 * The response may carry `X-Statwarden-Export-Truncated: true` when the window
 * exceeded the server's row cap. A plain `<a download>` cannot read that header,
 * which is why the UI narrows the window instead of guessing.
 */
export function auditLogExportUrl(window?: { from?: string; to?: string }): string {
	const qs = new URLSearchParams();
	if (window?.from) qs.set('from', window.from);
	if (window?.to) qs.set('to', window.to);
	const q = qs.toString();
	return `${BASE}/audit-log.csv${q ? `?${q}` : ''}`;
}

// ---- wire boundary --------------------------------------------------------

/**
 * Every mapper below is written by hand, one per entity, on purpose. A generic
 * snake↔camel transform would look like less code and be far worse: it reshapes
 * fields nobody audited, it makes `grep uptime_24h` find nothing, and it mangles
 * the names that are not word-separated the way it assumes (uptime24h →
 * uptime_24_h, latencyMs → latency_ms only by luck). Explicit mappers also make
 * the gaps visible — a wire field with no model counterpart, or the reverse, is
 * a line you have to write and therefore a decision you have to make.
 *
 * decodeJSON on the Go side uses DisallowUnknownFields, so a stray or camelCase
 * key on a WRITE yields an opaque 400 rather than being ignored. On a READ an
 * unmapped key is silently undefined, which is exactly how this survived.
 */

/** Model id (string) → wire id (int64 as a JSON number). */
function wireId(v: string): number {
	const n = Number(v);
	if (!Number.isFinite(n)) throw new Error(`non-numeric id ${v}; the real API uses int64 ids`);
	return n;
}

/** Wire id → model id. The single place a number becomes an id string. */
const modelId = (n: number): string => String(n);

/**
 * Paginated list envelope shared by every Go list endpoint that uses
 * writeList() — `{items, total, limit, offset}`. `items` is null, not [], when
 * the page is empty, because Go marshals a nil slice as null.
 */
interface WireList<T> {
	items: T[] | null;
	total: number;
	limit: number;
	offset: number;
}

/** Envelope used by the endpoints that wrap items without pagination. */
interface WireItems<T> {
	items: T[] | null;
}

// ---- users, sessions, api keys --------------------------------------------

interface WireUser {
	id: number;
	email: string;
	username: string;
	display_name: string;
	role: string;
	totp_enabled: boolean;
	avatar_path?: string;
	locale?: string;
	is_active: boolean;
	created_at: string;
	updated_at: string;
}

/**
 * `username` is the server's real login name now, not `display_name` (which is
 * kept in lockstep with it on write). `lastLoginAt` is deliberately not set: the
 * users table has no such column (see types.ts). Inventing one here — from
 * updated_at, say — would be a lie the UI renders as fact.
 */
function userFromWire(w: WireUser): User {
	return {
		id: modelId(w.id),
		username: w.username,
		email: w.email,
		role: w.role as Role,
		avatarUrl: w.avatar_path,
		totpEnabled: w.totp_enabled,
		disabled: !w.is_active,
		createdAt: w.created_at
	};
}

interface WireOidcProvider {
	id: string;
	display_name: string;
}

function oidcProviderFromWire(w: WireOidcProvider): OidcProvider {
	return { id: w.id, displayName: w.display_name };
}

/**
 * POST /users body (createUserRequest in internal/api/users.go): the server
 * hashes `password` and rejects it as missing, so it is always sent. There is
 * NO `is_active` on create — the account starts active, and decodeJSON's
 * DisallowUnknownFields turns a stray `is_active` into an opaque 400, so the two
 * write bodies must stay separate.
 */
function createUserToWire(i: Partial<User> & { password?: string }): Record<string, unknown> {
	return {
		email: i.email ?? '',
		username: i.username ?? '',
		// display_name is kept equal to the username: the UI models one name, and
		// the server column stays populated rather than drifting to empty.
		display_name: i.username ?? '',
		role: i.role ?? 'viewer',
		password: i.password ?? ''
	};
}

/**
 * PUT /users/{id} body (updateUserRequest): a full-replace of the editable
 * fields plus `is_active` (the inverse of the model's `disabled`). No
 * `password` — the update DTO has no such field and would reject it.
 */
function updateUserToWire(i: Partial<User>): Record<string, unknown> {
	return {
		email: i.email ?? '',
		username: i.username ?? '',
		// display_name is kept equal to the username (see createUserToWire).
		display_name: i.username ?? '',
		role: i.role ?? 'viewer',
		is_active: !(i.disabled ?? false)
	};
}

interface WireSession {
	id: string;
	user_id: number;
	created_at: string;
	expires_at: string;
	last_seen_at: string;
	ip?: string;
	user_agent?: string;
	current: boolean;
}

function sessionFromWire(w: WireSession): Session {
	return {
		id: w.id,
		userAgent: w.user_agent ?? '',
		ip: w.ip ?? '',
		current: w.current,
		createdAt: w.created_at,
		lastSeenAt: w.last_seen_at
	};
}

interface WireApiKey {
	id: number;
	user_id: number;
	name: string;
	prefix: string;
	scopes: string[];
	last_used_at?: string;
	created_at: string;
	expires_at?: string;
}

function apiKeyFromWire(w: WireApiKey, secret?: string): ApiKey {
	return {
		id: modelId(w.id),
		name: w.name,
		scopes: w.scopes ?? [],
		prefix: w.prefix,
		token: secret,
		lastUsedAt: w.last_used_at,
		createdAt: w.created_at,
		expiresAt: w.expires_at
	};
}

// ---- TOTP two-factor & linked identities ----------------------------------

interface WireTotpEnrollment {
	secret: string;
	otpauth_uri: string;
}

function totpEnrollmentFromWire(w: WireTotpEnrollment): TotpEnrollment {
	return { secret: w.secret, otpauthUri: w.otpauth_uri };
}

/**
 * Both verify and regenerate answer only the fresh backup codes, shown once.
 * A nil slice marshals as null, so the mapper widens it to [] — a caller
 * rendering "0 codes" from null would be a bug, never an intended state.
 */
interface WireBackupCodes {
	backup_codes: string[] | null;
}

/**
 * ExternalIdentity as internal/api/store.go serialises it: the numeric id and
 * the (issuer, subject) join key, plus display-only email/name claims. UserID is
 * `json:"-"` on the Go side, so there is nothing here to map beyond these.
 */
interface WireIdentity {
	id: number;
	provider: string;
	issuer: string;
	subject: string;
	email?: string;
	display_name?: string;
	created_at: string;
	last_login_at?: string;
}

function identityFromWire(w: WireIdentity): OidcIdentity {
	return {
		id: modelId(w.id),
		provider: w.provider,
		issuer: w.issuer,
		subject: w.subject,
		email: w.email || undefined,
		displayName: w.display_name || undefined,
		createdAt: w.created_at,
		lastLoginAt: w.last_login_at
	};
}

// ---- monitors -------------------------------------------------------------

interface WireMonitor {
	id: number;
	name: string;
	type: string;
	config: Record<string, unknown>;
	interval_seconds: number;
	timeout_seconds: number | null;
	max_retries: number;
	recovery_checks: number;
	retry_interval_seconds: number;
	resend_interval_minutes: number;
	upside_down: boolean;
	active: boolean;
	status: string;
	parent_id: number | null;
	group_id: number | null;
	push_token?: string;
	scheduling_error?: string;
	description: string;
	link_url: string;
	tag_ids: number[] | null;
	notification_ids: number[] | null;
	url: string;
	created_at: string;
	latency_ms: number | null;
	last_checked_at: string | null;
	uptime_24h: number | null;
	uptime_30d: number | null;
	uptime_1y: number | null;
	sparkline: number[] | null;
	recent_beats: string[] | null;
}

/** Wire 0..1 uptime fraction (null = no data) → model 0..100 percentage. */
const uptimePct = (v: number | null): number | undefined => (v == null ? undefined : v * 100);

function monitorFromWire(w: WireMonitor): Monitor {
	return {
		id: modelId(w.id),
		name: w.name,
		type: w.type,
		url: w.url || undefined,
		// The push token is revealed exactly once, in the create response, and is
		// blanked on every later read. Dropping it here made a push monitor
		// created in the dashboard permanently unusable: /push/{token} is the
		// whole point of the type, and the one response that carried the
		// credential was thrown away.
		pushToken: w.push_token || undefined,
		// Set when the row was written but the engine REFUSED to schedule it — an
		// unopenable config, a type it does not know. Dropping it rendered that
		// monitor as an ordinary healthy card while nothing was checking it,
		// which is exactly the condition the field exists to report.
		schedulingError: w.scheduling_error || undefined,
		status: w.status as MonitorStatus,
		active: w.active,
		interval: w.interval_seconds,
		timeout: w.timeout_seconds ?? 0,
		maxRetries: w.max_retries,
		// A server predating the field omits it; its behaviour then IS one.
		recoveryChecks: w.recovery_checks ?? 1,
		retryInterval: w.retry_interval_seconds,
		resendInterval: w.resend_interval_minutes,
		upsideDown: w.upside_down,
		tagIds: (w.tag_ids ?? []).map(modelId),
		notificationIds: (w.notification_ids ?? []).map(modelId),
		parentId: w.parent_id != null ? modelId(w.parent_id) : undefined,
		groupId: w.group_id != null ? modelId(w.group_id) : undefined,
		description: w.description || undefined,
		externalUrl: w.link_url || undefined,
		config: w.config ?? {},
		latencyMs: w.latency_ms ?? undefined,
		uptime24h: uptimePct(w.uptime_24h),
		uptime30d: uptimePct(w.uptime_30d),
		uptime1y: uptimePct(w.uptime_1y),
		sparkline: w.sparkline ?? [],
		recentBeats: (w.recent_beats ?? []) as MonitorStatus[],
		lastCheckedAt: w.last_checked_at ?? undefined,
		createdAt: w.created_at
	};
}

/**
 * monitorRequest (internal/api/monitors.go) is full-replace on both POST and
 * PUT, so every field it knows is sent on every write. The derived fields —
 * url, status, sparkline, the uptimes, created_at — are absent: the server
 * rejects unknown keys, and they are computed, not stored.
 *
 * Full-replace also means the caller must hand over a COMPLETE monitor on an
 * update. A partial that omits, say, retryInterval does not leave the column
 * alone — it rewrites it from the fallback below. Editors spread the monitor
 * they loaded rather than sending only the fields they changed.
 */
function monitorToWire(i: Partial<Monitor>): Record<string, unknown> {
	return {
		name: i.name ?? '',
		type: i.type ?? 'http',
		config: i.config ?? {},
		interval_seconds: i.interval ?? 60,
		timeout_seconds: i.timeout ? i.timeout : null,
		max_retries: i.maxRetries ?? 0,
		recovery_checks: i.recoveryChecks ?? 1,
		retry_interval_seconds: i.retryInterval ?? i.interval ?? 60,
		resend_interval_minutes: i.resendInterval ?? 0,
		upside_down: i.upsideDown ?? false,
		active: i.active ?? true,
		parent_id: i.parentId != null ? wireId(i.parentId) : null,
		group_id: i.groupId != null ? wireId(i.groupId) : null,
		description: i.description ?? '',
		link_url: i.externalUrl ?? '',
		tag_ids: (i.tagIds ?? []).map(wireId),
		notification_ids: (i.notificationIds ?? []).map(wireId)
	};
}

interface WireHeartbeat {
	monitor_id: number;
	checked_at: string;
	status: string;
	latency_ms?: number;
	message?: string;
	important: boolean;
	retries: number;
}

function heartbeatFromWire(w: WireHeartbeat): Heartbeat {
	return {
		monitorId: modelId(w.monitor_id),
		status: w.status as MonitorStatus,
		latencyMs: w.latency_ms ?? null,
		message: w.message || undefined,
		checkedAt: w.checked_at
	};
}

/**
 * Derives the transition log from the important=true heartbeats, newest first —
 * there is no separate event resource on the server. `from` is the status of the
 * NEXT entry (the older transition) and `durationSec` is how long that previous
 * state lasted, which is only knowable by looking at the neighbour; that is why
 * this maps the whole array rather than one beat at a time.
 */
function eventsFromWire(beats: WireHeartbeat[]): StateEvent[] {
	return beats.map((w, n) => {
		const prev = beats[n + 1];
		return {
			id: `${w.monitor_id}:${w.checked_at}`,
			monitorId: modelId(w.monitor_id),
			from: prev ? (prev.status as MonitorStatus) : null,
			to: w.status as MonitorStatus,
			message: w.message || undefined,
			durationSec: prev
				? Math.max(0, Math.round((Date.parse(w.checked_at) - Date.parse(prev.checked_at)) / 1000))
				: undefined,
			at: w.checked_at
		};
	});
}

function monitorEventFromWire(w: WireHeartbeat): MonitorEvent {
	return {
		id: `${w.monitor_id}:${w.checked_at}`,
		monitorId: modelId(w.monitor_id),
		status: w.status as MonitorStatus,
		message: w.message || undefined,
		at: w.checked_at
	};
}

// ---- monitor badges -------------------------------------------------------

interface WireMonitorBadge {
	badge_token: string;
	status_url: string;
	uptime_url: string;
}

function monitorBadgeFromWire(w: WireMonitorBadge): MonitorBadge {
	return { token: w.badge_token, statusUrl: w.status_url, uptimeUrl: w.uptime_url };
}

// ---- configuration import -------------------------------------------------

interface WireImportResult {
	monitors: number;
	notifications: number;
	tags: number;
	status_pages: number;
	warnings?: string[] | null;
}

function importResultFromWire(w: WireImportResult): ImportResult {
	return {
		monitors: w.monitors,
		notifications: w.notifications,
		tags: w.tags,
		statusPages: w.status_pages,
		warnings: w.warnings ?? []
	};
}

// ---- tags, notifications, maintenance -------------------------------------

interface WireTag {
	id: number;
	name: string;
	color: string;
	created_at: string;
}

function tagFromWire(w: WireTag): Tag {
	return { id: modelId(w.id), name: w.name, color: w.color };
}

function tagToWire(i: Partial<Tag>): Record<string, unknown> {
	return { name: i.name ?? '', color: i.color ?? '' };
}

interface WireBulkResult {
	monitor_id: number;
	ok: boolean;
	error?: string;
}

interface WireBulkResponse {
	action: string;
	succeeded: number;
	failed: number;
	results: WireBulkResult[] | null;
}

/**
 * `group_id` is sent as an explicit null when the caller passes null, because
 * that is how the "group" action says "clear the folder" — omitting the key
 * would leave GroupID nil on the server, which means exactly the same thing
 * there, but being explicit keeps the two readable as the same instruction.
 * `tag_ids` / `notification_ids` are omitted entirely unless present: the server
 * rejects an empty list for the tag and notify actions, and sending `[]` for a
 * pause would be noise. The schedule fields are flattened onto the request the
 * way the Go bulkMonitorRequest carries them, and each optional override is sent
 * only when set so an omitted one keeps the monitor's stored value.
 *
 * Every key here is one decodeJSON's DisallowUnknownFields recognises, so a
 * stray field would be an opaque 400 rather than being ignored.
 */
function bulkToWire(i: BulkMonitorInput): Record<string, unknown> {
	const out: Record<string, unknown> = {
		action: i.action,
		monitor_ids: i.monitorIds.map(wireId)
	};
	if (i.tagIds) out.tag_ids = i.tagIds.map(wireId);
	if (i.groupId !== undefined) out.group_id = i.groupId == null ? null : wireId(i.groupId);
	if (i.notificationIds) out.notification_ids = i.notificationIds.map(wireId);
	if (i.schedule) {
		out.interval_seconds = i.schedule.interval;
		if (i.schedule.timeout !== undefined) out.timeout_seconds = i.schedule.timeout;
		if (i.schedule.maxRetries !== undefined) out.max_retries = i.schedule.maxRetries;
		if (i.schedule.retryInterval !== undefined)
			out.retry_interval_seconds = i.schedule.retryInterval;
	}
	return out;
}

function bulkFromWire(w: WireBulkResponse): BulkMonitorResponse {
	return {
		action: w.action as BulkMonitorAction,
		succeeded: w.succeeded,
		failed: w.failed,
		results: (w.results ?? []).map((r) => ({
			monitorId: modelId(r.monitor_id),
			ok: r.ok,
			error: r.error || undefined
		}))
	};
}

interface WireMonitorGroup {
	id: number;
	name: string;
	sort_order: number;
	created_at: string;
}

/** `created_at` is dropped on purpose: nothing renders it, same as Tag. */
function monitorGroupFromWire(w: WireMonitorGroup): MonitorGroup {
	return { id: modelId(w.id), name: w.name, sortOrder: w.sort_order };
}

function monitorGroupToWire(i: Partial<MonitorGroup>): Record<string, unknown> {
	return { name: i.name ?? '', sort_order: i.sortOrder ?? 0 };
}

interface WireNotification {
	id: number;
	name: string;
	type: string;
	config: Record<string, unknown>;
	is_default: boolean;
	active: boolean;
	created_at: string;
	updated_at: string;
}

function notificationFromWire(w: WireNotification): Notification {
	return {
		id: modelId(w.id),
		name: w.name,
		type: w.type,
		isDefault: w.is_default,
		enabled: w.active,
		config: w.config ?? {},
		createdAt: w.created_at
	};
}

function notificationToWire(i: Partial<Notification>): Record<string, unknown> {
	const wire: Record<string, unknown> = {
		name: i.name ?? '',
		type: i.type ?? '',
		is_default: i.isDefault ?? false,
		active: i.enabled ?? true
	};
	// config is OMITTED when undefined, and the omission is the message: on
	// update the server reads an absent config as "leave the configuration
	// alone". A channel's config is hidden wholesale on read, so an editor
	// opened on an existing channel starts blank — sending that blank form
	// back as the new configuration is how a rename came to wipe an SMTP
	// password. `{}` still means "an empty configuration", which is why this
	// cannot default.
	if (i.config !== undefined) {
		wire.config = i.config;
	}
	return wire;
}

interface WireRecurrence {
	weekdays: number[];
	start: string;
	duration_minutes: number;
}

interface WireMaintenance {
	id: number;
	title: string;
	description: string;
	active: boolean;
	strategy: string;
	starts_at?: string;
	ends_at?: string;
	recurrence?: WireRecurrence | null;
	timezone: string;
	monitor_ids: number[] | null;
	created_at: string;
	updated_at: string;
}

function maintenanceFromWire(w: WireMaintenance): MaintenanceWindow {
	return {
		id: modelId(w.id),
		title: w.title,
		description: w.description || undefined,
		monitorIds: (w.monitor_ids ?? []).map(modelId),
		// Anything other than the one recurring value reads back as "single":
		// the column is constrained to those two, and defaulting the unknown
		// case to single keeps an unexpected value from presenting as a
		// recurring window with no recurrence to render.
		strategy: w.strategy === 'recurring' ? 'recurring' : 'single',
		startsAt: w.starts_at ?? '',
		endsAt: w.ends_at ?? '',
		recurrence: w.recurrence
			? {
					weekdays: w.recurrence.weekdays ?? [],
					start: w.recurrence.start,
					durationMinutes: w.recurrence.duration_minutes
				}
			: undefined,
		timezone: w.timezone || 'UTC',
		active: w.active
	};
}

/**
 * The Go update path is full-replace, so every field is written on every save —
 * omitting one would silently reset it server-side.
 *
 * `recurrence` is sent only for a recurring window and explicitly `null`
 * otherwise: the server validates the payload strictly, and leaving a stale
 * recurrence attached to a window switched back to `single` would be stored
 * even though nothing evaluates it.
 */
function maintenanceToWire(i: Partial<MaintenanceWindow>): Record<string, unknown> {
	const recurring = i.strategy === 'recurring';
	return {
		title: i.title ?? '',
		description: i.description ?? '',
		active: i.active ?? true,
		strategy: recurring ? 'recurring' : 'single',
		starts_at: i.startsAt || null,
		ends_at: i.endsAt || null,
		recurrence:
			recurring && i.recurrence
				? {
						weekdays: i.recurrence.weekdays,
						start: i.recurrence.start,
						duration_minutes: i.recurrence.durationMinutes
					}
				: null,
		timezone: i.timezone || 'UTC',
		monitor_ids: (i.monitorIds ?? []).map(wireId)
	};
}

// ---- audit log and settings ------------------------------------------------

interface WireAuditEntry {
	id: number;
	user_id?: number;
	action: string;
	entity_type?: string;
	entity_id?: number;
	details?: unknown;
	ip?: string;
	created_at: string;
}

function auditEntryFromWire(w: WireAuditEntry): AuditEntry {
	return {
		id: modelId(w.id),
		actor: w.user_id != null ? modelId(w.user_id) : 'system',
		action: w.action,
		target: w.entity_id != null ? modelId(w.entity_id) : '',
		targetType: w.entity_type ?? '',
		at: w.created_at
	};
}

interface WireSetting {
	key: string;
	value: unknown;
	updated_at: string;
}

/**
 * The settings row keys. The server stores opaque key/value JSON and never
 * names these, so this table IS the contract: a typo here reads as a default
 * and writes a row nothing else looks at.
 */
const SETTING_KEYS = {
	instanceName: 'instance_name',
	baseUrl: 'base_url',
	retentionDays: 'retention_days',
	defaultInterval: 'default_interval',
	certExpiryThresholds: 'cert_expiry_thresholds',
	sessionTtlDays: 'session_ttl_days',
	// Must match api.SettingAuditWebhookURL. A key spelled one way here and
	// another way there is a feature that is silently off forever: the screen
	// writes a row nothing reads, with no error anywhere to notice.
	auditWebhookUrl: 'audit_webhook_url',
	// Its twin, and it was missing. Both keys are exported by
	// internal/api/settings.go, validated by the same function, and read by
	// cmd/statwarden to build the two delivery queues — but only one had an entry
	// here, so the outbound webhook could be switched on only in psql. That
	// file's own comment warns that leaving one of a matched pair out is exactly
	// how the two drift.
	outboundWebhookUrl: 'outbound_webhook_url'
} as const;

/**
 * The signing-secret rows. Deliberately NOT in SETTING_KEYS: those are the
 * keys settingsToWire writes back, and the model only holds whether a secret is
 * set — writing that boolean over the secret would destroy it. Secrets are
 * written by setWebhookSecret alone. Must match api.Setting*WebhookSecret.
 */
const SECRET_KEYS: Record<WebhookFeed, string> = {
	audit: 'audit_webhook_secret',
	outbound: 'outbound_webhook_secret'
};

const SETTINGS_DEFAULTS: Settings = {
	instanceName: 'Statwarden',
	baseUrl: '',
	retentionDays: 90,
	defaultInterval: 60,
	certExpiryThresholds: [21, 14, 7, 3, 1],
	sessionTtlDays: 30,
	auditWebhookUrl: '',
	outboundWebhookUrl: '',
	auditWebhookSecretSet: false,
	outboundWebhookSecretSet: false
};

/** Folds the key/value rows into the flat object the settings screen edits. */
function settingsFromWire(rows: WireSetting[]): Settings {
	const byKey = new Map(rows.map((r) => [r.key, r.value]));
	const read = <T>(key: string, fallback: T): T => {
		const v = byKey.get(key);
		return v === undefined || v === null ? fallback : (v as T);
	};
	return {
		instanceName: read(SETTING_KEYS.instanceName, SETTINGS_DEFAULTS.instanceName),
		baseUrl: read(SETTING_KEYS.baseUrl, SETTINGS_DEFAULTS.baseUrl),
		retentionDays: read(SETTING_KEYS.retentionDays, SETTINGS_DEFAULTS.retentionDays),
		defaultInterval: read(SETTING_KEYS.defaultInterval, SETTINGS_DEFAULTS.defaultInterval),
		certExpiryThresholds: read(
			SETTING_KEYS.certExpiryThresholds,
			SETTINGS_DEFAULTS.certExpiryThresholds
		),
		sessionTtlDays: read(SETTING_KEYS.sessionTtlDays, SETTINGS_DEFAULTS.sessionTtlDays),
		auditWebhookUrl: read(SETTING_KEYS.auditWebhookUrl, SETTINGS_DEFAULTS.auditWebhookUrl),
		outboundWebhookUrl: read(SETTING_KEYS.outboundWebhookUrl, SETTINGS_DEFAULTS.outboundWebhookUrl),
		// The server masks a secret row to true/false; anything else is "not set".
		auditWebhookSecretSet: byKey.get(SECRET_KEYS.audit) === true,
		outboundWebhookSecretSet: byKey.get(SECRET_KEYS.outbound) === true
	};
}

/** The (key, value) writes a settings patch expands into. */
function settingsToWire(i: Partial<Settings>): [string, unknown][] {
	const out: [string, unknown][] = [];
	for (const [field, key] of Object.entries(SETTING_KEYS) as [keyof Settings, string][]) {
		const v = i[field];
		if (v !== undefined) out.push([key, v]);
	}
	return out;
}

// ---- reporting ------------------------------------------------------------

interface WireCertRow {
	monitor_id: number;
	monitor_name: string;
	subject?: string;
	issuer?: string;
	valid_to: string;
	days_remaining: number;
	expired: boolean;
}

/**
 * Subject and issuer are `omitempty` on the Go side, so a certificate the
 * checker could read only partially arrives without them. Defaulted to '' here
 * so the widget never renders `undefined` into a cell.
 */
function certRowFromWire(w: WireCertRow): CertificateRow {
	return {
		monitorId: modelId(w.monitor_id),
		monitorName: w.monitor_name,
		subject: w.subject ?? '',
		issuer: w.issuer ?? '',
		validTo: w.valid_to,
		daysRemaining: w.days_remaining,
		expired: w.expired
	};
}

// ---- incidents ------------------------------------------------------------

interface WireIncidentUpdate {
	id: number;
	incident_id: number;
	status: string;
	body: string;
	created_at: string;
}

interface WireIncident {
	id: number;
	status_page_id: number;
	title: string;
	body: string;
	severity: string;
	status: string;
	pinned: boolean;
	started_at: string;
	resolved_at?: string;
	monitor_ids: number[] | null;
	updates?: WireIncidentUpdate[] | null;
	postmortem?: string;
	postmortem_published_at?: string;
	created_at: string;
	updated_at: string;
}

function incidentUpdateFromWire(w: WireIncidentUpdate): IncidentUpdate {
	return {
		id: modelId(w.id),
		status: w.status as IncidentStatus,
		body: w.body,
		createdAt: w.created_at
	};
}

function incidentFromWire(w: WireIncident): Incident {
	return {
		id: modelId(w.id),
		statusPageId: modelId(w.status_page_id),
		title: w.title,
		body: w.body,
		severity: w.severity as Incident['severity'],
		status: w.status as IncidentStatus,
		pinned: w.pinned,
		startedAt: w.started_at,
		resolvedAt: w.resolved_at,
		monitorIds: (w.monitor_ids ?? []).map(modelId),
		updates: (w.updates ?? []).map(incidentUpdateFromWire),
		// Omitted by the server when empty (`json:",omitempty"`), and omitted
		// ENTIRELY on the public payload for a draft. Defaulted to '' rather than
		// left undefined so the editor can bind a textarea to it directly.
		postmortem: w.postmortem ?? '',
		postmortemPublishedAt: w.postmortem_published_at
	};
}

/**
 * The incident write body. It carries NO postmortem field, on purpose: the
 * server's update handler is full-replace over the fields it does accept, and
 * the retrospective is written through its own endpoint (setIncidentPostmortem)
 * so a stale editor cannot erase one it never loaded. decodeJSON rejects unknown
 * fields, so sending it here would be a 400 rather than a silent no-op.
 */
function incidentToWire(i: Partial<Incident>): Record<string, unknown> {
	return {
		title: i.title ?? '',
		body: i.body ?? '',
		severity: i.severity ?? 'minor',
		status: i.status ?? 'investigating',
		pinned: i.pinned ?? false,
		monitor_ids: (i.monitorIds ?? []).map(wireId)
	};
}

/**
 * The status page as the Go admin API speaks it: snake_case keys and int64 ids
 * (see internal/api/types.go StatusPage). The dashboard model is camelCase with
 * string ids, so every status-page request and response is mapped at this
 * boundary — decodeJSON uses DisallowUnknownFields, so a stray or camelCase key
 * yields an opaque 400 rather than being ignored.
 */
interface WireStatusPage {
	id: number;
	slug: string;
	title: string;
	description: string;
	footer_text: string;
	theme: string;
	logo_path?: string;
	custom_domain?: string;
	locale?: string;
	accent_color?: string;
	published: boolean;
	allow_indexing: boolean;
	has_password: boolean;
	view_count: number;
	monitor_ids: number[] | null;
	groups?: { id: number; name: string; sort_order: number }[];
	monitors?: {
		monitor_id: number;
		group_id?: number;
		group_name?: string;
		sort_order: number;
		show_uptime: boolean;
	}[];
	created_at: string;
	updated_at: string;
}

/**
 * camel→snake for everything the editor owns. `logo_path` is deliberately
 * absent: it is server-owned and written only by the logo endpoints, so sending
 * it would re-establish the illusion that the editor controls it. SortOrder is
 * emitted per group and per monitor because it is authoritative for display
 * order — omitting it would let a plain Save wipe a drag-and-drop reorder.
 */
function statusPageToWire(i: StatusPageInput): Record<string, unknown> {
	const num = wireId;
	return {
		slug: i.slug,
		title: i.title,
		description: i.description ?? '',
		footer_text: i.footerText ?? '',
		theme: i.theme,
		accent_color: i.accentColor ?? '',
		custom_domain: i.customDomain ?? '',
		locale: i.locale ?? '',
		password: i.password ?? '',
		clear_password: i.clearPassword ?? false,
		published: i.published,
		allow_indexing: i.allowIndexing,
		monitor_ids: (i.monitorIds ?? []).map(num),
		groups: (i.groups ?? []).map((g, n) => ({ name: g.name, sort_order: g.sortOrder ?? n })),
		monitors: (i.monitors ?? []).map((m, n) => ({
			monitor_id: num(m.monitorId),
			group_name: m.groupName ?? '',
			sort_order: m.sortOrder ?? n,
			show_uptime: m.showUptime
		}))
	};
}

/** snake→camel, the inverse of statusPageToWire plus the server-owned fields. */
function statusPageFromWire(w: WireStatusPage): StatusPage {
	return {
		id: String(w.id),
		slug: w.slug,
		title: w.title,
		description: w.description,
		footerText: w.footer_text,
		theme: w.theme as StatusPage['theme'],
		locale: w.locale,
		accentColor: w.accent_color,
		logoPath: w.logo_path,
		customDomain: w.custom_domain,
		published: w.published,
		allowIndexing: w.allow_indexing,
		hasPassword: w.has_password,
		viewCount: w.view_count,
		monitorIds: (w.monitor_ids ?? []).map(String),
		groups: (w.groups ?? []).map((g) => ({
			id: String(g.id),
			name: g.name,
			sortOrder: g.sort_order
		})),
		monitors: (w.monitors ?? []).map((m) => ({
			monitorId: String(m.monitor_id),
			groupId: m.group_id != null ? String(m.group_id) : undefined,
			groupName: m.group_name,
			sortOrder: m.sort_order,
			showUptime: m.show_uptime
		})),
		createdAt: w.created_at,
		updatedAt: w.updated_at
	};
}

// ---- websocket frames ------------------------------------------------------

/**
 * The hub's frames are the same wire dialect as the REST responses (see
 * internal/ws/hub.go heartbeatView / stateChangeView) and had the same problem:
 * the stores read `monitorId` and `checkedAt` off a payload carrying
 * `monitor_id` and an `at` in unix millis. The mock bus already emits the model
 * shape, so only the real socket needs these — they are exported for
 * $lib/ws/store.svelte.ts to apply as frames arrive.
 */
interface WireWsHeartbeat {
	monitor_id: number;
	status: string;
	at: number;
	latency_ms?: number;
	message?: string;
	important: boolean;
	retries: number;
}

interface WireWsStateChange {
	monitor_id: number;
	monitor_name: string;
	from: string;
	to: string;
	at: number;
	message?: string;
}

export function wsHeartbeatFromWire(w: WireWsHeartbeat): Heartbeat {
	return {
		monitorId: modelId(w.monitor_id),
		status: w.status as MonitorStatus,
		latencyMs: w.latency_ms ?? null,
		message: w.message || undefined,
		checkedAt: new Date(w.at).toISOString()
	};
}

export function wsStateChangeFromWire(w: WireWsStateChange): {
	monitorId: string;
	from: MonitorStatus | null;
	to: MonitorStatus;
	at: string;
} {
	return {
		monitorId: modelId(w.monitor_id),
		// The hub sends "" rather than null for the initial transition.
		from: w.from ? (w.from as MonitorStatus) : null,
		to: w.to as MonitorStatus,
		at: new Date(w.at).toISOString()
	};
}

/** The api contract, derived from the mock so the two never drift. */
export type Api = typeof mockApi;

/**
 * Per-range chart window. `seconds` is the wall-clock span the chart covers and
 * becomes the ?from=/?to= pair (RFC3339, matching parseReportRange on the
 * server); `points` is how many samples we want back. A named range used to map
 * only to a ?limit= of the NEWEST beats, so "24h" on a 60s interval was really
 * the last ~100 minutes. Binding the window by time fixes that, and the server
 * downsamples the window to roughly `points` so a dense monitor does not ship
 * thousands of beats. `points` still grows with the range so a wider window is
 * not starved of detail.
 */
const RANGE_SPEC: Record<'24h' | '7d' | '30d' | '1y', { seconds: number; points: number }> = {
	'24h': { seconds: 24 * 3600, points: 120 },
	'7d': { seconds: 7 * 24 * 3600, points: 150 },
	'30d': { seconds: 30 * 24 * 3600, points: 180 },
	'1y': { seconds: 365 * 24 * 3600, points: 200 }
};

/**
 * Thins an oldest-first series to at most `points` samples by an evenly spaced
 * stride. The server already downsamples across the ?from/&to window; this is a
 * defensive cap so an over-dense response never overloads the chart. Real beats
 * are kept — no synthetic averaging — so the min/max/avg overlay stays truthful,
 * and the newest beat is always retained so the tail reads current.
 */
function downsampleHeartbeats(beats: Heartbeat[], points: number): Heartbeat[] {
	if (beats.length <= points) return beats;
	const stride = Math.ceil(beats.length / points);
	const out: Heartbeat[] = [];
	for (let i = 0; i < beats.length; i += stride) out.push(beats[i]);
	const last = beats[beats.length - 1];
	if (out[out.length - 1] !== last) out.push(last);
	return out;
}

/** Transitions pulled for the event log. */
const EVENT_LIMIT = 50;

/**
 * POST /monitors/{id}/check answers 202 and runs the check asynchronously, so
 * re-reading the monitor straight afterwards returns the PRE-check row every
 * time — not sometimes, always. The button then looked like it did nothing and
 * the result only surfaced whenever something else happened to refresh.
 *
 * These bound how long the client waits for the result to land instead. The
 * ceiling covers the engine's default 30s per-check timeout plus slack for a
 * busy worker pool; on expiry the current row is returned rather than throwing,
 * because the check was accepted and will still complete.
 */
const CHECK_POLL_INTERVAL_MS = 750;
const CHECK_POLL_TIMEOUT_MS = 40_000;

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

/** Triggers a check and resolves once its heartbeat has actually landed. */
async function triggerCheckAndWait(id: string): Promise<Monitor> {
	const before = await http<WireMonitor>('GET', `/monitors/${id}`);
	await http('POST', `/monitors/${id}/check`);
	const deadline = Date.now() + CHECK_POLL_TIMEOUT_MS;
	for (;;) {
		await sleep(CHECK_POLL_INTERVAL_MS);
		const current = await http<WireMonitor>('GET', `/monitors/${id}`);
		// last_checked_at comes from the newest heartbeat, which the engine writes
		// only after the check completes, so a change is proof the check ran.
		if (current.last_checked_at !== before.last_checked_at || Date.now() >= deadline) {
			return monitorFromWire(current);
		}
	}
}

// stepUpToWire maps the re-authentication block. It is a mapper like every
// other camel↔snake bridge in this file rather than an inline object literal,
// because it is sent by four different calls and a field name that drifts in
// one of them fails silently: the server reads an empty password and answers
// 403, which looks exactly like the user mistyping.
function stepUpToWire(c: StepUpConfirmation): { password: string; totp_code: string } {
	return { password: c.password, totp_code: c.totpCode ?? '' };
}

const realApi: Api = {
	setupState: () =>
		http<{ needs_setup: boolean }>('GET', '/setup').then((w) => ({ needsSetup: w.needs_setup })),
	setup: (i) =>
		http<WireUser>('POST', '/setup', {
			email: i.email,
			username: i.username,
			password: i.password,
			// Kept equal to the username so the server's display_name column stays
			// populated; the wizard exposes a single name field.
			display_name: i.username
		}).then(userFromWire),
	// /auth/login verifies TOTP inline: the SAME request carries the optional
	// second factor (totp_code OR backup_code), and the server answers 401 with
	// an "invalid or missing two-factor code" detail when a 2FA-enabled account
	// omits it. The login screen submits credentials first, then resubmits here
	// with a code once that 401 signals one is required. Both keys are known to
	// loginRequest, so sending them empty is fine under DisallowUnknownFields.
	login: (i) =>
		http<{ user: WireUser; csrf_token: string }>('POST', '/auth/login', {
			email: i.username,
			password: i.password,
			totp_code: i.totpCode ?? '',
			backup_code: i.backupCode ?? ''
		}).then((w) => ({ user: userFromWire(w.user) })),
	me: () => http<WireUser>('GET', '/me').then(userFromWire),
	logout: () => http('POST', '/auth/logout'),

	// Unauthenticated. Answers [] when the operator has not configured OIDC, and
	// 404 on a server built before SSO landed — both mean "no SSO", so the login
	// screen treats a failure as an empty list rather than an error.
	oidcProviders: () =>
		http<WireOidcProvider[] | null>('GET', '/auth/oidc/providers').then((w) =>
			(w ?? []).map(oidcProviderFromWire)
		),
	// Public and deliberately uninformative: the server answers 202 with the same
	// body whether or not the address has an account, so there is nothing to
	// return and nothing for a caller to branch on. Resolving is NOT proof that a
	// mail was sent.
	requestPasswordReset: (email) => http('POST', '/auth/password-reset', { email }),
	// 204 on success; one generic 400 for expired, already-used, unknown and
	// malformed tokens alike. The server revokes every session and does NOT issue
	// one here, so the caller must send the user to /login afterwards.
	// `new_password`, NOT `password`: the key must match
	// passwordResetConfirmRequest in internal/api/password_reset.go exactly.
	// decodeJSON uses DisallowUnknownFields, so the old `password` key made every
	// reset attempt fail with an opaque 400 — the flow was completely dead.
	confirmPasswordReset: (i) =>
		http('POST', '/auth/password-reset/confirm', { token: i.token, new_password: i.password }),

	// The two snake_case keys below must match changePasswordRequest and
	// changeEmailRequest in internal/api/auth_handlers.go exactly: decodeJSON
	// uses DisallowUnknownFields, so a camelCase key is an opaque 400, not an
	// ignored one. 204 with no body, so nothing to map back.
	//
	// The server revokes every OTHER session on a successful password change, so
	// the caller has to say so — the current session survives and nothing
	// visible happens otherwise.
	changePassword: (i) =>
		http('PUT', '/me/password', {
			current_password: i.currentPassword,
			new_password: i.newPassword
		}),
	// `current_password` is required and verified before the address moves: the
	// address is the account recovery channel, so the server re-authenticates the
	// change (403 on a wrong password, 409 for an SSO account that has none).
	changeEmail: (i) =>
		http<WireUser>('PUT', '/me/email', {
			current_password: i.currentPassword,
			email: i.email
		}).then(userFromWire),
	// Both avatar calls answer with the whole user, so the mapper is load-bearing
	// the same way it is on the status-page logo upload: without it the caller
	// reads snake_case and avatarUrl is undefined. upload() hardcodes POST and
	// the CSRF header.
	uploadAvatar: (file) => upload<WireUser>('/me/avatar', 'avatar', file).then(userFromWire),
	deleteAvatar: () => http('DELETE', '/me/avatar'),

	// TOTP two-factor. Enrolment is two legs with the authenticator app in
	// between: enroll mints a pending secret (2FA still off), verify proves the
	// code and flips it on, answering the one-time backup codes. Disable takes a
	// password re-check and answers 204; regenerate re-issues the codes.
	//
	// Enrol and regenerate re-authenticate (`stepUp`): both change how the
	// account can be logged into, so a borrowed session must not be enough. The
	// wire names are password/totp_code — see stepUp in internal/api/reauth.go.
	enrollTotp: (confirm) =>
		http<WireTotpEnrollment>('POST', '/me/totp/enroll', stepUpToWire(confirm)).then(
			totpEnrollmentFromWire
		),
	verifyTotp: (code) =>
		http<WireBackupCodes>('POST', '/me/totp/verify', { code }).then((w) => w.backup_codes ?? []),
	// Disabling takes the SAME proof as enrolling. Removing a factor is the more
	// dangerous of the two and used to need only the password.
	disableTotp: (confirm) => http('POST', '/me/totp/disable', stepUpToWire(confirm)),
	regenerateBackupCodes: (confirm) =>
		http<WireBackupCodes>('POST', '/me/totp/backup-codes', stepUpToWire(confirm)).then(
			(w) => w.backup_codes ?? []
		),

	// Linked single sign-on identities. The list and unlink routes are only
	// mounted when the operator has configured OIDC; against an instance without
	// SSO they 404, which the profile screen treats as "no linked accounts".
	// linkOidc is a CSRF POST that answers an authorize URL for the browser to
	// navigate to — a mutation cannot be a top-level GET carrying the CSRF header.
	listIdentities: () =>
		http<WireIdentity[] | null>('GET', '/me/oidc/identities').then((w) =>
			(w ?? []).map(identityFromWire)
		),
	linkOidc: (provider, confirm) =>
		http<{ authorize_url: string }>(
			'POST',
			`/me/oidc/${provider}/link-start`,
			stepUpToWire(confirm)
		).then((w) => ({
			authorizeUrl: w.authorize_url
		})),
	unlinkIdentity: (id) => http('DELETE', `/me/oidc/identities/${id}`),

	// The page cap is asked for EXPLICITLY.
	//
	// Sending no limit meant the server's default of 50, and the mapper discarded
	// `total` — so on any instance with more than fifty monitors the dashboard,
	// the maintenance picker and the status-page picker all showed fifty of N
	// with nothing to say so. The mock returns everything, which is why it never
	// showed up in development. maxListLimit matches the server's own cap
	// (internal/api/respond.go), which is also the bulk endpoint's batch size.
	listMonitors: () =>
		http<WireList<WireMonitor>>('GET', `/monitors?limit=${maxListLimit}`).then((w) =>
			(w.items ?? []).map(monitorFromWire)
		),
	getMonitor: (id) => http<WireMonitor>('GET', `/monitors/${id}`).then(monitorFromWire),
	createMonitor: (i) =>
		http<WireMonitor>('POST', '/monitors', monitorToWire(i)).then(monitorFromWire),
	// PUT, not PATCH: monitorRequest is full-replace and the route is PUT.
	updateMonitor: (id, i) =>
		http<WireMonitor>('PUT', `/monitors/${id}`, monitorToWire(i)).then(monitorFromWire),
	// A soft delete: the monitor moves to the recycle bin, out of every other
	// read, and stays restorable until the grace period expires or someone purges
	// it. Same verb and same path as before, because from the caller's side
	// nothing changed — what changed is that it is now undoable.
	deleteMonitor: (id) => http('DELETE', `/monitors/${id}`),
	listDeletedMonitors: () =>
		http<WireItems<WireMonitor>>('GET', '/monitors/trash').then((w) =>
			(w.items ?? []).map(monitorFromWire)
		),
	restoreMonitor: (id) =>
		http<WireMonitor>('POST', `/monitors/${id}/restore`).then(monitorFromWire),
	// Irreversible, and the only DELETE here that destroys accumulated HISTORY
	// rather than a piece of configuration — a status page or a tag can be
	// recreated in a minute; three years of heartbeats cannot. Admin-only on the
	// server for that reason.
	purgeMonitor: (id) => http('DELETE', `/monitors/${id}/purge`),
	// pause/resume/check answer 204/202 with no body, but the callers replace a
	// monitor in the store with what they get back, so re-read the row. Without
	// the re-read the dashboard replaces the card with undefined.
	pauseMonitor: (id) =>
		http('POST', `/monitors/${id}/pause`).then(() =>
			http<WireMonitor>('GET', `/monitors/${id}`).then(monitorFromWire)
		),
	resumeMonitor: (id) =>
		http('POST', `/monitors/${id}/resume`).then(() =>
			http<WireMonitor>('GET', `/monitors/${id}`).then(monitorFromWire)
		),
	// Unlike pause/resume, this one cannot simply re-read: the check runs
	// asynchronously behind a 202, so an immediate re-read is guaranteed stale.
	// See triggerCheckAndWait.
	checkNow: (id) => triggerCheckAndWait(id),
	// The server answers newest-first; the charts plot oldest-first. from/to bound
	// the window in wall-clock time so "24h" is genuinely the last 24 hours rather
	// than the newest N beats; points caps the server-side downsample.
	heartbeats: (id, range) => {
		const { seconds, points } = RANGE_SPEC[range];
		const to = new Date();
		const from = new Date(to.getTime() - seconds * 1000);
		// `points`, NOT `limit`: in window mode the server discards limit in favour
		// of its own 50,000-beat ceiling (handleMonitorHeartbeats) and downsamples
		// only when given `points`. Sending limit meant a 30d window at a 60s
		// interval shipped ~43,000 heartbeats on every chart load, to be thinned in
		// the browser afterwards.
		const qs = new URLSearchParams({
			from: from.toISOString(),
			to: to.toISOString(),
			points: String(points)
		});
		return http<{ monitor_id: number; heartbeats: WireHeartbeat[] | null }>(
			'GET',
			`/monitors/${id}/heartbeats?${qs}`
		).then((w) =>
			downsampleHeartbeats((w.heartbeats ?? []).map(heartbeatFromWire).reverse(), points)
		);
	},
	events: (id) =>
		http<{ monitor_id: number; heartbeats: WireHeartbeat[] | null }>(
			'GET',
			`/monitors/${id}/heartbeats?important=true&limit=${EVENT_LIMIT}`
		).then((w) => eventsFromWire(w.heartbeats ?? [])),
	monitorEvents: (page) =>
		http<WireList<WireHeartbeat>>(
			'GET',
			`/monitors/events?limit=${page.limit}&offset=${page.offset}`
		).then((w) => ({ items: (w.items ?? []).map(monitorEventFromWire), total: w.total })),

	// Embeddable SVG badge lifecycle. Create is idempotent — a monitor already
	// carrying a badge keeps the same token, so the URLs stay stable — and answers
	// the token plus both public badge URLs. Revoke answers 204 and both URLs stop
	// resolving.
	createMonitorBadge: (id) =>
		http<WireMonitorBadge>('POST', `/monitors/${id}/badge`).then(monitorBadgeFromWire),
	revokeMonitorBadge: (id) => http('DELETE', `/monitors/${id}/badge`),

	// One request for the whole selection, not N. Answers 200 with per-id
	// outcomes even on partial failure, so callers must inspect `failed`.
	bulkMonitors: (i) =>
		http<WireBulkResponse>('POST', '/monitors/bulk', bulkToWire(i)).then(bulkFromWire),

	// Folders are read by anyone and written by writers; the list endpoint
	// answers a bare array (nil marshals as null), like /tags.
	listMonitorGroups: () =>
		http<WireMonitorGroup[] | null>('GET', '/monitor-groups').then((w) =>
			(w ?? []).map(monitorGroupFromWire)
		),
	createMonitorGroup: (i) =>
		http<WireMonitorGroup>('POST', '/monitor-groups', monitorGroupToWire(i)).then(
			monitorGroupFromWire
		),
	updateMonitorGroup: (id, i) =>
		http<WireMonitorGroup>('PUT', `/monitor-groups/${id}`, monitorGroupToWire(i)).then(
			monitorGroupFromWire
		),
	deleteMonitorGroup: (id) => http('DELETE', `/monitor-groups/${id}`),

	listTags: () => http<WireTag[] | null>('GET', '/tags').then((w) => (w ?? []).map(tagFromWire)),
	createTag: (i) => http<WireTag>('POST', '/tags', tagToWire(i)).then(tagFromWire),
	updateTag: (id, i) => http<WireTag>('PUT', `/tags/${id}`, tagToWire(i)).then(tagFromWire),
	deleteTag: (id) => http('DELETE', `/tags/${id}`),

	listNotifications: () =>
		http<WireNotification[] | null>('GET', '/notifications').then((w) =>
			(w ?? []).map(notificationFromWire)
		),
	createNotification: (i) =>
		http<WireNotification>('POST', '/notifications', notificationToWire(i)).then(
			notificationFromWire
		),
	updateNotification: (id, i) =>
		http<WireNotification>('PUT', `/notifications/${id}`, notificationToWire(i)).then(
			notificationFromWire
		),
	deleteNotification: (id) => http('DELETE', `/notifications/${id}`),
	testNotification: (id) =>
		http('POST', `/notifications/${id}/test`).then(() => ({ delivered: true })),

	certificateOverview: () =>
		http<WireItems<WireCertRow>>('GET', '/report/certificates').then((w) =>
			(w.items ?? []).map(certRowFromWire)
		),
	listMaintenance: () =>
		http<WireMaintenance[] | null>('GET', '/maintenance').then((w) =>
			(w ?? []).map(maintenanceFromWire)
		),
	createMaintenance: (i) =>
		http<WireMaintenance>('POST', '/maintenance', maintenanceToWire(i)).then(maintenanceFromWire),
	updateMaintenance: (id, i) =>
		http<WireMaintenance>('PUT', `/maintenance/${id}`, maintenanceToWire(i)).then(
			maintenanceFromWire
		),
	deleteMaintenance: (id) => http('DELETE', `/maintenance/${id}`),

	listUsers: () =>
		http<WireUser[] | null>('GET', '/users').then((w) => (w ?? []).map(userFromWire)),
	createUser: (i) => http<WireUser>('POST', '/users', createUserToWire(i)).then(userFromWire),
	updateUser: (id, i) =>
		http<WireUser>('PUT', `/users/${id}`, updateUserToWire(i)).then(userFromWire),
	deleteUser: (id) => http('DELETE', `/users/${id}`),

	listSessions: () =>
		http<WireSession[] | null>('GET', '/me/sessions').then((w) => (w ?? []).map(sessionFromWire)),
	// Session rows are addressed by the hash of their cookie token, which the
	// list endpoint blanks, so 'others' is the only selector the server accepts.
	revokeSession: (id) => http('DELETE', `/me/sessions/${id}`),
	listApiKeys: () =>
		http<WireApiKey[] | null>('GET', '/me/api-keys').then((w) =>
			(w ?? []).map((k) => apiKeyFromWire(k))
		),
	// An API key outlives every session and no password change revokes it, so
	// minting one re-authenticates.
	createApiKey: (i) =>
		http<{ api_key: WireApiKey; secret: string }>('POST', '/me/api-keys', {
			name: i.name,
			scopes: i.scopes,
			// Omitted rather than sent as null: the server tells "no expiry"
			// (field absent) apart from a bad value, and expires_in_days: 0 is a
			// 400 by design.
			...(i.expiresInDays ? { expires_in_days: i.expiresInDays } : {}),
			...stepUpToWire(i.confirm)
		}).then((w) => apiKeyFromWire(w.api_key, w.secret)),
	revokeApiKey: (id) => http('DELETE', `/me/api-keys/${id}`),

	// Same as listMonitors: without a limit the feed was the newest fifty rows,
	// and the Activity screen searches only what it has — so looking for an actor
	// whose last action was fifty-one entries ago answered "nothing found" on an
	// audit trail, which is the one place a false negative is unacceptable.
	auditLog: () =>
		http<WireList<WireAuditEntry>>('GET', `/audit-log?limit=${maxListLimit}`).then((w) =>
			(w.items ?? []).map(auditEntryFromWire)
		),
	getSettings: () =>
		http<WireSetting[] | null>('GET', '/settings').then((w) => settingsFromWire(w ?? [])),
	version: () =>
		http<{ version: string; commit: string; build_date: string; go_version: string }>(
			'GET',
			'/version'
		).then((w): AppVersion => ({
			version: w.version,
			commit: w.commit,
			buildDate: w.build_date,
			goVersion: w.go_version
		})),
	// The server only writes one key per request (PUT /settings/{key}), so a
	// patch fans out and then re-reads: the response of a single key write says
	// nothing about the other rows the screen renders.
	updateSettings: async (i) => {
		for (const [key, value] of settingsToWire(i)) {
			await http('PUT', `/settings/${key}`, value);
		}
		return http<WireSetting[] | null>('GET', '/settings').then((w) => settingsFromWire(w ?? []));
	},
	// One PUT to the feed's secret row; '' switches signing off. The response is
	// masked by the server, so the settings are re-read for the new flag.
	setWebhookSecret: async (feed, secret) => {
		await http('PUT', `/settings/${SECRET_KEYS[feed]}`, secret);
		return http<WireSetting[] | null>('GET', '/settings').then((w) => settingsFromWire(w ?? []));
	},

	// Configuration import (admin). The bundle is the exact JSON GET /export
	// produced — already snake_case — so it is POSTed VERBATIM, the one request
	// body in this client with no toWire mapper: re-camelising it would break the
	// contract with the server's own export. The Kuma body is a foreign export of
	// unknown shape, likewise passed through untouched. Only the result is mapped.
	importConfig: (bundle) =>
		http<WireImportResult>('POST', '/import', bundle).then(importResultFromWire),
	importUptimeKuma: (data) =>
		raw<WireImportResult>('/import/uptime-kuma', data).then(importResultFromWire),

	listStatusPages: () =>
		http<WireStatusPage[] | null>('GET', '/status-pages').then((w) =>
			(w ?? []).map(statusPageFromWire)
		),
	getStatusPage: (id) =>
		http<WireStatusPage>('GET', `/status-pages/${id}`).then(statusPageFromWire),
	createStatusPage: (i) =>
		http<WireStatusPage>('POST', '/status-pages', statusPageToWire(i)).then(statusPageFromWire),
	updateStatusPage: (id, i) =>
		http<WireStatusPage>('PUT', `/status-pages/${id}`, statusPageToWire(i)).then(
			statusPageFromWire
		),
	deleteStatusPage: (id) => http('DELETE', `/status-pages/${id}`),
	// The mapper on the upload is load-bearing: the handler returns the whole
	// page in wire shape, so without it the editor reads undefined from
	// logoPath and the new logo never previews.
	uploadStatusPageLogo: (id, file) =>
		upload<WireStatusPage>(`/status-pages/${id}/logo`, 'logo', file).then(statusPageFromWire),
	deleteStatusPageLogo: (id) => http('DELETE', `/status-pages/${id}/logo`),
	// Same story as the logo upload: the layout handler answers with the whole
	// page in wire shape. `Api` is derived from the mock, so a missing mapper is
	// invisible here — the return type only pins the method down, not the shape
	// http() hands back — and the editor would re-seed itself from snake_case.
	reorderStatusPageLayout: (id, order) =>
		http<WireStatusPage>('PATCH', `/status-pages/${id}/layout`, {
			ungrouped: order.ungrouped.map(Number),
			groups: order.groups.map((g) => ({
				group_id: Number(g.groupId),
				monitor_ids: g.monitorIds.map(Number)
			}))
		}).then(statusPageFromWire),
	listIncidents: (sid) =>
		http<WireItems<WireIncident>>('GET', `/status-pages/${sid}/incidents`).then((w) =>
			(w.items ?? []).map(incidentFromWire)
		),
	createIncident: (sid, i) =>
		http<WireIncident>('POST', `/status-pages/${sid}/incidents`, incidentToWire(i)).then(
			incidentFromWire
		),
	// Both ids are load-bearing. The server resolves the incident under the status
	// page named in the path and answers 404 when they disagree
	// (incidentForPage), so the literal 0 these two used to send — back when the
	// handlers really did ignore the page id — matched no page and made every
	// timeline post and every delete a 404 from the UI.
	addIncidentUpdate: (sid, iid, i) =>
		http<WireIncident>('POST', `/status-pages/${sid}/incidents/${iid}/updates`, {
			status: i.status,
			body: i.body
		}).then(incidentFromWire),
	deleteIncident: (sid, iid) => http('DELETE', `/status-pages/${sid}/incidents/${iid}`),
	// The retrospective has its own endpoint because updateIncident is
	// full-replace: routing it through there would let a stale editor erase a
	// write-up it never loaded. See incidentToWire.
	setIncidentPostmortem: (sid, iid, i) =>
		http<WireIncident>('PUT', `/status-pages/${sid}/incidents/${iid}/postmortem`, {
			body: i.body,
			published: i.published
		}).then(incidentFromWire),
	// A password-protected page answers 401 until the visitor proves the
	// password. It is sent as the X-Page-Password header (never the query string,
	// which would land in access logs); a correct one makes the server set a
	// short-lived, HttpOnly unlock cookie that later views of the page ride on
	// automatically, so the password is asked for exactly once.
	// `range` selects the uptime window (24h/7d/30d/90d/1y); the server clamps an
	// unknown value to its default, so it is safe to always send. The resolved
	// range and bucket unit come back on the payload for the client to label bars.
	publicStatusPage: (slug, password, range) =>
		http(
			'GET',
			`/public/status/${slug}${range ? `?range=${range}` : ''}`,
			undefined,
			password ? { 'X-Page-Password': password } : undefined
		),
	// The archive rides the same password header as the page. It is a separate
	// request rather than a wider `range` on the page payload because it answers a
	// different question — years at month resolution instead of one window at bar
	// resolution — and nobody opening the status page should pay for it.
	publicStatusHistory: (slug, password, months) =>
		http(
			'GET',
			`/public/status/${slug}/history${months ? `?months=${months}` : ''}`,
			undefined,
			password ? { 'X-Page-Password': password } : undefined
		)
};

/**
 * The api facade. Components only ever import this object; whether calls are
 * served by the mock or the network is an implementation detail toggled by
 * USE_MOCK.
 */
export const api: Api = USE_MOCK ? mockApi : realApi;
