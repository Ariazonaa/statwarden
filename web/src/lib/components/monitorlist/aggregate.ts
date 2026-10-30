/**
 * Folder roll-ups for the monitor list's group rows.
 *
 * A folder header shows the same three things a monitor row does — a status,
 * an uptime figure and a per-check bar — computed from the monitors inside it.
 * Pure functions, so the rules are tested without rendering anything.
 */
import type { Monitor, MonitorStatus } from '$lib/api';

/**
 * Severity order for "the worst state wins". Paused ranks lowest: a folder
 * with one paused and one up monitor is up, because the paused one is not
 * being checked and so says nothing about the folder's health.
 */
const SEVERITY: Record<MonitorStatus, number> = {
	down: 4,
	pending: 3,
	maintenance: 2,
	up: 1,
	paused: 0
};

/** The worst status among the monitors; paused only when every one is. */
export function worstStatus(items: Pick<Monitor, 'status'>[]): MonitorStatus {
	let worst: MonitorStatus = 'paused';
	for (const m of items) if (SEVERITY[m.status] > SEVERITY[worst]) worst = m.status;
	return worst;
}

/**
 * Mean 24-hour uptime over the monitors that HAVE a figure.
 *
 * A monitor with no conclusive check in the window is left out rather than
 * counted as 0 — the same rule the dashboard's "lowest availability" widget
 * applies — so a freshly created monitor does not drag its folder down.
 * Undefined when no monitor in the folder has a figure.
 */
export function meanUptime(items: Pick<Monitor, 'uptime24h'>[]): number | undefined {
	const known = items.map((m) => m.uptime24h).filter((u): u is number => u !== undefined);
	if (known.length === 0) return undefined;
	return known.reduce((a, b) => a + b, 0) / known.length;
}

/**
 * The folder's per-check bar: position by position from the NEWEST end, the
 * worst status any monitor had at that position.
 *
 * Aligned on the newest beat, not the oldest, because that is what the bar
 * shows: the right-hand pill of every row is "the last check", whatever each
 * monitor's history length. Monitors check on different intervals, so this is
 * "the last N checks of each" rather than a shared timeline — which is what a
 * glance at a folder is for: did anything in here fail recently?
 */
export function mergeBeats(items: Pick<Monitor, 'recentBeats'>[], slots = 50): MonitorStatus[] {
	const len = Math.min(slots, Math.max(0, ...items.map((m) => m.recentBeats.length)));
	const out: MonitorStatus[] = [];
	for (let back = len; back >= 1; back--) {
		let worst: MonitorStatus | undefined;
		for (const m of items) {
			const b = m.recentBeats[m.recentBeats.length - back];
			if (b && (worst === undefined || SEVERITY[b] > SEVERITY[worst])) worst = b;
		}
		if (worst) out.push(worst);
	}
	return out;
}
