/**
 * In-memory mock backend. Mirrors the REST surface and emits WebSocket-style
 * envelopes on a bus so the whole dashboard is fully interactive without the
 * Go server running (see CLAUDE task brief: "develop against the typed API
 * client with mock data"). Swapping to the real backend is a single flag in
 * ./client.ts — every method here has a real-fetch counterpart there.
 *
 * `export type Api = typeof mockApi` in ./client.ts derives the contract from
 * this file, which pins every METHOD but not one SHAPE: a mock that returns a
 * field the server never sends compiles, ships, and reads undefined in
 * production. So this file is held to the mappers in ./client.ts, not to what
 * is convenient to fake. Where the two genuinely cannot agree the divergence is
 * named in a comment here AND on the type in ./types.ts.
 */
import type {
	AppVersion,
	ApiKey,
	AuditEntry,
	BulkMonitorInput,
	BulkMonitorResponse,
	ConfigBundle,
	Heartbeat,
	ImportResult,
	MaintenanceWindow,
	Monitor,
	MonitorEvent,
	Page,
	MonitorBadge,
	MonitorGroup,
	MonitorStatus,
	Notification,
	OidcIdentity,
	OidcProvider,
	Session,
	Settings,
	WebhookFeed,
	StateEvent,
	Tag,
	StepUpConfirmation,
	TotpEnrollment,
	User,
	StatusPage,
	StatusPageInput,
	StatusPageGroupInput,
	StatusPageLayoutOrder,
	StatusPageMonitorRef,
	Incident,
	IncidentStatus,
	IncidentSeverity,
	PostmortemInput,
	PublicPage,
	PublicMonitorView,
	PublicGroupView,
	PublicIncident,
	PublicHistoryMonitor,
	PublicMaintenance,
	PublicDayBar,
	PublicRange,
	PublicBucket,
	OverallStatus
} from './types';
import type { WsEnvelope } from '$lib/ws/store.svelte';

// ---- seed helpers ---------------------------------------------------------

let seq = 1;
const id = (p: string) => `${p}_${(seq++).toString(36)}${Math.random().toString(36).slice(2, 6)}`;
const iso = (msAgo: number) => new Date(Date.now() - msAgo).toISOString();

function spark(base: number, jitter: number, n = 40): number[] {
	return Array.from({ length: n }, (_, i) =>
		Math.max(1, Math.round(base + Math.sin(i / 4) * jitter + (Math.random() - 0.5) * jitter))
	);
}

/**
 * A plausible per-check history ending in `status`: mostly up, the odd retry,
 * and a run of the current state at the end for anything not up. Paused
 * monitors keep the history they had before they stopped.
 */
function beatsFor(status: MonitorStatus, n = 50): MonitorStatus[] {
	const out: MonitorStatus[] = Array.from({ length: n }, () =>
		Math.random() < 0.03 ? 'pending' : 'up'
	);
	if (status === 'down') out.splice(n - 6, 6, 'pending', 'down', 'down', 'down', 'down', 'down');
	else if (status === 'pending') out[n - 1] = 'pending';
	else if (status === 'maintenance') out.splice(n - 5, 5, ...Array(5).fill('maintenance'));
	return out;
}

// ---- dataset --------------------------------------------------------------

// lastLoginAt is absent from every seed on purpose: userFromWire never sets it
// because the users table has no such column. A mock that filled it in would
// make the users screen look finished and go blank against a real server.
export const adminUser: User = {
	id: 'usr_admin',
	username: 'admin',
	email: 'admin@example.com',
	role: 'admin',
	totpEnabled: false,
	disabled: false,
	createdAt: iso(90 * 864e5)
};

const users: User[] = [
	adminUser,
	{
		id: 'usr_ops',
		username: 'ops',
		email: 'ops@example.com',
		role: 'user',
		totpEnabled: true,
		disabled: false,
		createdAt: iso(40 * 864e5)
	},
	{
		id: 'usr_view',
		username: 'auditor',
		email: 'auditor@example.com',
		role: 'viewer',
		totpEnabled: false,
		disabled: true,
		createdAt: iso(20 * 864e5)
	}
];

// Linked single sign-on identities for adminUser. The real /me/oidc/identities
// route only exists when the operator configured OIDC; the mock always answers
// so the profile screen's SSO section can be developed offline — the same
// DIVERGES-BY-DESIGN choice as oidcProviders(). Empty this array to see the
// "no linked accounts" rendering.
let identities: OidcIdentity[] = [
	{
		id: 'oid_kc',
		provider: 'keycloak',
		issuer: 'https://id.example.com/realms/statwarden',
		subject: 'f47ac10b-58cc-4372-a567-0e02b2c3d479',
		email: 'admin@example.com',
		displayName: 'admin',
		createdAt: iso(20 * 864e5),
		lastLoginAt: iso(3 * 36e5)
	}
];

// Opaque badge tokens per monitor id, standing in for the server's
// EnsureBadgeToken row. A monitor is absent until a badge is created and its
// token is stable across repeat creates, mirroring the idempotent handler.
const badgeTokens = new Map<string, string>();

// One set of single-use backup codes, in the xxxx-xxxx shape the server prints.
function mockBackupCodes(): string[] {
	const chunk = () => Math.random().toString(36).slice(2, 6);
	return Array.from({ length: 10 }, () => `${chunk()}-${chunk()}`);
}

const tags: Tag[] = [
	{ id: 'tag_prod', name: 'Production', color: '#ef4444' },
	{ id: 'tag_home', name: 'Homelab', color: '#0ea5e9' },
	{ id: 'tag_api', name: 'API', color: '#8b5cf6' },
	{ id: 'tag_db', name: 'Database', color: '#10b981' }
];

// Organisational folders. Single assignment per monitor (Monitor.groupId), so
// these are seeded sparsely — the "no folder" case is the common one and the
// dashboard has to render it.
const monitorGroups: MonitorGroup[] = [
	{ id: 'mg_edge', name: 'Edge & web', sortOrder: 0 },
	{ id: 'mg_data', name: 'Data stores', sortOrder: 1 },
	{ id: 'mg_home', name: 'Homelab', sortOrder: 2 }
];

const notifications: Notification[] = [
	{
		id: 'ntf_slack',
		name: 'Ops Slack',
		type: 'slack',
		isDefault: true,
		enabled: true,
		config: { webhook_url: 'https://hooks.slack.com/services/xxx' },
		createdAt: iso(50 * 864e5)
	},
	{
		id: 'ntf_mail',
		name: 'On-call e-mail',
		// Registered as "smtp"; `to` is a []string, not a single address.
		type: 'smtp',
		isDefault: false,
		enabled: true,
		config: {
			host: 'smtp.example.com',
			port: 587,
			from: 'statwarden@example.com',
			to: ['oncall@example.com'],
			encryption: 'starttls'
		},
		createdAt: iso(48 * 864e5)
	},
	{
		id: 'ntf_tg',
		name: 'Telegram alerts',
		type: 'telegram',
		isDefault: false,
		enabled: false,
		config: { bot_token: '123456:abcdef', chat_id: '-100200300' },
		createdAt: iso(10 * 864e5)
	}
];

/**
 * Mirrors monitorTargetURL in internal/api/convert.go: the check target is
 * derived from config, never stored beside it. Keeping the rule in one place on
 * each side is what stops the mock's `url` from drifting into a field the
 * server does not have.
 */
function targetUrl(config: Record<string, unknown>): string | undefined {
	if (typeof config.url === 'string' && config.url) return config.url;
	if (typeof config.host !== 'string' || !config.host) return undefined;
	return typeof config.port === 'number' && config.port > 0
		? `${config.host}:${config.port}`
		: config.host;
}

function mkMonitor(
	name: string,
	type: string,
	status: MonitorStatus,
	base: number,
	tagIds: string[],
	config: Record<string, unknown>
): Monitor {
	const active = status !== 'paused';
	return {
		id: id('mon'),
		name,
		type,
		url: targetUrl(config),
		status,
		active,
		interval: 60,
		timeout: 30,
		maxRetries: 3,
		recoveryChecks: 1,
		retryInterval: 20,
		resendInterval: 0,
		upsideDown: false,
		tagIds,
		notificationIds: ['ntf_slack'],
		config,
		latencyMs: status === 'up' ? base : status === 'pending' ? base * 2 : undefined,
		uptime24h: status === 'up' ? 99.9 : status === 'down' ? 91.2 : 100,
		uptime30d: status === 'down' ? 98.4 : 99.95,
		uptime1y: 99.87,
		sparkline: spark(base, base * 0.35),
		recentBeats: beatsFor(status),
		lastCheckedAt: iso(20000),
		createdAt: iso(60 * 864e5)
	};
}

const monitors: Monitor[] = [
	mkMonitor('Marketing site', 'http', 'up', 120, ['tag_prod'], {
		url: 'https://example.com',
		method: 'GET',
		accepted_status_codes: ['200-299']
	}),
	mkMonitor('API gateway', 'http', 'up', 84, ['tag_prod', 'tag_api'], {
		url: 'https://api.example.com/health',
		method: 'GET',
		keyword: 'healthy'
	}),
	mkMonitor('Primary Postgres', 'postgres', 'up', 12, ['tag_prod', 'tag_db'], {
		host: 'db.internal',
		port: 5432,
		user: 'statwarden',
		database: 'app',
		ssl_mode: 'require',
		query: 'SELECT 1'
	}),
	mkMonitor('Billing worker', 'http', 'down', 0, ['tag_prod', 'tag_api'], {
		url: 'https://billing.example.com/health'
	}),
	mkMonitor('Redis cache', 'redis', 'up', 3, ['tag_prod', 'tag_db'], {
		host: 'cache.internal',
		port: 6379
	}),
	mkMonitor('Homelab NAS', 'ping', 'pending', 40, ['tag_home'], {
		host: '192.168.1.10',
		count: 4
	}),
	mkMonitor('Backup cron', 'push', 'up', 1, ['tag_home'], {
		expected_interval_seconds: 86400,
		grace_period_seconds: 3600
	}),
	mkMonitor('Grafana', 'http', 'maintenance', 210, ['tag_home'], {
		url: 'https://grafana.internal'
	}),
	mkMonitor('Legacy SFTP', 'tcp', 'paused', 0, [], {
		host: 'sftp.example.com',
		port: 22
	})
];

// Folder and dependency seeds, applied after the array because they reference
// minted ids. The two relations are deliberately independent: `groupId` is
// presentation only, while `parentId` is the chain the engine walks to suppress
// a child's alerts while its parent is DOWN.
monitors[0].groupId = 'mg_edge';
monitors[1].groupId = 'mg_edge';
monitors[2].groupId = 'mg_data';
monitors[4].groupId = 'mg_data';
monitors[5].groupId = 'mg_home';
monitors[6].groupId = 'mg_home';
monitors[1].parentId = monitors[2].id;
monitors[1].description =
	'Fronts every public API. Depends on **Primary Postgres** — check that first.\n\n' +
	'- Health endpoint: `GET /health`\n' +
	'- Runbook: https://runbook.example.com/api-gateway';

const maintenance: MaintenanceWindow[] = [
	{
		id: 'mw_1',
		title: 'Grafana upgrade',
		description: 'Rolling upgrade to v11.',
		monitorIds: [monitors[7].id],
		strategy: 'single',
		startsAt: iso(-1 * 36e5),
		endsAt: iso(-3 * 36e5),
		timezone: 'UTC',
		active: true
	},
	{
		id: 'mw_2',
		title: 'DB maintenance',
		monitorIds: [monitors[2].id],
		strategy: 'single',
		startsAt: iso(-3 * 864e5),
		endsAt: iso(-3 * 864e5 - 2 * 36e5),
		timezone: 'UTC',
		active: false
	},
	{
		// A recurring window so the editor's second mode has something to load
		// in mock mode: Mon/Wed/Fri 02:00 Berlin local for two hours, unbounded.
		id: 'mw_3',
		title: 'Nightly index rebuild',
		description: 'Recurring maintenance on the primary database.',
		monitorIds: [monitors[2].id],
		strategy: 'recurring',
		startsAt: '',
		endsAt: '',
		recurrence: { weekdays: [1, 3, 5], start: '02:00', durationMinutes: 120 },
		timezone: 'Europe/Berlin',
		active: true
	},
	{
		// A window that is IN EFFECT right now and covers a monitor on the public
		// 'example' page (Primary Postgres), so the public page's maintenance banner
		// and the per-service "Maintenance" pill are exercisable offline.
		id: 'mw_4',
		title: 'Postgres failover drill',
		description: 'Planned failover exercise on the primary database.',
		monitorIds: [monitors[2].id],
		strategy: 'single',
		startsAt: iso(1 * 36e5), // started 1h ago
		endsAt: iso(-2 * 36e5), // ends in 2h
		timezone: 'UTC',
		active: true
	}
];

const sessions: Session[] = [
	{
		id: 'ses_cur',
		userAgent: 'Chrome on macOS',
		ip: '10.0.0.4',
		current: true,
		createdAt: iso(3 * 36e5),
		lastSeenAt: iso(10000)
	},
	{
		id: 'ses_phone',
		userAgent: 'Safari on iPhone',
		ip: '82.14.2.9',
		current: false,
		createdAt: iso(2 * 864e5),
		lastSeenAt: iso(6 * 36e5)
	}
];

const apiKeys: ApiKey[] = [
	{
		id: 'key_ci',
		name: 'CI pipeline',
		scopes: ['monitors:read', 'monitors:write'],
		prefix: 'upt_ci_9f2',
		lastUsedAt: iso(90 * 60000),
		createdAt: iso(30 * 864e5)
	},
	{
		id: 'key_rotating',
		name: 'Grafana (rotating)',
		scopes: ['monitors:read'],
		prefix: 'upt_gr_41c',
		lastUsedAt: iso(4 * 36e5),
		createdAt: iso(10 * 864e5),
		expiresAt: iso(-80 * 864e5)
	}
];

const audit: AuditEntry[] = [
	{
		id: id('aud'),
		actor: 'admin',
		action: 'monitor.paused',
		target: 'Legacy SFTP',
		targetType: 'monitor',
		at: iso(30 * 60000)
	},
	{
		id: id('aud'),
		actor: 'ops',
		action: 'notification.created',
		target: 'Telegram alerts',
		targetType: 'notification',
		at: iso(10 * 864e5)
	},
	{
		id: id('aud'),
		actor: 'admin',
		action: 'user.disabled',
		target: 'auditor',
		targetType: 'user',
		at: iso(5 * 864e5)
	},
	{
		id: id('aud'),
		actor: 'admin',
		action: 'monitor.created',
		target: 'API gateway',
		targetType: 'monitor',
		at: iso(60 * 864e5)
	}
];

let settings: Settings = {
	instanceName: 'Statwarden · Example',
	baseUrl: 'https://status.example.com',
	retentionDays: 90,
	defaultInterval: 60,
	certExpiryThresholds: [21, 14, 7, 3, 1],
	sessionTtlDays: 30,
	auditWebhookUrl: '',
	outboundWebhookUrl: '',
	auditWebhookSecretSet: false,
	outboundWebhookSecretSet: false
};

// ---- live event bus (WebSocket simulation) -------------------------------

type BusListener = (env: WsEnvelope) => void;
const listeners = new Set<BusListener>();
let ticker: ReturnType<typeof setInterval> | undefined;

export const mockBus = {
	subscribe(fn: BusListener): () => void {
		listeners.add(fn);
		if (!ticker) startTicker();
		return () => {
			listeners.delete(fn);
			if (listeners.size === 0 && ticker) {
				clearInterval(ticker);
				ticker = undefined;
			}
		};
	}
};

function emit(env: WsEnvelope) {
	for (const fn of listeners) fn(env);
}

function pushHeartbeat(m: Monitor) {
	if (!m.active) return;
	const base = m.status === 'down' ? 0 : m.status === 'up' ? (m.latencyMs ?? 100) : 200;
	const latency =
		m.status === 'down' ? null : Math.max(1, Math.round(base + (Math.random() - 0.5) * base * 0.4));
	if (latency != null) {
		m.latencyMs = latency;
		m.sparkline = [...m.sparkline.slice(-39), latency];
	}
	m.recentBeats = [...m.recentBeats.slice(-49), m.status];
	m.lastCheckedAt = new Date().toISOString();
	const hb: Heartbeat = {
		monitorId: m.id,
		status: m.status,
		latencyMs: latency,
		checkedAt: m.lastCheckedAt
	};
	emit({ type: 'heartbeat', topic: `monitor:${m.id}`, data: hb });
	emit({ type: 'heartbeat', topic: 'dashboard:all', data: hb });
}

function startTicker() {
	ticker = setInterval(() => {
		// Heartbeat a couple of random monitors each tick.
		for (const m of monitors) {
			if (m.active && Math.random() < 0.5) pushHeartbeat(m);
		}
		// Occasionally flip one active monitor's state.
		if (Math.random() < 0.25) {
			const active = monitors.filter((m) => m.active && m.status !== 'maintenance');
			const m = active[Math.floor(Math.random() * active.length)];
			if (m) {
				const from = m.status;
				m.status = m.status === 'up' ? (Math.random() < 0.5 ? 'pending' : 'down') : 'up';
				emit({
					type: 'state_change',
					topic: `monitor:${m.id}`,
					data: { monitorId: m.id, from, to: m.status, at: new Date().toISOString() }
				});
				emit({
					type: 'state_change',
					topic: 'dashboard:all',
					data: { monitorId: m.id, from, to: m.status, at: new Date().toISOString() }
				});
			}
		}
	}, 3000);
}

// ---- history generation ---------------------------------------------------

function genHeartbeats(m: Monitor, rangeSec: number, points: number): Heartbeat[] {
	const now = Math.floor(Date.now() / 1000);
	const step = Math.floor(rangeSec / points);
	const base = m.latencyMs ?? 120;
	return Array.from({ length: points }, (_, i) => {
		const t = now - (points - i) * step;
		const down = m.status === 'down' && i > points - 4;
		return {
			monitorId: m.id,
			status: down ? ('down' as MonitorStatus) : ('up' as MonitorStatus),
			latencyMs: down
				? null
				: Math.max(
						1,
						Math.round(base + Math.sin(i / 6) * base * 0.4 + (Math.random() - 0.5) * base * 0.3)
					),
			checkedAt: new Date(t * 1000).toISOString()
		};
	});
}

function genEvents(m: Monitor): StateEvent[] {
	const evs: StateEvent[] = [
		{
			id: id('ev'),
			monitorId: m.id,
			from: null,
			to: 'up',
			at: iso(60 * 864e5),
			durationSec: undefined
		}
	];
	if (m.status === 'down') {
		evs.push({
			id: id('ev'),
			monitorId: m.id,
			from: 'up',
			to: 'pending',
			message: 'connection refused',
			durationSec: 12,
			at: iso(9 * 60000)
		});
		evs.push({
			id: id('ev'),
			monitorId: m.id,
			from: 'pending',
			to: 'down',
			message: 'connection refused',
			durationSec: 40,
			at: iso(8 * 60000)
		});
	} else {
		evs.push({
			id: id('ev'),
			monitorId: m.id,
			from: 'up',
			to: 'down',
			message: 'timeout',
			durationSec: 3600 * 24 * 12,
			at: iso(6 * 864e5)
		});
		evs.push({
			id: id('ev'),
			monitorId: m.id,
			from: 'down',
			to: 'up',
			durationSec: 120,
			at: iso(6 * 864e5 - 12e4)
		});
	}
	return evs.reverse();
}

// ---- mock API implementation ---------------------------------------------

const clone = <T>(v: T): T => structuredClone(v);
const delay = (ms = 120) => new Promise((r) => setTimeout(r, ms));
const find = (mid: string) => {
	const m = monitors.find((x) => x.id === mid);
	if (!m) throw { title: 'Not found', status: 404 };
	return m;
};

/**
 * Mirrors validateMonitorParent in internal/api/monitors.go: a monitor may not
 * be its own parent, and the proposed edge may not close a loop anywhere up the
 * chain. The mock enforces it — with the server's own wording — so the form's
 * error path is reachable without a live backend. `mid` is '' on create, which
 * no stored monitor can equal, so only the "unknown parent" leg can fire there.
 */
function assertNoParentCycle(mid: string, parentId?: string) {
	if (parentId == null) return;
	const bad = (detail: string) => {
		throw { title: 'Bad Request', status: 400, detail };
	};
	if (parentId === mid) bad('parent_id must not reference the monitor itself');
	const seen = new Set<string>();
	let cur: string | undefined = parentId;
	while (cur != null) {
		if (cur === mid || seen.has(cur)) bad('parent_id would create a dependency cycle');
		seen.add(cur);
		const p: Monitor | undefined = monitors.find((x) => x.id === cur);
		if (!p) bad('parent_id references an unknown monitor');
		cur = p?.parentId;
	}
}

let mockNeedsSetup = false;
let mockLoggedIn = true;

// ---- status pages ---------------------------------------------------------

const statusPages: StatusPage[] = [
	{
		id: 'sp_public',
		slug: 'example',
		title: 'Example Status',
		description: 'Live status of Example public services.',
		footerText: 'Questions? support@example.com',
		theme: 'auto',
		locale: 'en',
		accentColor: '#0ea5e9',
		published: true,
		allowIndexing: true,
		hasPassword: false,
		viewCount: 1284,
		monitorIds: monitors.slice(0, 3).map((m) => m.id),
		groups: [{ id: 'spg_public', name: 'Public', sortOrder: 0 }],
		monitors: monitors.slice(0, 3).map((m, i) => ({
			monitorId: m.id,
			groupId: i < 2 ? 'spg_public' : undefined,
			groupName: i < 2 ? 'Public' : undefined,
			showUptime: true,
			sortOrder: i
		})),
		createdAt: iso(60 * 864e5),
		updatedAt: iso(2 * 864e5)
	}
];

/** Soft-deleted monitors. See deleteMonitor for why this is a second array. */
const binnedMonitors: Monitor[] = [];

const incidents: Incident[] = [
	{
		id: 'inc_1',
		statusPageId: 'sp_public',
		title: 'Elevated API latency',
		body: 'We are seeing elevated response times on the API gateway.',
		severity: 'major',
		status: 'monitoring',
		pinned: false,
		startedAt: iso(3 * 36e5),
		monitorIds: monitors.slice(1, 2).map((m) => m.id),
		updates: [
			{
				id: 'iu_1',
				status: 'investigating',
				body: 'Investigating elevated latency.',
				createdAt: iso(3 * 36e5)
			},
			{
				id: 'iu_2',
				status: 'identified',
				body: 'Root cause identified: DB connection pool.',
				createdAt: iso(2 * 36e5)
			},
			{
				id: 'iu_3',
				status: 'monitoring',
				body: 'Fix deployed, monitoring recovery.',
				createdAt: iso(1 * 36e5)
			}
		],
		postmortem: ''
	},
	// A closed incident with a PUBLISHED retrospective, so the mock exercises the
	// public postmortem section as well as the editor. Without one, a mock build
	// renders the status page as if the feature did not exist.
	{
		id: 'inc_2',
		statusPageId: 'sp_public',
		title: 'Checkout unavailable',
		body: 'Checkout returned 503 for a subset of customers.',
		severity: 'critical',
		status: 'resolved',
		pinned: false,
		startedAt: iso(30 * 24 * 36e5),
		resolvedAt: iso(30 * 24 * 36e5 - 2 * 36e5),
		monitorIds: [],
		updates: [
			{
				id: 'iu_4',
				status: 'investigating',
				body: 'Checkout is returning errors; investigating.',
				createdAt: iso(30 * 24 * 36e5)
			},
			{
				id: 'iu_5',
				status: 'resolved',
				body: 'Rolled back the release; checkout is healthy again.',
				createdAt: iso(30 * 24 * 36e5 - 2 * 36e5)
			}
		],
		postmortem: [
			'## Summary',
			'',
			'A release shipped a migration that locked the orders table for two hours.',
			'',
			'## Impact',
			'',
			'Roughly 8% of checkout attempts failed between 09:10 and 11:05 UTC.',
			'',
			'## Root cause',
			'',
			'The migration took an ACCESS EXCLUSIVE lock and was not run concurrently.',
			'',
			'## What we are changing',
			'',
			'- Migrations that touch hot tables now require a review sign-off.',
			'- The deploy pipeline refuses a lock-taking migration during business hours.'
		].join('\n'),
		postmortemPublishedAt: iso(29 * 24 * 36e5)
	}
];

/**
 * Resolves an incident under the status page it is addressed through, throwing
 * the server's 404 when they do not match.
 *
 * The API's incident routes all carry BOTH ids and honour both: asking about
 * page 2 must not act on page 1's incident. Mirroring that here is what keeps a
 * mock build from teaching the UI that the page id is decoration.
 */
function incidentOn(sid: string, iid: string): Incident {
	const inc = incidents.find((x) => x.id === iid);
	if (!inc || inc.statusPageId !== sid) throw { title: 'Not found', status: 404 };
	return inc;
}

/**
 * A stable, unique NUMBER for a mock monitor's string id — its 1-based position
 * in the monitors array. The public wire carries int64 ids, so the mock must
 * hand the page distinct numbers (a naive `Number('mon_x')` is NaN→0 for every
 * monitor, which collapses every id to 0 and makes the keyed lists collide).
 * Maintenance windows reference the same string ids, so they map through here
 * too and the page can resolve monitor_ids back to names.
 */
function numId(strId: string): number {
	const i = monitors.findIndex((m) => m.id === strId);
	return i >= 0 ? i + 1 : 0;
}

/** Bucket unit for a range, matching publicBuckets() on the Go side. */
function bucketUnitFor(range: PublicRange): PublicBucket {
	if (range === '24h' || range === '7d') return 'hour';
	if (range === '1y') return 'month';
	return 'day';
}

/**
 * One uptime bar per bucket of the selected range (oldest first), aligned in UTC
 * — hourly for 24h/7d, daily for 30d/90d, monthly for 1y — mirroring the Go
 * publicBuckets() boundaries. Mostly up, with a couple of blips for a realistic
 * look; `start` is the bucket boundary and `date` its UTC day.
 */
function heatmapFor(status: MonitorStatus, range: PublicRange): PublicDayBar[] {
	const unit = bucketUnitFor(range);
	const count =
		range === '24h' ? 24 : range === '7d' ? 168 : range === '30d' ? 30 : range === '1y' ? 12 : 90;
	const now = new Date();
	const bars: PublicDayBar[] = [];
	for (let i = count - 1; i >= 0; i--) {
		let start: Date;
		if (unit === 'hour') {
			start = new Date(now);
			start.setUTCMinutes(0, 0, 0);
			start.setUTCHours(start.getUTCHours() - i);
		} else if (unit === 'month') {
			start = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth() - i, 1));
		} else {
			start = new Date(now);
			start.setUTCHours(0, 0, 0, 0);
			start.setUTCDate(start.getUTCDate() - i);
		}
		let s: PublicDayBar['status'] = 'up';
		let uptime = 1;
		if (i === 3 && status === 'down') {
			s = 'down';
			uptime = 0.4;
		} else if (i !== 0 && i % 17 === 0) {
			s = 'degraded';
			uptime = 0.985;
		}
		bars.push({
			start: start.toISOString(),
			date: start.toISOString().slice(0, 10),
			status: s,
			uptime
		});
	}
	return bars;
}

/**
 * Mints the ids and positions the layout editor keys off. A reorder payload
 * addresses groups by id and the Go backend assigns one (plus a contiguous
 * sort_order) on every save, so a mock-backed page needs the same or its Layout
 * tab has nothing to drag. Existing values are preserved, which makes this safe
 * to re-apply on update.
 */
function mintLayout(
	groups: StatusPageGroupInput[] = [],
	monitors: StatusPageMonitorRef[] = []
): Pick<StatusPage, 'groups' | 'monitors'> {
	const minted = groups.map((g, i) => ({
		...g,
		id: g.id ?? id('spg'),
		sortOrder: g.sortOrder ?? i
	}));
	return {
		groups: minted,
		monitors: monitors.map((m, i) => ({
			...m,
			// groupId is server-owned too: the editor submits a group NAME and the
			// backend resolves it on read, so resolve it here or a mock-backed
			// page hands the layout editor ids it cannot match.
			groupId: minted.find((g) => g.name === m.groupName)?.id,
			sortOrder: m.sortOrder ?? i
		}))
	};
}

// Plaintext page passwords, keyed by page id. The real server stores an
// argon2id hash and never returns it; the mock keeps the plaintext only so the
// public-page prompt flow is exercisable offline. `unlockedPages` stands in for
// the server's unlock cookie: once a page is unlocked in this session, further
// views of it succeed without re-supplying the password.
const pagePasswords = new Map<string, string>();
const unlockedPages = new Set<string>();

// applyPasswordInput mirrors the server's write-only password controls: a
// non-empty `password` sets protection, `clearPassword` removes it. It returns
// whether the page ends up protected so the caller can set hasPassword.
function applyPasswordInput(pageId: string, input: StatusPageInput): boolean {
	if (input.clearPassword) {
		pagePasswords.delete(pageId);
		unlockedPages.delete(pageId);
		return false;
	}
	if (input.password) {
		pagePasswords.set(pageId, input.password);
		unlockedPages.delete(pageId);
		return true;
	}
	return pagePasswords.has(pageId);
}

/**
 * The in-effect and upcoming maintenance windows touching a page's monitors,
 * mapped to the public wire shape (soonest first). Mirrors pageMaintenance() on
 * the Go side: a single window is "active" while now is inside [start, end] and
 * "upcoming" before it starts; a recurring window is shown as scheduled. Past,
 * disabled or off-page windows are dropped.
 */
function pageMaintenanceViews(page: StatusPage): PublicMaintenance[] {
	const pageMons = new Set(page.monitorIds);
	const now = Date.now();
	const out: PublicMaintenance[] = [];
	for (const w of maintenance) {
		if (!w.active) continue; // enabled flag
		const touches = w.monitorIds.length === 0 || w.monitorIds.some((id) => pageMons.has(id));
		if (!touches) continue;
		let inEffect = false;
		if (w.strategy === 'single') {
			const s = w.startsAt ? Date.parse(w.startsAt) : NaN;
			const e = w.endsAt ? Date.parse(w.endsAt) : NaN;
			if (!Number.isNaN(s) && s <= now && (Number.isNaN(e) || now <= e)) inEffect = true;
			else if (!(!Number.isNaN(s) && s > now)) continue; // neither active nor upcoming → drop
		}
		out.push({
			id: numId(w.id) || Number(w.id.replace(/\D/g, '')) || 0,
			title: w.title,
			body: w.description ?? '',
			starts_at: w.startsAt || undefined,
			ends_at: w.endsAt || undefined,
			active: inEffect,
			monitor_ids: w.monitorIds.map(numId)
		});
	}
	out.sort(
		(a, b) =>
			(a.starts_at ? Date.parse(a.starts_at) : 0) - (b.starts_at ? Date.parse(b.starts_at) : 0)
	);
	return out;
}

function buildPublicPage(page: StatusPage, range: PublicRange = '90d'): PublicPage {
	const mkView = (ref: StatusPageMonitorRef): PublicMonitorView | null => {
		const m = monitors.find((x) => x.id === ref.monitorId);
		if (!m) return null;
		return {
			// Real ids, not 0: the public page keys its lists on these, and a
			// constant would collide exactly the way duplicate names used to.
			id: numId(m.id),
			name: m.name,
			status: m.status,
			show_uptime: ref.showUptime,
			// Mirrors the server: show_uptime=false WITHHOLDS the figures rather
			// than leaving them on the wire for the client to hide. Computing them
			// anyway is why the page's own missing gate went unnoticed — USE_MOCK
			// defaults to true, so the empty-strip case was unreachable in
			// development.
			uptime: ref.showUptime ? (m.status === 'down' ? 0.982 : 0.9995) : -1,
			heatmap: ref.showUptime ? heatmapFor(m.status, range) : []
		};
	};
	// Empty groups are DROPPED, as the server does: a heading with nothing under
	// it says nothing to a visitor and cannot be removed from the editor.
	const groups: PublicGroupView[] = page.groups.map((g, i) => ({
		id: Number(g.id) || i + 1,
		name: g.name,
		monitors: page.monitors
			.filter((r) => r.groupName === g.name)
			.map(mkView)
			.filter((v): v is PublicMonitorView => v !== null)
	}));
	const ungrouped = page.monitors
		.filter((r) => !r.groupName)
		.map(mkView)
		.filter((v): v is PublicMonitorView => v !== null);
	const active = incidents.filter((i) => i.statusPageId === page.id && i.status !== 'resolved');
	const recent = incidents.filter((i) => i.statusPageId === page.id && i.status === 'resolved');
	const maintenanceViews = pageMaintenanceViews(page);
	const maintNow = maintenanceViews.some((w) => w.active);
	const anyDown = [...groups.flatMap((g) => g.monitors), ...ungrouped].some(
		(m) => m.status === 'down'
	);
	const overall: OverallStatus = anyDown
		? 'major_outage'
		: active.length > 0
			? 'degraded'
			: maintNow
				? 'maintenance'
				: 'operational';
	// The DRAFT filter is the load-bearing line here, and it mirrors
	// Incident.publicCopy on the server: an unpublished retrospective is written
	// over days, is wrong in its first pass and names internal systems. A mock
	// that leaked it would let the public page be built against a field that never
	// arrives in production — or, worse, teach nobody that the filter exists.
	const toPublicIncident = (i: Incident): PublicIncident => ({
		id: 0,
		title: i.title,
		body: i.body,
		severity: i.severity,
		status: i.status,
		started_at: i.startedAt,
		resolved_at: i.resolvedAt,
		updates: i.updates.map((u) => ({ status: u.status, body: u.body, created_at: u.createdAt })),
		postmortem: i.postmortemPublishedAt ? i.postmortem : undefined,
		postmortem_published_at: i.postmortemPublishedAt
	});
	return {
		slug: page.slug,
		title: page.title,
		description: page.description,
		footer_text: page.footerText,
		theme: page.theme,
		logo_path: page.logoPath,
		accent_color: page.accentColor,
		locale: page.locale,
		allow_indexing: page.allowIndexing,
		overall_status: overall,
		groups,
		ungrouped,
		active_incidents: active.map(toPublicIncident),
		recent_incidents: recent.map(toPublicIncident),
		maintenance: maintenanceViews,
		range,
		bucket: bucketUnitFor(range),
		generated_at: new Date().toISOString()
	};
}

/**
 * The mock's stand-in for the server's re-authentication (reauth.go). Anything
 * that changes how the account can be logged into — minting an API key,
 * enrolling an authenticator, linking single sign-on, regenerating backup codes
 * — demands the password again, so a borrowed session is not enough. 'admin' is
 * the correct one here, matching changePassword, which keeps the 403 branch on
 * the profile screen reachable offline.
 */
function requireStepUp(confirm: StepUpConfirmation | undefined): void {
	if (confirm?.password !== 'admin') {
		throw { title: 'Forbidden', status: 403, detail: 'password is incorrect' };
	}
}

export const mockApi = {
	// auth
	async setupState() {
		await delay();
		return { needsSetup: mockNeedsSetup };
	},
	async setup(input: { username: string; email: string; password: string }) {
		await delay(200);
		adminUser.username = input.username;
		adminUser.email = input.email;
		mockNeedsSetup = false;
		mockLoggedIn = true;
		return clone(adminUser);
	},
	// Mirrors /auth/login: TOTP is verified inline in the SAME request. A
	// 2FA-enabled account (sign in as `ops`) that submits no valid second factor
	// gets the server's 401 "invalid or missing two-factor code" — the signal the
	// login screen watches for to reveal its code field. A well-formed 6-digit
	// TOTP code or any non-empty backup code clears it, so both legs of the flow
	// are exercisable offline. The password is not checked, matching the mock's
	// single-user model; `me()` always resolves to adminUser.
	async login(input: {
		username: string;
		password: string;
		totpCode?: string;
		backupCode?: string;
	}): Promise<{ user: User }> {
		await delay(200);
		// The identifier is username OR email, matched case-insensitively — the
		// mock mirrors the server's GetCredentialsByLogin so the offline 2FA flow
		// triggers on either form.
		const ident = input.username.trim().toLowerCase();
		const account = users.find(
			(u) => u.username.toLowerCase() === ident || u.email.toLowerCase() === ident
		);
		if (account?.totpEnabled) {
			const totpOk = /^\d{6}$/.test((input.totpCode ?? '').trim());
			const backupOk = (input.backupCode ?? '').trim().length > 0;
			if (!totpOk && !backupOk) {
				throw { title: 'Unauthorized', status: 401, detail: 'invalid or missing two-factor code' };
			}
		}
		mockLoggedIn = true;
		return { user: clone(adminUser) };
	},
	async me() {
		await delay(80);
		if (!mockLoggedIn) throw { title: 'Unauthorized', status: 401 };
		return clone(adminUser);
	},
	async logout() {
		await delay(80);
		mockLoggedIn = false;
	},

	// profile self-service
	/**
	 * The server verifies the current password and answers 403 when it is wrong
	 * (auth_handlers.go handleChangePassword). 'admin' is the mock's stand-in for
	 * the correct one, so the error branch on the profile screen is reachable
	 * without a live backend. Answers no body, like the server's 204.
	 */
	async changePassword(input: { currentPassword: string; newPassword: string }): Promise<void> {
		await delay(200);
		if (input.currentPassword !== 'admin') {
			throw { title: 'Forbidden', status: 403, detail: 'current password is incorrect' };
		}
		if (input.newPassword.length < 8) {
			throw { title: 'Bad Request', status: 400, detail: 'password must be at least 8 characters' };
		}
	},
	/**
	 * The server re-verifies the current password before it will move the address
	 * (auth_handlers.go handleChangeEmail): the address is where password-reset
	 * tokens are mailed, so a hijacked session must not be able to repoint it.
	 * 'admin' is the mock's stand-in for the correct password, exactly as in
	 * changePassword, so the 403 branch on the profile screen is reachable
	 * offline. The password is checked FIRST, like the server does.
	 */
	async changeEmail(input: { email: string; currentPassword: string }) {
		await delay(200);
		if (input.currentPassword !== 'admin') {
			throw { title: 'Forbidden', status: 403, detail: 'current password is incorrect' };
		}
		if (!input.email.includes('@')) {
			throw { title: 'Bad Request', status: 400, detail: 'email is not a valid address' };
		}
		adminUser.email = input.email;
		return clone(adminUser);
	},
	async uploadAvatar(file: File) {
		await delay(300);
		// NOTE: a blob: URL is mock-only. The real server returns a
		// /uploads/avatars/… path; nothing may ever send this value back.
		adminUser.avatarUrl = URL.createObjectURL(file);
		return clone(adminUser);
	},
	async deleteAvatar(): Promise<void> {
		await delay(120);
		adminUser.avatarUrl = undefined;
	},

	// TOTP two-factor. The mock tracks state on adminUser.totpEnabled so the
	// profile screen's enabled/disabled rendering and the enrol → verify → disable
	// flow are all exercisable offline.
	async enrollTotp(confirm: StepUpConfirmation): Promise<TotpEnrollment> {
		await delay(150);
		requireStepUp(confirm);
		if (adminUser.totpEnabled) {
			throw {
				title: 'Conflict',
				status: 409,
				detail: 'two-factor authentication is already enabled'
			};
		}
		// A stable, well-known base32 test secret; the real server mints a random
		// one. otpauth:// is the URI the enrolment QR code encodes.
		const secret = 'JBSWY3DPEHPK3PXP';
		const label = encodeURIComponent(`Statwarden:${adminUser.email}`);
		return {
			secret,
			otpauthUri: `otpauth://totp/${label}?secret=${secret}&issuer=Statwarden`
		};
	},
	async verifyTotp(code: string): Promise<string[]> {
		await delay(200);
		// The server verifies the code against the pending secret; the mock accepts
		// any well-formed 6-digit code and rejects the rest with the server's 400,
		// so both the success and the "code did not verify" paths are reachable.
		if (!/^\d{6}$/.test(code.trim())) {
			throw { title: 'Bad Request', status: 400, detail: 'code did not verify' };
		}
		adminUser.totpEnabled = true;
		return mockBackupCodes();
	},
	async disableTotp(confirm: StepUpConfirmation): Promise<void> {
		await delay(200);
		requireStepUp(confirm);
		adminUser.totpEnabled = false;
	},
	async regenerateBackupCodes(confirm: StepUpConfirmation): Promise<string[]> {
		await delay(200);
		requireStepUp(confirm);
		if (!adminUser.totpEnabled) {
			throw {
				title: 'Bad Request',
				status: 400,
				detail: 'two-factor authentication is not enabled'
			};
		}
		return mockBackupCodes();
	},

	// Linked single sign-on identities. See the `identities` seed for why the mock
	// answers these even though the real routes exist only when OIDC is configured.
	async listIdentities(): Promise<OidcIdentity[]> {
		await delay();
		return clone(identities);
	},
	async linkOidc(provider: string, confirm: StepUpConfirmation): Promise<{ authorizeUrl: string }> {
		await delay(120);
		requireStepUp(confirm);
		// The real call answers an authorize URL the browser navigates to, which
		// only resolves against a configured Go server; in a mock build it 404s.
		return { authorizeUrl: `/api/v1/auth/oidc/${provider}/start` };
	},
	async unlinkIdentity(oid: string): Promise<void> {
		await delay(120);
		identities = identities.filter((x) => x.id !== oid);
	},

	/**
	 * DIVERGES-BY-DESIGN: the real server returns [] unless an operator has
	 * configured OIDC, so a faithful mock would return [] and the SSO section of
	 * the login screen could never be developed or reviewed. This ships one fake
	 * provider instead. Its button performs a real top-level navigation to
	 * /api/v1/auth/oidc/keycloak/start, which only resolves against a configured
	 * Go server — in a mock build it 404s. Empty this array to see the
	 * SSO-disabled rendering.
	 */
	async oidcProviders(): Promise<OidcProvider[]> {
		await delay(60);
		return [{ id: 'keycloak', displayName: 'Keycloak' }];
	},
	/**
	 * Mirrors the server's non-enumeration contract: one exit path, the same
	 * answer for a known address, an unknown address and an address whose
	 * account is disabled. There is deliberately nothing here to branch on.
	 */
	async requestPasswordReset(_email: string): Promise<void> {
		await delay(200);
	},
	/**
	 * The server answers one generic 400 for every rejection — expired, already
	 * used, unknown and malformed are indistinguishable on purpose — so the mock
	 * rejects everything but one magic token with exactly that shape. Visit
	 * /reset-password?token=mock-valid to exercise the success path.
	 */
	async confirmPasswordReset(input: { token: string; password: string }): Promise<void> {
		await delay(200);
		if (input.token !== 'mock-valid') {
			throw {
				title: 'Bad Request',
				status: 400,
				detail: 'invalid or expired reset token'
			};
		}
	},

	// monitors
	async listMonitors() {
		await delay();
		return clone(monitors);
	},
	async getMonitor(mid: string) {
		await delay();
		return clone(find(mid));
	},
	async createMonitor(input: Partial<Monitor>) {
		await delay(200);
		assertNoParentCycle('', input.parentId);
		const config = input.config ?? {};
		const m = mkMonitor(
			input.name ?? 'New monitor',
			input.type ?? 'http',
			'pending',
			120,
			input.tagIds ?? [],
			config
		);
		// `url` is server-derived (monitorToWire never sends it), so it is
		// re-resolved from config rather than taken from the input.
		Object.assign(m, input, {
			id: m.id,
			status: 'pending' as MonitorStatus,
			url: targetUrl(config)
		});
		monitors.unshift(m);
		return clone(m);
	},
	async updateMonitor(mid: string, input: Partial<Monitor>) {
		await delay(180);
		const m = find(mid);
		assertNoParentCycle(mid, input.parentId);
		Object.assign(m, input, { id: m.id });
		m.url = targetUrl(m.config);
		return clone(m);
	},
	/**
	 * Deleting is a SOFT delete, mirroring the server (migration 0018): the
	 * monitor moves to the bin, out of every other read, and can be brought back.
	 *
	 * Modelled as a second array rather than a flag on the monitor, for the same
	 * reason MemStore uses a second map: every existing lookup walks `monitors`,
	 * so a binned monitor vanishes from all of them without one of those lookups
	 * gaining a filter it could later forget.
	 */
	async deleteMonitor(mid: string) {
		await delay(150);
		const i = monitors.findIndex((x) => x.id === mid);
		if (i < 0) throw { title: 'Not found', status: 404 };
		const [m] = monitors.splice(i, 1);
		m.active = false;
		m.status = 'paused';
		binnedMonitors.unshift(m);
	},
	async listDeletedMonitors() {
		await delay();
		return clone(binnedMonitors);
	},
	async restoreMonitor(mid: string) {
		await delay(150);
		const i = binnedMonitors.findIndex((x) => x.id === mid);
		if (i < 0) throw { title: 'Not found', status: 404 };
		const [m] = binnedMonitors.splice(i, 1);
		// Paused, always — the server cannot know what `active` was before, and
		// a mock that guessed differently would teach the UI a contract that
		// does not exist.
		m.active = false;
		m.status = 'paused';
		monitors.push(m);
		return clone(m);
	},
	async purgeMonitor(mid: string) {
		await delay(150);
		const i = binnedMonitors.findIndex((x) => x.id === mid);
		// 404 for a LIVE monitor too: the only route to an irreversible delete
		// runs through the bin.
		if (i < 0) throw { title: 'Not found', status: 404 };
		binnedMonitors.splice(i, 1);
	},
	async pauseMonitor(mid: string) {
		await delay(120);
		const m = find(mid);
		m.active = false;
		m.status = 'paused';
		return clone(m);
	},
	async resumeMonitor(mid: string) {
		await delay(120);
		const m = find(mid);
		m.active = true;
		m.status = 'pending';
		return clone(m);
	},
	async checkNow(mid: string) {
		await delay(400);
		const m = find(mid);
		if (m.active) {
			m.status = Math.random() < 0.85 ? 'up' : 'down';
			pushHeartbeat(m);
		}
		return clone(m);
	},
	async heartbeats(mid: string, range: '24h' | '7d' | '30d' | '1y') {
		await delay();
		const m = find(mid);
		const spec = {
			'24h': [864e2, 96],
			'7d': [7 * 864e2, 120],
			'30d': [30 * 864e2, 120],
			'1y': [365 * 864e2, 120]
		} as const;
		const [rangeSec, points] = spec[range];
		return genHeartbeats(m, rangeSec, points);
	},
	async events(mid: string) {
		await delay();
		return genEvents(find(mid));
	},
	async monitorEvents(page: { limit: number; offset: number }): Promise<Page<MonitorEvent>> {
		await delay();
		const all: MonitorEvent[] = monitors
			.flatMap((m) => genEvents(m))
			.map((e) => ({
				id: e.id,
				monitorId: e.monitorId,
				status: e.to,
				message: e.message,
				at: e.at
			}))
			.sort((a, b) => Date.parse(b.at) - Date.parse(a.at));
		return { items: all.slice(page.offset, page.offset + page.limit), total: all.length };
	},

	// Embeddable SVG badge lifecycle. Idempotent create, like EnsureBadgeToken:
	// re-creating returns the same token so the URLs stay stable. The base is the
	// settings row's base URL, standing in for the server's PUBLIC_BASE_URL.
	async createMonitorBadge(mid: string): Promise<MonitorBadge> {
		await delay(150);
		find(mid);
		let token = badgeTokens.get(mid);
		if (!token) {
			token = `bdg_${Math.random().toString(36).slice(2, 10)}${Math.random().toString(36).slice(2, 10)}`;
			badgeTokens.set(mid, token);
		}
		const base = settings.baseUrl ? settings.baseUrl.replace(/\/+$/, '') : '';
		return {
			token,
			statusUrl: `${base}/api/v1/badge/${token}/status.svg`,
			uptimeUrl: `${base}/api/v1/badge/${token}/uptime.svg`
		};
	},
	async revokeMonitorBadge(mid: string): Promise<void> {
		await delay(120);
		find(mid);
		badgeTokens.delete(mid);
	},

	/**
	 * Mirrors handleBulkMonitors: one action over an explicit id list, whole-batch
	 * validation of the action's arguments up front, then per-id outcomes. It
	 * resolves with `failed > 0` rather than rejecting on a bad id — the UI's
	 * partial-failure path is only reachable if the mock can produce one.
	 */
	async bulkMonitors(input: BulkMonitorInput): Promise<BulkMonitorResponse> {
		await delay(220);
		const bad = (detail: string) => {
			throw { title: 'Bad Request', status: 400, detail };
		};
		if (input.monitorIds.length === 0) bad('monitor_ids must not be empty');
		if (input.monitorIds.length > 200) bad('monitor_ids exceeds the maximum batch size');
		if (input.action === 'tag') {
			if (!input.tagIds?.length) bad('tag_ids is required for the tag action');
			for (const tid of input.tagIds ?? [])
				if (!tags.some((x) => x.id === tid)) bad('tag_ids references an unknown tag');
		}
		if (input.action === 'group' && input.groupId != null) {
			if (!monitorGroups.some((g) => g.id === input.groupId))
				bad('group_id references an unknown monitor group');
		}
		if (input.action === 'notify') {
			if (!input.notificationIds?.length) bad('notification_ids is required for the notify action');
			for (const nid of input.notificationIds ?? [])
				if (!notifications.some((x) => x.id === nid))
					bad('notification_ids references an unknown notification');
		}
		if (input.action === 'schedule') {
			// Mirrors validateBulkArgs' schedule bounds, so the toolbar's error path
			// is reachable offline exactly as it is against the real backend.
			const sc = input.schedule;
			if (!sc || sc.interval < 1) bad('interval_seconds must be >= 1');
			if (sc && sc.timeout != null && sc.timeout < 1) bad('timeout_seconds must be >= 1');
			if (sc && sc.maxRetries != null && sc.maxRetries < 0) bad('max_retries must be >= 0');
			if (sc && sc.retryInterval != null && sc.retryInterval < 1)
				bad('retry_interval_seconds must be >= 1');
		}

		// Deduplicated, caller order preserved — same reason as normaliseBulkIDs:
		// a repeated id would otherwise be counted twice and report a spurious
		// "not found" on its second pass through delete.
		const ids = [...new Set(input.monitorIds)];
		const results: BulkMonitorResponse['results'] = [];
		let succeeded = 0;
		let failed = 0;
		for (const mid of ids) {
			const i = monitors.findIndex((x) => x.id === mid);
			if (i < 0) {
				failed++;
				results.push({ monitorId: mid, ok: false, error: 'not found' });
				continue;
			}
			const m = monitors[i];
			switch (input.action) {
				case 'pause':
					m.active = false;
					m.status = 'paused';
					break;
				case 'resume':
					m.active = true;
					m.status = 'up';
					break;
				case 'delete':
					// SOFT, exactly like deleteMonitor above. The bulk bar and the
					// single delete open the same dialog, and that dialog promises
					// the recycle bin; a mock that quietly destroyed the monitor
					// instead taught the UI a contract the server does not have —
					// and USE_MOCK defaults to true, so it is what most of this
					// screen was ever developed against.
					monitors.splice(i, 1);
					m.active = false;
					m.status = 'paused';
					binnedMonitors.unshift(m);
					break;
				case 'tag':
					// ADDS, never replaces — AddMonitorTags on the server.
					m.tagIds = [...new Set([...m.tagIds, ...(input.tagIds ?? [])])];
					break;
				case 'group':
					m.groupId = input.groupId ?? undefined;
					break;
				case 'notify':
					// ADDS, never replaces — AddMonitorNotifications on the server.
					m.notificationIds = [
						...new Set([...m.notificationIds, ...(input.notificationIds ?? [])])
					];
					break;
				case 'schedule':
					// Interval is always written; each override is applied only when
					// present, mirroring SetMonitorSchedule's COALESCE semantics.
					if (input.schedule) {
						m.interval = input.schedule.interval;
						if (input.schedule.timeout != null) m.timeout = input.schedule.timeout;
						if (input.schedule.maxRetries != null) m.maxRetries = input.schedule.maxRetries;
						if (input.schedule.retryInterval != null)
							m.retryInterval = input.schedule.retryInterval;
					}
					break;
			}
			succeeded++;
			results.push({ monitorId: mid, ok: true });
		}
		return { action: input.action, succeeded, failed, results };
	},

	// monitor groups (organisational folders)
	async listMonitorGroups() {
		await delay();
		// Sorted the way both Go stores sort: sort_order, then id.
		return clone([...monitorGroups].sort((a, b) => a.sortOrder - b.sortOrder));
	},
	async createMonitorGroup(input: Partial<MonitorGroup>) {
		await delay(120);
		const g: MonitorGroup = {
			id: id('mg'),
			name: input.name ?? 'New group',
			sortOrder: input.sortOrder ?? monitorGroups.length
		};
		monitorGroups.push(g);
		return clone(g);
	},
	async updateMonitorGroup(gid: string, input: Partial<MonitorGroup>) {
		await delay(120);
		const g = monitorGroups.find((x) => x.id === gid);
		if (!g) throw { title: 'Not found', status: 404 };
		Object.assign(g, input, { id: g.id });
		return clone(g);
	},
	async deleteMonitorGroup(gid: string) {
		await delay(120);
		const i = monitorGroups.findIndex((x) => x.id === gid);
		if (i >= 0) monitorGroups.splice(i, 1);
		// ON DELETE SET NULL in the schema: the monitors filed here survive with
		// their folder cleared. Deleting a folder never loses monitors.
		for (const m of monitors) if (m.groupId === gid) m.groupId = undefined;
	},

	// tags
	async listTags() {
		await delay();
		return clone(tags);
	},
	async createTag(input: Omit<Tag, 'id'>) {
		await delay(120);
		const t = { ...input, id: id('tag') };
		tags.push(t);
		return clone(t);
	},
	async updateTag(tid: string, input: Partial<Tag>) {
		await delay(120);
		const t = tags.find((x) => x.id === tid);
		if (!t) throw { title: 'Not found', status: 404 };
		Object.assign(t, input, { id: t.id });
		return clone(t);
	},
	async deleteTag(tid: string) {
		await delay(120);
		const i = tags.findIndex((x) => x.id === tid);
		if (i >= 0) tags.splice(i, 1);
	},

	// notifications
	async listNotifications() {
		await delay();
		return clone(notifications);
	},
	async createNotification(input: Partial<Notification>) {
		await delay(150);
		const n: Notification = {
			id: id('ntf'),
			name: input.name ?? 'New channel',
			type: input.type ?? 'slack',
			isDefault: input.isDefault ?? false,
			enabled: input.enabled ?? true,
			config: input.config ?? {},
			createdAt: new Date().toISOString()
		};
		notifications.push(n);
		return clone(n);
	},
	async updateNotification(nid: string, input: Partial<Notification>) {
		await delay(150);
		const n = notifications.find((x) => x.id === nid);
		if (!n) throw { title: 'Not found', status: 404 };
		Object.assign(n, input, { id: n.id });
		return clone(n);
	},
	async deleteNotification(nid: string) {
		await delay(120);
		const i = notifications.findIndex((x) => x.id === nid);
		if (i >= 0) notifications.splice(i, 1);
	},
	async testNotification(nid: string) {
		await delay(600);
		const n = notifications.find((x) => x.id === nid);
		if (!n) throw { title: 'Not found', status: 404 };
		if (!n.enabled)
			throw {
				title: 'Channel disabled',
				status: 400,
				detail: 'Enable the channel before testing.'
			};
		return { delivered: true };
	},

	/**
	 * TLS certificates, derived from the mock monitors rather than a fixed list,
	 * so the widget shows names that exist elsewhere in the mock UI.
	 *
	 * The spread deliberately includes one already-expired certificate: the
	 * expired branch is the one a developer never sees by accident, and it is the
	 * branch that has to be unmistakable.
	 */
	async certificateOverview() {
		await delay();
		const tls = monitors.filter((m) => m.type === 'http' || m.type === 'cert').slice(0, 4);
		const spread = [-2, 5, 27, 210];
		return tls.map((m, i) => {
			const days = spread[i % spread.length];
			return {
				monitorId: m.id,
				monitorName: m.name,
				subject: (m.url ?? m.name).replace(/^https?:\/\//, '').split('/')[0],
				issuer: "Let's Encrypt R3",
				validTo: iso(-days * 24 * 36e5),
				daysRemaining: days,
				expired: days < 0
			};
		});
	},

	// maintenance
	async listMaintenance() {
		await delay();
		return clone(maintenance);
	},
	async createMaintenance(input: Partial<MaintenanceWindow>) {
		await delay(150);
		const w: MaintenanceWindow = {
			id: id('mw'),
			title: input.title ?? 'Maintenance',
			description: input.description,
			monitorIds: input.monitorIds ?? [],
			strategy: input.strategy ?? 'single',
			startsAt: input.startsAt ?? new Date().toISOString(),
			endsAt: input.endsAt ?? new Date(Date.now() + 36e5).toISOString(),
			recurrence: input.recurrence,
			timezone: input.timezone ?? 'UTC',
			active: false
		};
		maintenance.push(w);
		return clone(w);
	},
	async updateMaintenance(wid: string, input: Partial<MaintenanceWindow>) {
		await delay(150);
		const w = maintenance.find((x) => x.id === wid);
		if (!w) throw { title: 'Not found', status: 404 };
		Object.assign(w, input, { id: w.id });
		return clone(w);
	},
	async deleteMaintenance(wid: string) {
		await delay(120);
		const i = maintenance.findIndex((x) => x.id === wid);
		if (i >= 0) maintenance.splice(i, 1);
	},

	// users
	async listUsers() {
		await delay();
		return clone(users);
	},
	// POST /users requires an initial password, which the server hashes and whose
	// length it validates (validateCredentials -> min 8). Mirror both here so the
	// create form's error path is reachable offline and a passwordless create
	// fails in mock mode exactly as it does against the real backend.
	async createUser(input: Partial<User> & { password?: string }) {
		await delay(150);
		if (!input.email || !input.email.includes('@')) {
			throw { title: 'Bad Request', status: 400, detail: 'email is not a valid address' };
		}
		if (!input.password || input.password.length < 8) {
			throw { title: 'Bad Request', status: 400, detail: 'password must be at least 8 characters' };
		}
		const u: User = {
			id: id('usr'),
			username: input.username ?? 'user',
			email: input.email ?? '',
			role: input.role ?? 'viewer',
			totpEnabled: false,
			disabled: false,
			createdAt: new Date().toISOString()
		};
		users.push(u);
		return clone(u);
	},
	async updateUser(uid: string, input: Partial<User>) {
		await delay(150);
		const u = users.find((x) => x.id === uid);
		if (!u) throw { title: 'Not found', status: 404 };
		Object.assign(u, input, { id: u.id });
		return clone(u);
	},
	async deleteUser(uid: string) {
		await delay(120);
		const i = users.findIndex((x) => x.id === uid);
		if (i >= 0) users.splice(i, 1);
	},

	// sessions & api keys
	async listSessions() {
		await delay();
		return clone(sessions);
	},
	async revokeSession(sid: string) {
		await delay(120);
		const i = sessions.findIndex((x) => x.id === sid);
		if (i >= 0 && !sessions[i].current) sessions.splice(i, 1);
	},
	async listApiKeys() {
		await delay();
		return clone(apiKeys);
	},
	async createApiKey(input: {
		name: string;
		scopes: string[];
		/** Whole days from now. Omitted means the key never expires. */
		expiresInDays?: number;
		confirm: StepUpConfirmation;
	}) {
		await delay(150);
		requireStepUp(input.confirm);
		const k: ApiKey = {
			id: id('key'),
			name: input.name,
			scopes: input.scopes,
			prefix: `upt_${Math.random().toString(36).slice(2, 5)}`,
			token: `upt_${Math.random().toString(36).slice(2)}${Math.random().toString(36).slice(2)}`,
			createdAt: new Date().toISOString(),
			expiresAt: input.expiresInDays
				? new Date(Date.now() + input.expiresInDays * 864e5).toISOString()
				: undefined
		};
		apiKeys.push({ ...k, token: undefined });
		return clone(k);
	},
	async revokeApiKey(kid: string) {
		await delay(120);
		const i = apiKeys.findIndex((x) => x.id === kid);
		if (i >= 0) apiKeys.splice(i, 1);
	},

	// audit & settings
	async auditLog() {
		await delay();
		return clone(audit);
	},
	async getSettings() {
		await delay();
		return clone(settings);
	},
	async version(): Promise<AppVersion> {
		await delay();
		return { version: 'dev', commit: 'mock', buildDate: '', goVersion: 'go1.26' };
	},
	async updateSettings(input: Partial<Settings>) {
		await delay(150);
		settings = { ...settings, ...input };
		return clone(settings);
	},
	/**
	 * Mirrors the server: the secret itself is not kept anywhere readable, only
	 * whether one is set; '' clears it. Same 16..256 rule as CheckWebhookSecret.
	 */
	async setWebhookSecret(feed: WebhookFeed, secret: string) {
		await delay(150);
		if (
			secret !== '' &&
			(secret.length < 16 || secret.length > 256 || /[\s\x00-\x1f\x7f]/.test(secret))
		) {
			throw {
				title: 'Bad Request',
				status: 400,
				detail: 'must be between 16 and 256 characters, without spaces'
			};
		}
		if (feed === 'audit') settings.auditWebhookSecretSet = secret !== '';
		else settings.outboundWebhookSecretSet = secret !== '';
		return clone(settings);
	},

	/**
	 * Configuration import. DIVERGES-BY-DESIGN: the real server persists the
	 * bundle and the imported monitors appear on the dashboard; the mock does NOT
	 * persist (the bundle's snake_case specs are not the camelCase models the rest
	 * of the store holds), so it only summarises what a real import WOULD create —
	 * counts derived straight from the supplied bundle, no fabrication. Enough to
	 * exercise the result screen; connect a backend to see the entities land.
	 */
	async importConfig(bundle: ConfigBundle): Promise<ImportResult> {
		await delay(300);
		return {
			monitors: bundle?.monitors?.length ?? 0,
			notifications: bundle?.notifications?.length ?? 0,
			tags: bundle?.tags?.length ?? 0,
			statusPages: bundle?.status_pages?.length ?? 0,
			warnings: []
		};
	},
	/**
	 * Uptime-Kuma import. The mapping runs entirely server-side, so the mock has
	 * nothing to map a foreign export against and returns an empty result with a
	 * warning making the offline limitation explicit rather than faking counts.
	 */
	async importUptimeKuma(_data: ArrayBuffer): Promise<ImportResult> {
		await delay(300);
		return {
			monitors: 0,
			notifications: 0,
			tags: 0,
			statusPages: 0,
			warnings: ['Uptime Kuma import runs on the server; connect a backend to import for real.']
		};
	},

	// status pages
	async listStatusPages() {
		await delay();
		return clone(statusPages);
	},
	async getStatusPage(sid: string) {
		await delay();
		const p = statusPages.find((x) => x.id === sid);
		if (!p) throw { title: 'Not found', status: 404 };
		return clone(p);
	},
	async createStatusPage(input: StatusPageInput) {
		await delay(150);
		const now = new Date().toISOString();
		const newId = id('sp');
		const p: StatusPage = {
			id: newId,
			slug: input.slug ?? 'page',
			title: input.title ?? 'New status page',
			description: input.description ?? '',
			footerText: input.footerText ?? '',
			theme: input.theme ?? 'auto',
			locale: input.locale,
			accentColor: input.accentColor,
			// logoPath is deliberately not taken from input: it is server-owned and
			// written only by uploadStatusPageLogo/deleteStatusPageLogo.
			customDomain: input.customDomain,
			published: input.published ?? true,
			allowIndexing: input.allowIndexing ?? true,
			// hasPassword is derived from the write-only password controls, never
			// echoed back into the model any other way — matching the server.
			hasPassword: applyPasswordInput(newId, input),
			viewCount: 0,
			monitorIds: input.monitorIds ?? [],
			...mintLayout(input.groups, input.monitors),
			createdAt: now,
			updatedAt: now
		};
		statusPages.push(p);
		return clone(p);
	},
	async updateStatusPage(sid: string, input: StatusPageInput) {
		await delay(150);
		const p = statusPages.find((x) => x.id === sid);
		if (!p) throw { title: 'Not found', status: 404 };
		const hasPassword = applyPasswordInput(p.id, input);
		// The write-only password controls are consumed above; they must not be
		// copied onto the stored model (the server never echoes them back).
		const pageFields: StatusPageInput = { ...input };
		delete pageFields.password;
		delete pageFields.clearPassword;
		Object.assign(p, pageFields, {
			id: p.id,
			// logoPath is server-owned, mirroring MemStore.UpdateStatusPage: the
			// stored value survives a full-replace update regardless of the input.
			logoPath: p.logoPath,
			hasPassword,
			...mintLayout(input.groups ?? p.groups, input.monitors ?? p.monitors),
			updatedAt: new Date().toISOString()
		});
		return clone(p);
	},
	async deleteStatusPage(sid: string) {
		await delay(120);
		const i = statusPages.findIndex((x) => x.id === sid);
		if (i >= 0) statusPages.splice(i, 1);
	},
	async uploadStatusPageLogo(sid: string, file: File) {
		await delay(300);
		const p = statusPages.find((x) => x.id === sid);
		if (!p) throw { title: 'Not found', status: 404 };
		// NOTE: a blob: URL is mock-only. The real server returns a
		// /uploads/logos/… path; nothing may ever send this value back.
		p.logoPath = URL.createObjectURL(file);
		p.updatedAt = new Date().toISOString();
		return clone(p);
	},
	async deleteStatusPageLogo(sid: string) {
		await delay(120);
		const p = statusPages.find((x) => x.id === sid);
		if (p) p.logoPath = undefined;
	},
	/**
	 * Rewrites group and monitor positions from a COMPLETE ordering. The size
	 * checks against the raw payload length are the mock's stand-in for the
	 * distinctness sets both Go stores keep inside their transaction: counting
	 * alone lets a payload naming the same id twice through, which would leave
	 * another item with a stale position.
	 */
	async reorderStatusPageLayout(sid: string, order: StatusPageLayoutOrder) {
		await delay(120);
		const p = statusPages.find((x) => x.id === sid);
		if (!p) throw { title: 'Not found', status: 404 };
		const stale = { title: 'Conflict', status: 409, detail: 'layout is out of date' };
		const wantGroups = new Set(p.groups.map((g) => g.id));
		const gotGroups = new Set(order.groups.map((g) => g.groupId));
		if (
			gotGroups.size !== order.groups.length ||
			gotGroups.size !== wantGroups.size ||
			[...gotGroups].some((g) => !wantGroups.has(g))
		)
			throw stale;
		const flat = [
			...order.ungrouped.map((m) => ({ monitorId: m, groupId: undefined as string | undefined })),
			...order.groups.flatMap((g) =>
				g.monitorIds.map((m) => ({ monitorId: m, groupId: g.groupId }))
			)
		];
		const wantMonitors = new Set(p.monitors.map((m) => m.monitorId));
		const gotMonitors = new Set(flat.map((f) => f.monitorId));
		if (
			gotMonitors.size !== flat.length ||
			gotMonitors.size !== wantMonitors.size ||
			[...gotMonitors].some((m) => !wantMonitors.has(m))
		)
			throw stale;
		const nameOf = (gid?: string) => p.groups.find((g) => g.id === gid)?.name;
		p.groups = order.groups.map((g, i) => ({
			...p.groups.find((x) => x.id === g.groupId)!,
			sortOrder: i
		}));
		p.monitors = flat.map((f, i) => ({
			...p.monitors.find((m) => m.monitorId === f.monitorId)!,
			groupId: f.groupId,
			groupName: nameOf(f.groupId),
			sortOrder: i
		}));
		p.monitorIds = p.monitors.map((m) => m.monitorId);
		p.updatedAt = new Date().toISOString();
		return clone(p);
	},

	// incidents
	async listIncidents(sid: string) {
		await delay();
		return clone(incidents.filter((i) => i.statusPageId === sid));
	},
	async createIncident(sid: string, input: Partial<Incident>) {
		await delay(150);
		const now = new Date().toISOString();
		const status = (input.status ?? 'investigating') as IncidentStatus;
		const inc: Incident = {
			id: id('inc'),
			statusPageId: sid,
			title: input.title ?? 'Incident',
			body: input.body ?? '',
			severity: (input.severity ?? 'minor') as IncidentSeverity,
			status,
			pinned: !!input.pinned,
			startedAt: now,
			monitorIds: input.monitorIds ?? [],
			updates: [{ id: id('iu'), status, body: input.body ?? '', createdAt: now }],
			postmortem: ''
		};
		incidents.unshift(inc);
		return clone(inc);
	},
	// sid is not decoration: the server resolves the incident under the status
	// page named in the path and answers 404 when they disagree
	// (incidentForPage). The mock enforces the same pairing so a caller that
	// passes the wrong page fails here too, rather than only in production.
	async addIncidentUpdate(
		sid: string,
		iid: string,
		input: { status: IncidentStatus; body: string }
	) {
		await delay(150);
		const inc = incidentOn(sid, iid);
		const now = new Date().toISOString();
		inc.updates.push({ id: id('iu'), status: input.status, body: input.body, createdAt: now });
		inc.status = input.status;
		if (input.status === 'resolved') inc.resolvedAt = now;
		return clone(inc);
	},
	async deleteIncident(sid: string, iid: string) {
		await delay(120);
		incidentOn(sid, iid);
		const i = incidents.findIndex((x) => x.id === iid);
		if (i >= 0) incidents.splice(i, 1);
	},
	/**
	 * Writes (and optionally publishes) a retrospective. The rules mirror the
	 * server exactly — publishing needs a non-empty body and a resolved incident,
	 * and re-publishing keeps the original timestamp — because a mock that is
	 * more permissive than the API teaches the UI a contract that does not exist.
	 */
	async setIncidentPostmortem(sid: string, iid: string, input: PostmortemInput) {
		await delay(150);
		const inc = incidentOn(sid, iid);
		const body = input.body.trim();
		if (input.published) {
			if (!body)
				throw { title: 'Bad Request', status: 400, detail: 'cannot publish an empty postmortem' };
			if (inc.status !== 'resolved') {
				throw {
					title: 'Conflict',
					status: 409,
					detail: 'the incident must be resolved before its postmortem can be published'
				};
			}
			inc.postmortemPublishedAt = inc.postmortemPublishedAt ?? new Date().toISOString();
		} else {
			inc.postmortemPublishedAt = undefined;
		}
		inc.postmortem = body;
		return clone(inc);
	},

	// public rendering
	async publicStatusPage(slug: string, password?: string, range?: PublicRange) {
		await delay();
		const p = statusPages.find((x) => x.slug === slug && x.published);
		if (!p) throw { title: 'Not found', status: 404 };
		// A protected page answers 401 until unlocked. Once the right password is
		// supplied the page is remembered as unlocked for the session, standing in
		// for the server's unlock cookie so later views need no password.
		if (p.hasPassword && !unlockedPages.has(p.id)) {
			if (!password || password !== pagePasswords.get(p.id)) {
				throw { title: 'Unauthorized', status: 401, detail: 'password required' };
			}
			unlockedPages.add(p.id);
		}
		// An unknown range falls back to the default, matching parsePublicRange.
		const known: PublicRange[] = ['24h', '7d', '30d', '90d', '1y'];
		return buildPublicPage(p, range && known.includes(range) ? range : '90d');
	},

	/**
	 * The availability archive.
	 *
	 * Mirrors the server's rules deliberately, because a mock that is looser
	 * teaches the UI a contract that does not exist: the same password gate, the
	 * same clamp on `months` (never a rejection), `show_uptime: false` monitors
	 * omitted entirely, and -1 for a month with no data.
	 */
	async publicStatusHistory(slug: string, password?: string, months?: number) {
		await delay();
		const p = statusPages.find((x) => x.slug === slug && x.published);
		if (!p) throw { title: 'Not found', status: 404 };
		if (p.hasPassword && !unlockedPages.has(p.id)) {
			if (!password || password !== pagePasswords.get(p.id)) {
				throw { title: 'Unauthorized', status: 401, detail: 'password required' };
			}
			unlockedPages.add(p.id);
		}

		const n = !months || months < 1 ? 24 : Math.min(months, 120);
		const now = new Date();
		const starts: Date[] = [];
		for (let i = n - 1; i >= 0; i--) {
			starts.push(new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth() - i, 1)));
		}

		const rows: PublicHistoryMonitor[] = p.monitors
			.filter((r) => r.showUptime)
			.map((r, mi) => {
				const mon = monitors.find((m) => m.id === r.monitorId);
				// The first two months of the oldest year carry no data, so the
				// "monitor did not exist yet" branch is visible in a mock build.
				const bars: PublicDayBar[] = starts.map((s, i) => {
					if (i < 2) {
						return {
							start: s.toISOString(),
							date: s.toISOString().slice(0, 10),
							status: 'none',
							uptime: -1
						};
					}
					const uptime = Math.min(1, 0.982 + ((i * 7 + mi * 3) % 18) / 1000);
					return {
						start: s.toISOString(),
						date: s.toISOString().slice(0, 10),
						status: uptime >= 0.9999 ? 'up' : uptime >= 0.95 ? 'degraded' : 'down',
						uptime
					};
				});
				const known = bars.filter((b) => b.uptime >= 0);
				return {
					id: numId(r.monitorId),
					name: mon?.name ?? r.monitorId,
					months: bars,
					uptime: known.length ? known.reduce((a, b) => a + b.uptime, 0) / known.length : -1
				};
			});

		const overall = starts.map((_, i) => {
			const vals = rows.map((r) => r.months[i].uptime).filter((v) => v >= 0);
			return vals.length ? vals.reduce((a, b) => a + b, 0) / vals.length : -1;
		});

		return {
			slug: p.slug,
			title: p.title,
			theme: p.theme,
			logo_path: p.logoPath,
			accent_color: p.accentColor,
			locale: p.locale,
			allow_indexing: p.allowIndexing,
			generated_at: new Date().toISOString(),
			months: starts.map((s) => s.toISOString()),
			overall,
			monitors: rows
		};
	}
};
