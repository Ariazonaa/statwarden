// Pure model helpers for the status-page layout editor. These live in a plain
// module rather than in LayoutSorter.svelte because a runes-mode instance
// `export` is a component-instance export, not an importable binding.
import type { StatusPageLayoutOrder } from '$lib/api';

/** One droppable column of the layout editor. containers[0] is always the
 *  ungrouped pseudo-group and carries id === null. */
export interface Container {
	id: string | null;
	name: string;
	monitors: { id: string; name: string }[];
}

/** Flatten the editor model into the wire payload: containers[0] becomes
 *  `ungrouped`, the rest become `groups` in display order. */
export function toOrder(containers: Container[]): StatusPageLayoutOrder {
	const [ungrouped, ...groups] = containers;
	return {
		ungrouped: (ungrouped?.monitors ?? []).map((m) => m.id),
		groups: groups.map((c) => ({ groupId: c.id!, monitorIds: c.monitors.map((m) => m.id) }))
	};
}

function clamp(v: number, lo: number, hi: number): number {
	return v < lo ? lo : v > hi ? hi : v;
}

/** Immutable move primitives, shared by the pointer and keyboard paths. Both
 *  clamp their indices so a bad call is a no-op rather than a splice bug. */
export function moveGroup(cs: Container[], from: number, to: number): Container[] {
	// containers[0] is the ungrouped pseudo-group and never moves, so both ends
	// clamp into [1, length - 1]; fewer than two real groups means nothing to do.
	if (cs.length < 3) return cs;
	const f = clamp(from, 1, cs.length - 1);
	const t = clamp(to, 1, cs.length - 1);
	if (f === t) return cs;
	const next = cs.slice();
	const [moved] = next.splice(f, 1);
	next.splice(t, 0, moved);
	return next;
}

export function moveMonitor(
	cs: Container[],
	fromC: number,
	fromI: number,
	toC: number,
	toI: number
): Container[] {
	if (cs.length === 0) return cs;
	const fc = clamp(fromC, 0, cs.length - 1);
	const tc = clamp(toC, 0, cs.length - 1);
	const source = cs[fc];
	if (source.monitors.length === 0) return cs;
	const fi = clamp(fromI, 0, source.monitors.length - 1);

	const next = cs.slice();
	const moving = source.monitors[fi];
	const without = source.monitors.slice();
	without.splice(fi, 1);
	next[fc] = { ...source, monitors: without };

	// Read the target list *after* the removal, so a move inside one container
	// indexes into the list the item is about to be re-inserted into.
	const target = next[tc];
	const monitors = target.monitors.slice();
	const ti = clamp(toI, 0, monitors.length);
	if (fc === tc && fi === ti) return cs;
	monitors.splice(ti, 0, moving);
	next[tc] = { ...target, monitors };
	return next;
}
