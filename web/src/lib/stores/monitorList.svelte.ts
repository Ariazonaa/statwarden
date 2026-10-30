/**
 * Filter state of the persistent monitor list.
 *
 * The list lives in the monitor area's layout and outlives every page inside
 * it, but one thing outside it drives it too: the dashboard's quick stats,
 * where clicking "Down" narrows the list to the down monitors — the same
 * shortcut the old dashboard's summary tiles had over the card grid beside
 * them. A module-level store is what lets the two components share the value
 * without the layout threading props through every route.
 *
 * Deliberately NOT persisted: a filter that survives a reload is a filter
 * that hides monitors from someone who forgot they set it.
 */
import type { MonitorStatus } from '$lib/api';

export type ListSort = 'status' | 'name' | 'latency' | 'uptime' | 'created' | 'manual';

class MonitorListState {
	search = $state('');
	status = $state<'all' | MonitorStatus>('all');
	tag = $state('all');
	type = $state('all');
	sort = $state<ListSort>('status');
	groupByFolder = $state(true);

	/** How many of the narrowing filters are set (search and sort excluded). */
	get activeFilters(): number {
		return (
			(this.status !== 'all' ? 1 : 0) + (this.tag !== 'all' ? 1 : 0) + (this.type !== 'all' ? 1 : 0)
		);
	}

	/** Toggles the status filter, as the dashboard's quick stats do. */
	toggleStatus(s: 'all' | MonitorStatus) {
		this.status = this.status === s ? 'all' : s;
	}

	clearFilters() {
		this.status = 'all';
		this.tag = 'all';
		this.type = 'all';
	}
}

export const monitorList = new MonitorListState();
