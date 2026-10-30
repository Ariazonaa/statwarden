/**
 * Round-trip guard for the wire boundary in ./client.ts.
 *
 * The mappers are module-private, so these tests drive them the way the app
 * does: with `VITE_USE_MOCK=false` the exported `api` facade is the real fetch
 * client, so a stubbed `fetch` lets a test feed a wire body in through a read
 * and read the wire body back out of the following write.
 *
 * The shape of every write assertion is deliberate: `toEqual(omit(wire, …))`
 * rather than a hand-copied literal. The omitted keys are exactly the fields
 * the server owns, so the list documents what is knowingly not written — and a
 * renamed key fails, because the body would carry a name the wire fixture does
 * not have AND miss one it does.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Monitor, Settings, Tag } from './types';

// USE_MOCK is read once at module evaluation, so the stub has to be in place
// before the import — hence the dynamic import rather than a static one.
vi.stubEnv('VITE_USE_MOCK', 'false');
const { api, ApiError, csrfToken, problemStatus, wsHeartbeatFromWire, wsStateChangeFromWire } =
	await import('./client');

interface Call {
	method: string;
	path: string;
	body: unknown;
	headers: Record<string, string>;
}

let calls: Call[] = [];
let replies: { status: number; body?: unknown }[] = [];

/** Queue one JSON reply per fetch, in call order. */
function reply(body: unknown, status = 200) {
	replies.push({ status, body });
}

/** The write body of the nth (default: last) captured request. */
function sentBody(n = calls.length - 1): Record<string, unknown> {
	return calls[n].body as Record<string, unknown>;
}

function omit<T extends object>(o: T, keys: string[]): Record<string, unknown> {
	return Object.fromEntries(Object.entries(o).filter(([k]) => !keys.includes(k)));
}

beforeEach(() => {
	calls = [];
	replies = [];
	vi.stubGlobal('fetch', async (url: string, init: RequestInit) => {
		const headers = (init.headers ?? {}) as Record<string, string>;
		calls.push({
			method: init.method ?? 'GET',
			path: String(url),
			body: typeof init.body === 'string' ? JSON.parse(init.body) : init.body,
			headers
		});
		const r = replies.shift() ?? { status: 200, body: {} };
		const text = r.body === undefined ? '' : JSON.stringify(r.body);
		return {
			ok: r.status >= 200 && r.status < 300,
			status: r.status,
			statusText: '',
			text: async () => text
		};
	});
});

afterEach(() => {
	vi.unstubAllGlobals();
});

// ---- monitors --------------------------------------------------------------

const wireMonitor = {
	id: 7,
	name: 'API',
	type: 'http',
	config: { url: 'https://example.test/health', method: 'GET' },
	interval_seconds: 30,
	timeout_seconds: 10,
	max_retries: 2,
	recovery_checks: 3,
	retry_interval_seconds: 45,
	resend_interval_minutes: 15,
	upside_down: true,
	active: false,
	status: 'down',
	parent_id: 3,
	group_id: 4,
	description: 'the public api',
	link_url: 'https://docs.example.test',
	tag_ids: [1, 2],
	notification_ids: [5],
	url: 'https://example.test/health',
	created_at: '2026-01-02T03:04:05Z',
	latency_ms: 123,
	last_checked_at: '2026-01-02T03:05:00Z',
	uptime_24h: 0.75,
	uptime_30d: 0.5,
	uptime_1y: null,
	sparkline: [10, 20, 30],
	recent_beats: ['up', 'down']
};

/** Fields monitorToWire deliberately drops: server-derived or server-owned. */
const MONITOR_READONLY = [
	'id',
	'status',
	'url',
	'created_at',
	'latency_ms',
	'last_checked_at',
	'uptime_24h',
	'uptime_30d',
	'uptime_1y',
	'sparkline',
	'recent_beats'
];

describe('monitor mapper', () => {
	it('reads every wire field into the model', async () => {
		reply(wireMonitor);
		const m = await api.getMonitor('7');
		expect(m).toEqual({
			id: '7',
			name: 'API',
			type: 'http',
			url: 'https://example.test/health',
			status: 'down',
			active: false,
			interval: 30,
			timeout: 10,
			maxRetries: 2,
			recoveryChecks: 3,
			retryInterval: 45,
			resendInterval: 15,
			upsideDown: true,
			tagIds: ['1', '2'],
			notificationIds: ['5'],
			parentId: '3',
			groupId: '4',
			description: 'the public api',
			externalUrl: 'https://docs.example.test',
			config: { url: 'https://example.test/health', method: 'GET' },
			latencyMs: 123,
			uptime24h: 75,
			uptime30d: 50,
			uptime1y: undefined,
			sparkline: [10, 20, 30],
			recentBeats: ['up', 'down'],
			lastCheckedAt: '2026-01-02T03:05:00Z',
			createdAt: '2026-01-02T03:04:05Z'
		});
	});

	it('round-trips wire → model → wire for every writable field', async () => {
		reply(wireMonitor);
		const m = await api.getMonitor('7');
		reply(wireMonitor);
		await api.updateMonitor('7', m);
		expect(calls[1].method).toBe('PUT');
		expect(sentBody()).toEqual(omit(wireMonitor, MONITOR_READONLY));
	});

	it('scales the 0..1 uptime fraction to a percentage and widens null to undefined', async () => {
		reply({ ...wireMonitor, uptime_24h: null, uptime_30d: 1, uptime_1y: 0 });
		const m = await api.getMonitor('7');
		expect(m.uptime24h).toBeUndefined();
		expect(m.uptime30d).toBe(100);
		expect(m.uptime1y).toBe(0);
	});

	it('substitutes null for the nullable columns rather than omitting them', async () => {
		reply({
			...wireMonitor,
			parent_id: null,
			group_id: null,
			timeout_seconds: null,
			tag_ids: null
		});
		const m = await api.getMonitor('7');
		expect(m.parentId).toBeUndefined();
		expect(m.groupId).toBeUndefined();
		expect(m.timeout).toBe(0);
		expect(m.tagIds).toEqual([]);

		reply(wireMonitor);
		await api.updateMonitor('7', m);
		// A full-replace write must still name the columns, or the server cannot
		// tell "leave it" from "clear it".
		expect(sentBody()).toHaveProperty('parent_id', null);
		expect(sentBody()).toHaveProperty('group_id', null);
		expect(sentBody()).toHaveProperty('timeout_seconds', null);
		expect(sentBody()).toHaveProperty('tag_ids', []);
	});

	it('falls back retryInterval to the interval when a partial omits it', async () => {
		reply(wireMonitor);
		await api.createMonitor({ name: 'x', interval: 300 } as Partial<Monitor>);
		expect(sentBody()).toMatchObject({ interval_seconds: 300, retry_interval_seconds: 300 });
	});

	it('rejects a non-numeric id instead of sending NaN to an int64 column', () => {
		// Synchronous: the mapper runs before the request is ever built, so nothing
		// reaches the wire.
		expect(() => api.updateMonitor('7', { tagIds: ['abc'] })).toThrow(/non-numeric id/);
		expect(calls).toHaveLength(0);
	});
});

describe('heartbeat and event mappers', () => {
	const beats = [
		{
			monitor_id: 7,
			checked_at: '2026-01-02T03:10:00Z',
			status: 'up',
			latency_ms: 12,
			message: 'ok',
			important: true,
			retries: 0
		},
		{
			monitor_id: 7,
			checked_at: '2026-01-02T03:00:00Z',
			status: 'down',
			important: true,
			retries: 2
		}
	];

	it('returns heartbeats oldest-first for the charts', async () => {
		reply({ monitor_id: 7, heartbeats: beats });
		const hb = await api.heartbeats('7', '24h');
		expect(hb.map((h) => h.checkedAt)).toEqual(['2026-01-02T03:00:00Z', '2026-01-02T03:10:00Z']);
		expect(hb[1]).toEqual({
			monitorId: '7',
			status: 'up',
			latencyMs: 12,
			message: 'ok',
			checkedAt: '2026-01-02T03:10:00Z'
		});
		expect(hb[0].latencyMs).toBeNull();
	});

	it('asks for a bigger heartbeat budget on a wider range', async () => {
		reply({ monitor_id: 7, heartbeats: [] });
		await api.heartbeats('7', '24h');
		reply({ monitor_id: 7, heartbeats: [] });
		await api.heartbeats('7', '1y');
		// `points`, not `limit`: in window mode the server discards limit in
		// favour of its own 50,000-beat ceiling and downsamples only when given
		// points, so sending limit shipped the entire window every time.
		const points = (n: number) =>
			Number(new URL(calls[n].path, 'http://t').searchParams.get('points'));
		expect(points(0)).toBeGreaterThan(0);
		expect(points(0)).toBeLessThan(points(1));
	});

	it('sends points so the server actually downsamples the window', async () => {
		reply({ monitor_id: 7, heartbeats: [] });
		await api.heartbeats('7', '30d');
		const q = new URL(calls[0].path, 'http://t').searchParams;
		expect(q.get('points')).toBeTruthy();
		// limit is ignored by the server in window mode; sending it was the bug.
		expect(q.get('limit')).toBeNull();
	});

	it('bounds the range in wall-clock time with from/to, not just a row limit', async () => {
		// The bug: a named range mapped only to a ?limit= of the newest beats, so
		// "24h" on a 60s interval was really the last ~100 minutes. from/to (RFC3339,
		// matching the server's parseReportRange) makes the window genuinely 24h.
		reply({ monitor_id: 7, heartbeats: [] });
		await api.heartbeats('7', '24h');
		const url = new URL(calls[0].path, 'http://t');
		const from = url.searchParams.get('from');
		const to = url.searchParams.get('to');
		expect(from).toBeTruthy();
		expect(to).toBeTruthy();
		// RFC3339 timestamps, not unix seconds.
		expect(from).toMatch(/^\d{4}-\d\d-\d\dT/);
		const span = Date.parse(to as string) - Date.parse(from as string);
		expect(span).toBeGreaterThan(23 * 3600 * 1000);
		expect(span).toBeLessThan(25 * 3600 * 1000);
	});

	it('downsamples an over-dense response to the requested point budget', async () => {
		// The server downsamples across the window, but a dense response must never
		// reach the chart raw. 1000 beats in, at most ~120 (the 24h budget) out.
		// Newest-first, as the server answers: index 0 is the most recent beat.
		const many = Array.from({ length: 1000 }, (_, i) => ({
			monitor_id: 7,
			checked_at: new Date(1_700_000_000_000 - i * 60_000).toISOString(),
			status: 'up',
			latency_ms: 999 - i,
			important: false,
			retries: 0
		}));
		reply({ monitor_id: 7, heartbeats: many });
		const hb = await api.heartbeats('7', '24h');
		expect(hb.length).toBeLessThanOrEqual(121);
		expect(hb.length).toBeGreaterThan(1);
		// Oldest-first, and the newest beat is retained as the tail.
		expect(hb[0].checkedAt < hb[hb.length - 1].checkedAt).toBe(true);
		expect(hb[hb.length - 1].latencyMs).toBe(999);
	});

	it('derives from/durationSec of an event from the next (older) transition', async () => {
		reply({ monitor_id: 7, heartbeats: beats });
		const events = await api.events('7');
		expect(events[0]).toEqual({
			id: '7:2026-01-02T03:10:00Z',
			monitorId: '7',
			from: 'down',
			to: 'up',
			message: 'ok',
			durationSec: 600,
			at: '2026-01-02T03:10:00Z'
		});
		// The oldest transition has no predecessor to measure against.
		expect(events[1].from).toBeNull();
		expect(events[1].durationSec).toBeUndefined();
	});

	it('tolerates a null heartbeats array (Go marshals a nil slice as null)', async () => {
		reply({ monitor_id: 7, heartbeats: null });
		await expect(api.heartbeats('7', '24h')).resolves.toEqual([]);
	});
});

// ---- tags, notifications, maintenance --------------------------------------

describe('tag mapper', () => {
	const wire = { id: 4, name: 'prod', color: '#ff0000', created_at: '2026-01-01T00:00:00Z' };

	it('round-trips wire → model → wire', async () => {
		reply([wire]);
		const [tag] = await api.listTags();
		expect(tag).toEqual({ id: '4', name: 'prod', color: '#ff0000' });
		reply(wire);
		await api.updateTag(tag.id, tag);
		expect(sentBody()).toEqual(omit(wire, ['id', 'created_at']));
	});

	it('tolerates a null list body', async () => {
		reply(null);
		await expect(api.listTags()).resolves.toEqual([]);
	});
});

describe('monitor group mapper', () => {
	const wire = { id: 9, name: 'Edge & web', sort_order: 2, created_at: '2026-01-01T00:00:00Z' };

	it('round-trips wire → model → wire', async () => {
		reply([wire]);
		const [group] = await api.listMonitorGroups();
		expect(group).toEqual({ id: '9', name: 'Edge & web', sortOrder: 2 });
		reply(wire);
		await api.updateMonitorGroup(group.id, group);
		expect(sentBody()).toEqual(omit(wire, ['id', 'created_at']));
	});

	it('tolerates a null list body', async () => {
		reply(null);
		await expect(api.listMonitorGroups()).resolves.toEqual([]);
	});
});

describe('bulk monitor mapper', () => {
	it('sends snake_case ids and maps per-id outcomes back', async () => {
		reply({
			action: 'pause',
			succeeded: 1,
			failed: 1,
			results: [
				{ monitor_id: 4, ok: true },
				{ monitor_id: 5, ok: false, error: 'not found' }
			]
		});
		const res = await api.bulkMonitors({ action: 'pause', monitorIds: ['4', '5'] });
		expect(calls[0].method).toBe('POST');
		expect(calls[0].path).toContain('/monitors/bulk');
		// Numeric ids, not the client's id strings: the Go handler decodes []int64
		// and would reject "4" outright.
		expect(sentBody()).toEqual({ action: 'pause', monitor_ids: [4, 5] });
		expect(res).toEqual({
			action: 'pause',
			succeeded: 1,
			failed: 1,
			results: [
				{ monitorId: '4', ok: true, error: undefined },
				{ monitorId: '5', ok: false, error: 'not found' }
			]
		});
	});

	it('omits tag_ids when the action does not take them', async () => {
		reply({ action: 'delete', succeeded: 1, failed: 0, results: [{ monitor_id: 4, ok: true }] });
		await api.bulkMonitors({ action: 'delete', monitorIds: ['4'] });
		expect(sentBody()).not.toHaveProperty('tag_ids');
		expect(sentBody()).not.toHaveProperty('group_id');
	});

	it('sends group_id: null to clear the folder, distinctly from omitting it', async () => {
		// The distinction is load-bearing: `group` with a null group_id is how a
		// monitor is unfiled, so a mapper that dropped the key would turn "remove
		// from folder" into a request the server reads as the same thing only by
		// luck — and any future non-null default would silently refile monitors.
		reply({ action: 'group', succeeded: 1, failed: 0, results: [{ monitor_id: 4, ok: true }] });
		await api.bulkMonitors({ action: 'group', monitorIds: ['4'], groupId: null });
		expect(sentBody()).toEqual({ action: 'group', monitor_ids: [4], group_id: null });

		reply({ action: 'group', succeeded: 1, failed: 0, results: [{ monitor_id: 4, ok: true }] });
		await api.bulkMonitors({ action: 'group', monitorIds: ['4'], groupId: '7' });
		expect(sentBody()).toEqual({ action: 'group', monitor_ids: [4], group_id: 7 });
	});

	it('maps tag_ids for the tag action', async () => {
		reply({ action: 'tag', succeeded: 1, failed: 0, results: [{ monitor_id: 4, ok: true }] });
		await api.bulkMonitors({ action: 'tag', monitorIds: ['4'], tagIds: ['1', '2'] });
		expect(sentBody()).toEqual({ action: 'tag', monitor_ids: [4], tag_ids: [1, 2] });
	});

	it('tolerates a null results array', async () => {
		reply({ action: 'pause', succeeded: 0, failed: 0, results: null });
		await expect(api.bulkMonitors({ action: 'pause', monitorIds: ['4'] })).resolves.toEqual({
			action: 'pause',
			succeeded: 0,
			failed: 0,
			results: []
		});
	});
});

describe('notification mapper', () => {
	const wire = {
		id: 9,
		name: 'ops email',
		type: 'smtp',
		config: { to: 'ops@example.test' },
		is_default: true,
		active: false,
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-05T00:00:00Z'
	};

	it('round-trips wire → model → wire', async () => {
		reply([wire]);
		const [n] = await api.listNotifications();
		expect(n).toEqual({
			id: '9',
			name: 'ops email',
			type: 'smtp',
			isDefault: true,
			enabled: false,
			config: { to: 'ops@example.test' },
			createdAt: '2026-01-01T00:00:00Z'
		});
		reply(wire);
		await api.updateNotification(n.id, n);
		expect(sentBody()).toEqual(omit(wire, ['id', 'created_at', 'updated_at']));
	});
});

describe('maintenance mapper', () => {
	const wire = {
		id: 3,
		title: 'db upgrade',
		description: 'moving to 17',
		active: true,
		strategy: 'single',
		starts_at: '2026-02-01T22:00:00Z',
		ends_at: '2026-02-02T02:00:00Z',
		timezone: 'UTC',
		monitor_ids: [1, 2],
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z'
	};

	it('round-trips wire → model → wire, restating the request defaults', async () => {
		reply([wire]);
		const [w] = await api.listMaintenance();
		expect(w).toEqual({
			id: '3',
			title: 'db upgrade',
			description: 'moving to 17',
			monitorIds: ['1', '2'],
			strategy: 'single',
			startsAt: '2026-02-01T22:00:00Z',
			endsAt: '2026-02-02T02:00:00Z',
			recurrence: undefined,
			timezone: 'UTC',
			active: true
		});
		reply(wire);
		await api.updateMaintenance(w.id, w);
		// `recurrence` has no wire counterpart on read but is part of the request,
		// and strategy/timezone are restated rather than left to the server.
		expect(sentBody()).toEqual({
			...omit(wire, ['id', 'created_at', 'updated_at']),
			recurrence: null
		});
	});

	// Replaces an earlier "KNOWN LOSS" test which asserted that a full-replace
	// save flattened a recurring window into a one-off, because the model had no
	// recurrence fields. It does now, so the loss is gone and the inverse is
	// asserted: the round trip has to preserve the schedule.
	it('preserves a recurring window across a full-replace save', async () => {
		const recurringWire = {
			...wire,
			strategy: 'recurring',
			starts_at: undefined,
			ends_at: undefined,
			recurrence: { weekdays: [1, 3, 5], start: '02:00', duration_minutes: 120 },
			timezone: 'Europe/Berlin'
		};
		reply([recurringWire]);
		const [w] = await api.listMaintenance();
		expect(w).toMatchObject({
			strategy: 'recurring',
			recurrence: { weekdays: [1, 3, 5], start: '02:00', durationMinutes: 120 },
			timezone: 'Europe/Berlin'
		});

		reply(recurringWire);
		await api.updateMaintenance(w.id, w);
		// duration_minutes, not durationMinutes: the server decodes strictly and
		// rejects unknown fields, so a camelCase leak here would be a 400.
		expect(sentBody()).toMatchObject({
			strategy: 'recurring',
			recurrence: { weekdays: [1, 3, 5], start: '02:00', duration_minutes: 120 },
			timezone: 'Europe/Berlin',
			starts_at: null,
			ends_at: null
		});
	});

	it('drops the recurrence when a window is switched back to one-off', async () => {
		// A stale recurrence left on a `single` window would be stored even
		// though nothing evaluates it, so the mapper sends an explicit null.
		reply(wire);
		await api.updateMaintenance('3', {
			title: 'db upgrade',
			strategy: 'single',
			startsAt: '2026-02-01T22:00:00Z',
			endsAt: '2026-02-02T02:00:00Z',
			recurrence: { weekdays: [1], start: '02:00', durationMinutes: 60 },
			timezone: 'UTC'
		});
		expect(sentBody()).toMatchObject({ strategy: 'single', recurrence: null });
	});

	it('reads an unrecognised strategy back as one-off', async () => {
		// The column is constrained to two values; anything else must not
		// present as a recurring window with no recurrence to render.
		reply([{ ...wire, strategy: 'recurring-weekday' }]);
		const [w] = await api.listMaintenance();
		expect(w.strategy).toBe('single');
	});

	it('sends null, not an empty string, for an unset window boundary', async () => {
		reply(wire);
		await api.createMaintenance({ title: 'x', startsAt: '', endsAt: '' });
		expect(sentBody()).toMatchObject({ starts_at: null, ends_at: null });
	});
});

// ---- users, sessions, api keys ---------------------------------------------

describe('user mapper', () => {
	// display_name equals username: the model carries one name and the write
	// mappers keep the server column in lockstep with it, so the round-trip below
	// (which compares the write body against the wire read shape) stays exact.
	const wire = {
		id: 2,
		email: 'ada@example.test',
		username: 'ada',
		display_name: 'ada',
		role: 'admin',
		totp_enabled: true,
		avatar_path: '/uploads/a.png',
		locale: 'de',
		is_active: false,
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z'
	};

	it('round-trips wire → model → wire and inverts is_active', async () => {
		reply([wire]);
		const [u] = await api.listUsers();
		expect(u).toEqual({
			id: '2',
			username: 'ada',
			email: 'ada@example.test',
			role: 'admin',
			avatarUrl: '/uploads/a.png',
			totpEnabled: true,
			disabled: true,
			createdAt: '2026-01-01T00:00:00Z'
		});
		reply(wire);
		await api.updateUser(u.id, u);
		expect(sentBody()).toEqual(
			omit(wire, ['id', 'totp_enabled', 'avatar_path', 'locale', 'created_at', 'updated_at'])
		);
	});

	it('does not invent a lastLoginAt the users table cannot serve', async () => {
		reply([wire]);
		const [u] = await api.listUsers();
		expect(u.lastLoginAt).toBeUndefined();
	});
});

describe('read-only mappers', () => {
	it('maps a session', async () => {
		reply([
			{
				id: 'abc',
				user_id: 2,
				created_at: '2026-01-01T00:00:00Z',
				expires_at: '2026-02-01T00:00:00Z',
				last_seen_at: '2026-01-09T00:00:00Z',
				ip: '10.0.0.1',
				user_agent: 'Firefox',
				current: true
			}
		]);
		await expect(api.listSessions()).resolves.toEqual([
			{
				id: 'abc',
				userAgent: 'Firefox',
				ip: '10.0.0.1',
				current: true,
				createdAt: '2026-01-01T00:00:00Z',
				lastSeenAt: '2026-01-09T00:00:00Z'
			}
		]);
	});

	it('maps an api key and only carries the secret on create', async () => {
		const wire = {
			id: 5,
			user_id: 2,
			name: 'ci',
			prefix: 'upt_ab',
			scopes: ['read'],
			last_used_at: '2026-01-08T00:00:00Z',
			created_at: '2026-01-01T00:00:00Z'
		};
		reply([wire]);
		const [k] = await api.listApiKeys();
		expect(k).toEqual({
			id: '5',
			name: 'ci',
			scopes: ['read'],
			prefix: 'upt_ab',
			token: undefined,
			lastUsedAt: '2026-01-08T00:00:00Z',
			createdAt: '2026-01-01T00:00:00Z'
		});
		reply({ api_key: wire, secret: 'upt_ab_plaintext' });
		const created = await api.createApiKey({
			name: 'ci',
			scopes: ['read'],
			confirm: { password: 'pw', totpCode: '123456' }
		});
		expect(created.token).toBe('upt_ab_plaintext');
		// The re-authentication travels flattened and snake_cased, alongside the
		// key's own fields — the server reads one object (createAPIKeyRequest
		// embeds stepUp), not a nested one. A wrong shape here does not fail
		// loudly: the server sees an empty password and answers 403, which looks
		// exactly like the user mistyping.
		expect(sentBody()).toEqual({
			name: 'ci',
			scopes: ['read'],
			password: 'pw',
			totp_code: '123456'
		});
	});

	it('omits an undefined notification config so an update keeps the stored one', async () => {
		// A channel's config is hidden wholesale on read, so an editor opened on
		// an existing channel starts blank. Sending that blank form back as the
		// new configuration is how renaming a channel came to wipe its
		// credentials; the server reads an ABSENT config as "leave it alone", so
		// the key must genuinely not be on the wire. `{}` would mean the
		// opposite, which is why this cannot be tested by value alone.
		reply({ id: 1, name: 'ops', type: 'fake', config: {}, is_default: false, active: true });
		await api.updateNotification('1', { name: 'ops (renamed)', type: 'fake' });
		expect('config' in sentBody()).toBe(false);

		reply({ id: 1, name: 'ops', type: 'fake', config: {}, is_default: false, active: true });
		await api.updateNotification('1', { name: 'ops', type: 'fake', config: { token: 'new' } });
		expect(sentBody().config).toEqual({ token: 'new' });
	});

	it('maps an audit entry and labels an actorless row as system', async () => {
		reply({
			items: [
				{
					id: 1,
					user_id: 2,
					action: 'monitor.create',
					entity_type: 'monitor',
					entity_id: 7,
					created_at: '2026-01-01T00:00:00Z'
				},
				{ id: 2, action: 'retention.prune', created_at: '2026-01-01T00:00:00Z' }
			],
			total: 2,
			limit: 50,
			offset: 0
		});
		const rows = await api.auditLog();
		expect(rows[0]).toEqual({
			id: '1',
			actor: '2',
			action: 'monitor.create',
			target: '7',
			targetType: 'monitor',
			at: '2026-01-01T00:00:00Z'
		});
		expect(rows[1]).toMatchObject({ actor: 'system', target: '', targetType: '' });
	});
});

// ---- settings ---------------------------------------------------------------

describe('settings mapper', () => {
	const rows = [
		{ key: 'instance_name', value: 'Acme', updated_at: '2026-01-01T00:00:00Z' },
		{ key: 'base_url', value: 'https://status.example.test', updated_at: '2026-01-01T00:00:00Z' },
		{ key: 'retention_days', value: 30, updated_at: '2026-01-01T00:00:00Z' },
		{ key: 'default_interval', value: 120, updated_at: '2026-01-01T00:00:00Z' },
		{ key: 'cert_expiry_thresholds', value: [7, 1], updated_at: '2026-01-01T00:00:00Z' },
		{ key: 'session_ttl_days', value: 7, updated_at: '2026-01-01T00:00:00Z' },
		{
			key: 'audit_webhook_url',
			value: 'https://collector.example/statwarden',
			updated_at: '2026-01-01T00:00:00Z'
		},
		{
			key: 'outbound_webhook_url',
			value: 'https://n8n.example/hook',
			updated_at: '2026-01-01T00:00:00Z'
		}
	];
	// The server masks each secret row to whether it is set.
	const secretRows = [
		{ key: 'audit_webhook_secret', value: true, updated_at: '2026-01-01T00:00:00Z' },
		{ key: 'outbound_webhook_secret', value: false, updated_at: '2026-01-01T00:00:00Z' }
	];

	it('folds the key/value rows into the flat model', async () => {
		reply([...rows, ...secretRows]);
		await expect(api.getSettings()).resolves.toEqual({
			instanceName: 'Acme',
			baseUrl: 'https://status.example.test',
			retentionDays: 30,
			defaultInterval: 120,
			certExpiryThresholds: [7, 1],
			sessionTtlDays: 7,
			auditWebhookUrl: 'https://collector.example/statwarden',
			outboundWebhookUrl: 'https://n8n.example/hook',
			auditWebhookSecretSet: true,
			outboundWebhookSecretSet: false
		});
	});

	it('never writes a secret flag back over the secret', async () => {
		reply([...rows, ...secretRows]);
		const s = await api.getSettings();
		for (const _ of rows) reply(undefined, 204);
		reply([...rows, ...secretRows]);
		await api.updateSettings(s);
		const paths = calls.filter((c) => c.method === 'PUT').map((c) => c.path);
		expect(paths.some((p) => p.includes('_secret'))).toBe(false);
	});

	it('writes a webhook secret to its own row, verbatim', async () => {
		reply(undefined, 204);
		reply([...rows, ...secretRows]);
		await api.setWebhookSecret('outbound', 'whsec_0123456789abcdef');
		const writes = calls.filter((c) => c.method === 'PUT');
		expect(writes).toHaveLength(1);
		expect(writes[0].path).toMatch(/\/settings\/outbound_webhook_secret$/);
		expect(writes[0].body).toBe('whsec_0123456789abcdef');
	});

	it('falls back to the defaults for a missing or null row', async () => {
		reply([{ key: 'retention_days', value: null, updated_at: '2026-01-01T00:00:00Z' }]);
		const s = await api.getSettings();
		expect(s.retentionDays).toBe(90);
		expect(s.instanceName).toBe('Statwarden');
		expect(s.sessionTtlDays).toBe(30);
	});

	it('writes the session lifetime to its own key', async () => {
		reply(undefined, 204);
		reply(rows);
		await api.updateSettings({ sessionTtlDays: 14 } as Partial<Settings>);
		const writes = calls.filter((c) => c.method === 'PUT');
		expect(writes).toHaveLength(1);
		expect(writes[0].path).toMatch(/\/settings\/session_ttl_days$/);
		expect(writes[0].body).toBe(14);
	});

	it('round-trips every field back to its own key, one PUT each', async () => {
		reply(rows);
		const s = await api.getSettings();
		for (const _ of rows) reply(undefined, 204);
		reply(rows);
		await api.updateSettings(s);
		const writes = calls.filter((c) => c.method === 'PUT');
		expect(writes.map((c) => c.path.replace(/^.*\/settings\//, ''))).toEqual(
			rows.map((r) => r.key)
		);
		expect(writes.map((c) => c.body)).toEqual(rows.map((r) => r.value));
	});

	it('only writes the keys a partial patch names', async () => {
		reply(undefined, 204);
		reply(rows);
		await api.updateSettings({ retentionDays: 7 } as Partial<Settings>);
		const writes = calls.filter((c) => c.method === 'PUT');
		expect(writes).toHaveLength(1);
		expect(writes[0].path).toMatch(/\/settings\/retention_days$/);
	});
});

// ---- status pages, incidents ------------------------------------------------

const wireStatusPage = {
	id: 1,
	slug: 'public',
	title: 'Acme Status',
	description: 'how we are doing',
	footer_text: 'made with care',
	theme: 'dark',
	logo_path: '/uploads/logo.png',
	custom_domain: 'status.acme.test',
	locale: 'de',
	accent_color: '#4f46e5',
	published: true,
	allow_indexing: false,
	has_password: true,
	view_count: 42,
	monitor_ids: [1, 2],
	groups: [{ id: 10, name: 'Core', sort_order: 0 }],
	monitors: [
		{ monitor_id: 1, group_id: 10, group_name: 'Core', sort_order: 0, show_uptime: true },
		{ monitor_id: 2, group_name: '', sort_order: 1, show_uptime: false }
	],
	created_at: '2026-01-01T00:00:00Z',
	updated_at: '2026-01-05T00:00:00Z'
};

describe('status page mapper', () => {
	it('reads every wire field into the model', async () => {
		reply(wireStatusPage);
		await expect(api.getStatusPage('1')).resolves.toEqual({
			id: '1',
			slug: 'public',
			title: 'Acme Status',
			description: 'how we are doing',
			footerText: 'made with care',
			theme: 'dark',
			locale: 'de',
			accentColor: '#4f46e5',
			logoPath: '/uploads/logo.png',
			customDomain: 'status.acme.test',
			published: true,
			allowIndexing: false,
			hasPassword: true,
			viewCount: 42,
			monitorIds: ['1', '2'],
			groups: [{ id: '10', name: 'Core', sortOrder: 0 }],
			monitors: [
				{ monitorId: '1', groupId: '10', groupName: 'Core', sortOrder: 0, showUptime: true },
				{ monitorId: '2', groupId: undefined, groupName: '', sortOrder: 1, showUptime: false }
			],
			createdAt: '2026-01-01T00:00:00Z',
			updatedAt: '2026-01-05T00:00:00Z'
		});
	});

	it('round-trips wire → model → wire for the fields the editor owns', async () => {
		reply(wireStatusPage);
		const p = await api.getStatusPage('1');
		reply(wireStatusPage);
		await api.updateStatusPage('1', p);
		expect(sentBody()).toEqual({
			slug: 'public',
			title: 'Acme Status',
			description: 'how we are doing',
			footer_text: 'made with care',
			theme: 'dark',
			accent_color: '#4f46e5',
			custom_domain: 'status.acme.test',
			locale: 'de',
			// Write-only controls the response never echoes back.
			password: '',
			clear_password: false,
			published: true,
			allow_indexing: false,
			monitor_ids: [1, 2],
			// Groups are written by name; the id is server-assigned every save.
			groups: [{ name: 'Core', sort_order: 0 }],
			monitors: [
				{ monitor_id: 1, group_name: 'Core', sort_order: 0, show_uptime: true },
				{ monitor_id: 2, group_name: '', sort_order: 1, show_uptime: false }
			]
		});
	});

	it('never sends the server-owned logo_path', async () => {
		reply(wireStatusPage);
		const p = await api.getStatusPage('1');
		reply(wireStatusPage);
		await api.updateStatusPage('1', p);
		expect(sentBody()).not.toHaveProperty('logo_path');
	});

	it('falls sort_order back to the array position when the model omits it', async () => {
		reply(wireStatusPage);
		await api.createStatusPage({
			slug: 's',
			title: 't',
			theme: 'auto',
			published: false,
			allowIndexing: false,
			groups: [{ name: 'A' }, { name: 'B' }],
			monitors: [
				{ monitorId: '1', showUptime: true },
				{ monitorId: '2', showUptime: true }
			]
		});
		expect(sentBody().groups).toEqual([
			{ name: 'A', sort_order: 0 },
			{ name: 'B', sort_order: 1 }
		]);
		expect((sentBody().monitors as { sort_order: number }[]).map((m) => m.sort_order)).toEqual([
			0, 1
		]);
	});

	it('maps the layout order to snake_case numeric ids and the response back', async () => {
		reply(wireStatusPage);
		const page = await api.reorderStatusPageLayout('1', {
			ungrouped: ['2'],
			groups: [{ groupId: '10', monitorIds: ['1'] }]
		});
		expect(calls[0].method).toBe('PATCH');
		expect(calls[0].body).toEqual({
			ungrouped: [2],
			groups: [{ group_id: 10, monitor_ids: [1] }]
		});
		// The handler answers with the whole page in wire shape; without the mapper
		// the editor would re-seed itself from snake_case.
		expect(page.footerText).toBe('made with care');
	});

	it('maps the wire page back out of a logo upload', async () => {
		reply(wireStatusPage);
		const file = new File(['x'], 'logo.png', { type: 'image/png' });
		const page = await api.uploadStatusPageLogo('1', file);
		expect(page.logoPath).toBe('/uploads/logo.png');
		// Only the browser can generate a multipart boundary.
		expect(calls[0].headers).not.toHaveProperty('Content-Type');
	});
});

describe('incident mapper', () => {
	const wire = {
		id: 6,
		status_page_id: 1,
		title: 'Elevated errors',
		body: 'We are looking into it.',
		severity: 'major',
		status: 'identified',
		pinned: true,
		started_at: '2026-01-09T10:00:00Z',
		resolved_at: '2026-01-09T12:00:00Z',
		monitor_ids: [1, 2],
		updates: [
			{
				id: 20,
				incident_id: 6,
				status: 'monitoring',
				body: 'Fix deployed.',
				created_at: '2026-01-09T11:00:00Z'
			}
		],
		created_at: '2026-01-09T10:00:00Z',
		updated_at: '2026-01-09T12:00:00Z'
	};

	it('round-trips wire → model → wire for the fields the editor owns', async () => {
		reply({ items: [wire] });
		const [i] = await api.listIncidents('1');
		expect(i).toEqual({
			id: '6',
			statusPageId: '1',
			title: 'Elevated errors',
			body: 'We are looking into it.',
			severity: 'major',
			status: 'identified',
			pinned: true,
			startedAt: '2026-01-09T10:00:00Z',
			resolvedAt: '2026-01-09T12:00:00Z',
			monitorIds: ['1', '2'],
			updates: [
				{
					id: '20',
					status: 'monitoring',
					body: 'Fix deployed.',
					createdAt: '2026-01-09T11:00:00Z'
				}
			],
			// Defaulted to '' rather than left undefined so the editor can bind a
			// textarea to it; the wire omits the key entirely when there is none.
			postmortem: '',
			postmortemPublishedAt: undefined
		});
		// toEqual treats an undefined property as absent, so the line above
		// asserts nothing on its own. This is the assertion: a draft (or an
		// incident with no retrospective at all) must not arrive carrying a
		// publication date, because that is the single field the editor reads to
		// decide whether the button says "Publish" or "Unpublish".
		expect(i.postmortemPublishedAt).toBeUndefined();
	});

	it('maps a published retrospective and its timestamp', async () => {
		reply({
			items: [
				{
					...wire,
					postmortem: '## Root cause\n\nA failover nobody had exercised.',
					postmortem_published_at: '2026-01-10T08:30:00Z'
				}
			]
		});
		const [i] = await api.listIncidents('1');
		expect(i.postmortem).toBe('## Root cause\n\nA failover nobody had exercised.');
		expect(i.postmortemPublishedAt).toBe('2026-01-10T08:30:00Z');
	});

	it('keeps a draft distinguishable from a published retrospective', async () => {
		// Text but no timestamp: the ordinary intermediate state, and the one the
		// public page filters on.
		reply({ items: [{ ...wire, postmortem: 'still writing this' }] });
		const [i] = await api.listIncidents('1');
		expect(i.postmortem).toBe('still writing this');
		expect(i.postmortemPublishedAt).toBeUndefined();
	});

	it('sends back only the fields the editor owns', async () => {
		reply({ items: [wire] });
		const [i] = await api.listIncidents('1');
		reply(wire);
		await api.createIncident('1', i);
		expect(sentBody()).toEqual(
			omit(wire, [
				'id',
				'status_page_id',
				'started_at',
				'resolved_at',
				'updates',
				'created_at',
				'updated_at'
			])
		);
	});

	/**
	 * Both ids go in the path.
	 *
	 * These two used to send the literal 0 as the status-page id, under a comment
	 * claiming the handler ignored it. It stopped ignoring it — incidentForPage
	 * now refuses an incident that does not belong to the page in the URL — and
	 * since no page has id 0, every timeline post and every delete from the UI
	 * became a 404. Nothing caught it, because nothing asserted the URL.
	 */
	it('addresses an incident through its own status page, not a placeholder', async () => {
		reply(wire);
		await api.addIncidentUpdate('1', '6', { status: 'monitoring', body: 'still on it' });
		expect(calls[0].path).toBe('/api/v1/status-pages/1/incidents/6/updates');

		reply(undefined, 204);
		await api.deleteIncident('1', '6');
		expect(calls[1].path).toBe('/api/v1/status-pages/1/incidents/6');

		reply(wire);
		await api.setIncidentPostmortem('1', '6', { body: 'the write-up', published: true });
		expect(calls[2].path).toBe('/api/v1/status-pages/1/incidents/6/postmortem');
		expect(calls[2].method).toBe('PUT');
		expect(sentBody()).toEqual({ body: 'the write-up', published: true });
	});

	// The postmortem is server-owned. incidentToWire is the shape every incident
	// write uses, and decodeJSON rejects unknown fields, so a postmortem smuggled
	// in here would be a 400 rather than a silent no-op.
	it('never sends a postmortem on an incident write', async () => {
		reply({ items: [{ ...wire, postmortem: 'draft' }] });
		const [i] = await api.listIncidents('1');
		expect(i.postmortem).toBe('draft');

		reply(wire);
		await api.createIncident('1', i);
		expect(sentBody()).not.toHaveProperty('postmortem');
		expect(sentBody()).not.toHaveProperty('postmortem_published_at');
	});

	it('tolerates an absent updates array', async () => {
		reply({ items: [{ ...wire, updates: null }] });
		const [i] = await api.listIncidents('1');
		expect(i.updates).toEqual([]);
	});
});

// ---- websocket frames -------------------------------------------------------

describe('websocket frame mappers', () => {
	it('renames monitor_id and turns unix millis into an ISO timestamp', () => {
		expect(
			wsHeartbeatFromWire({
				monitor_id: 7,
				status: 'up',
				at: Date.UTC(2026, 0, 2, 3, 4, 5),
				latency_ms: 42,
				message: 'ok',
				important: false,
				retries: 0
			})
		).toEqual({
			monitorId: '7',
			status: 'up',
			latencyMs: 42,
			message: 'ok',
			checkedAt: '2026-01-02T03:04:05.000Z'
		});
	});

	it('widens a missing latency to null and an empty message to undefined', () => {
		const hb = wsHeartbeatFromWire({
			monitor_id: 7,
			status: 'down',
			at: 0,
			message: '',
			important: true,
			retries: 1
		});
		expect(hb.latencyMs).toBeNull();
		expect(hb.message).toBeUndefined();
	});

	it('reads the hub’s empty-string "no previous state" as null', () => {
		expect(
			wsStateChangeFromWire({
				monitor_id: 7,
				monitor_name: 'API',
				from: '',
				to: 'up',
				at: Date.UTC(2026, 0, 2)
			})
		).toEqual({ monitorId: '7', from: null, to: 'up', at: '2026-01-02T00:00:00.000Z' });
	});
});

// ---- transport --------------------------------------------------------------

describe('transport', () => {
	it('sends the CSRF header and same-origin credentials on a mutation', async () => {
		document.cookie = 'statwarden_csrf=tok-123';
		reply(wireMonitor);
		await api.updateMonitor('7', { name: 'x' });
		expect(calls[0].headers['X-CSRF-Token']).toBe('tok-123');
		expect(csrfToken()).toBe('tok-123');
	});

	it('returns undefined rather than parsing an empty 204 body', async () => {
		reply(undefined, 204);
		await expect(api.deleteMonitor('7')).resolves.toBeUndefined();
	});

	/**
	 * The recycle bin's three routes.
	 *
	 * `trash` is a LITERAL segment competing with `{id}` on the server, and purge
	 * is the one irreversible call in this client — both are worth pinning by URL
	 * rather than trusting that nobody reorders a route or drops a suffix.
	 */
	it('addresses the recycle bin by its own routes', async () => {
		reply({ items: [wireMonitor] });
		const binned = await api.listDeletedMonitors();
		expect(calls[0].path).toBe('/api/v1/monitors/trash');
		expect(binned[0].id).toBe('7');

		reply(wireMonitor);
		await api.restoreMonitor('7');
		expect(calls[1].method).toBe('POST');
		expect(calls[1].path).toBe('/api/v1/monitors/7/restore');

		reply(undefined, 204);
		await api.purgeMonitor('7');
		expect(calls[2].method).toBe('DELETE');
		// NOT /monitors/7 — that is the soft delete, and confusing the two would
		// turn "empty the bin" into "delete a live monitor".
		expect(calls[2].path).toBe('/api/v1/monitors/7/purge');
	});

	it('translates a problem+json body into an ApiError carrying the status', async () => {
		reply({ title: 'Conflict', status: 409, detail: 'layout is stale' }, 409);
		const err = await api
			.reorderStatusPageLayout('1', { ungrouped: [], groups: [] })
			.catch((e) => e);
		expect(err).toBeInstanceOf(ApiError);
		expect(err.message).toBe('layout is stale');
		expect(problemStatus(err)).toBe(409);
	});

	it('synthesises a problem when the error body is not problem+json', async () => {
		reply('nope', 500);
		const err = await api.getMonitor('7').catch((e) => e);
		expect(err).toBeInstanceOf(ApiError);
		expect(problemStatus(err)).toBe(500);
	});

	it('reads a status off the mock backend’s plain problem-shaped throws', () => {
		// The mock rejects with an object literal, so `instanceof ApiError` alone
		// would miss every mock failure a caller branches on.
		expect(problemStatus({ title: 'Conflict', status: 409 })).toBe(409);
		expect(problemStatus(new Error('boom'))).toBeUndefined();
		expect(problemStatus(null)).toBeUndefined();
	});
});
