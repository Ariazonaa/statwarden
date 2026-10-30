/**
 * The stored dashboard arrangement must survive a build that cannot render it.
 *
 * sanitizeLayout drops widget types this build does not know about — correct
 * for rendering, destructive for storing. One browser reaching two versions of
 * Statwarden is all it takes: a tab left on the old one, a rollback, a stale
 * service worker.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const KEY = 'statwarden:prefs';

/** A layout containing one widget this build has never heard of. */
const storedFromTheFuture = {
	density: 'comfortable',
	favorites: [],
	order: [],
	dashboard: [
		{ type: 'summary', width: 'full' },
		{ type: 'flux-capacitor', width: 'half' },
		{ type: 'activity', width: 'half' }
	]
};

async function freshStore() {
	vi.resetModules();
	const mod = await import('./prefs.svelte');
	return mod.prefs;
}

function stored() {
	return JSON.parse(localStorage.getItem(KEY) ?? '{}');
}

beforeEach(() => {
	localStorage.clear();
});
afterEach(() => {
	vi.resetModules();
});

describe('prefs.dashboard', () => {
	it('renders only the widgets it understands', async () => {
		localStorage.setItem(KEY, JSON.stringify(storedFromTheFuture));
		const prefs = await freshStore();
		expect(prefs.dashboard.map((w) => w.type)).toEqual(['summary', 'activity']);
	});

	it('writes the stored layout back untouched when an unrelated preference changes', async () => {
		localStorage.setItem(KEY, JSON.stringify(storedFromTheFuture));
		const prefs = await freshStore();

		prefs.toggleDensity();
		prefs.toggleFavorite('mon_1');

		expect(stored().density).toBe('compact');
		expect(stored().favorites).toEqual(['mon_1']);
		// The unknown widget is still there. The user never touched the dashboard.
		expect(stored().dashboard).toEqual(storedFromTheFuture.dashboard);
	});

	it('writes the sanitized layout once the arrangement is actually changed', async () => {
		localStorage.setItem(KEY, JSON.stringify(storedFromTheFuture));
		const prefs = await freshStore();

		prefs.setDashboard([{ type: 'summary', width: 'full' }]);

		expect(stored().dashboard).toEqual([{ type: 'summary', width: 'full' }]);
	});

	it('leaves the key absent for a browser that has never arranged anything', async () => {
		const prefs = await freshStore();
		prefs.toggleDensity();
		// Absent means "never arranged" and yields the default layout; writing an
		// array here would freeze today's default into every old browser.
		expect(stored().dashboard).toBeUndefined();
		expect(prefs.dashboard.length).toBeGreaterThan(0);
	});

	it('keeps a deliberately emptied dashboard empty', async () => {
		const prefs = await freshStore();
		prefs.setDashboard([]);
		expect(stored().dashboard).toEqual([]);
		const reopened = await freshStore();
		expect(reopened.dashboard).toEqual([]);
	});
});

describe('prefs.setDashboard', () => {
	it('does not mark the layout touched when nothing actually changed', async () => {
		localStorage.setItem(KEY, JSON.stringify(storedFromTheFuture));
		const prefs = await freshStore();

		// reorderWidget returns the SAME array instance when a widget is dropped
		// onto itself, and the drop handler passes that straight to setDashboard.
		prefs.setDashboard(prefs.dashboard);
		prefs.toggleDensity();

		expect(stored().dashboard).toEqual(storedFromTheFuture.dashboard);
	});

	it('does not mark it touched for an equal-but-new array either', async () => {
		localStorage.setItem(KEY, JSON.stringify(storedFromTheFuture));
		const prefs = await freshStore();

		prefs.setDashboard(prefs.dashboard.map((w) => ({ ...w })));
		prefs.toggleDensity();

		expect(stored().dashboard).toEqual(storedFromTheFuture.dashboard);
	});

	it('still persists a real rearrangement', async () => {
		localStorage.setItem(KEY, JSON.stringify(storedFromTheFuture));
		const prefs = await freshStore();

		prefs.setDashboard([{ type: 'activity', width: 'full' }]);

		expect(stored().dashboard).toEqual([{ type: 'activity', width: 'full' }]);
	});
});
