/**
 * The dashboard layout: which widgets are shown, in what order, at what width.
 *
 * Everything here is pure and data-only — no Svelte, no DOM — so the rules that
 * matter (what a stored layout is allowed to contain, what happens to one
 * written by a newer version) are testable without rendering anything.
 *
 * ## Why localStorage and not the server
 *
 * This joins card density, favourites and the manual monitor order in
 * `$lib/stores/prefs.svelte.ts`, all of which already live in localStorage. A
 * dashboard arrangement is the same class of thing: a per-person, per-browser
 * view preference that nobody else needs to read and that no server rule
 * depends on. Splitting personalisation across two stores — half in
 * localStorage, half in Postgres — would be worse than either choice on its own,
 * and it would mean a new table, a Store method in BOTH implementations and a
 * migration for something that is not data anyone will miss.
 *
 * The trade is stated rather than hidden: the layout does not follow a user to
 * another browser, and clearing site data resets it to the default. That is the
 * same deal favourites already have.
 */

import type { MessageKey } from '$lib/i18n';

/**
 * The widget catalogue.
 *
 * Every entry renders from data the dashboard already has — the monitors store,
 * the maintenance list, the audit feed — so adding a widget has never required a
 * new endpoint, and removing one cannot strand a query.
 */
export type WidgetType =
	| 'summary'
	| 'events'
	| 'attention'
	| 'uptime'
	| 'latency'
	| 'certificates'
	| 'maintenance'
	| 'activity';

/** Column span in the 6-column grid the dashboard lays widgets out on. */
export type WidgetWidth = 'third' | 'half' | 'full';

export interface Widget {
	type: WidgetType;
	width: WidgetWidth;
}

/**
 * The catalogue, in the order the "add widget" menu offers it.
 *
 * `adminOnly` marks a widget whose data the API refuses to a non-admin. It is
 * hidden from the menu and skipped at render for everyone else, rather than
 * being rendered as a permanent error — but it is NOT stripped from a stored
 * layout, so an admin who is temporarily demoted and promoted back finds their
 * dashboard as they left it.
 */
export const WIDGET_CATALOGUE: {
	type: WidgetType;
	/**
	 * i18n keys for the title and the one-line description in the add menu.
	 *
	 * Typed as MessageKey, not string: a loose type would compile a catalogue
	 * entry whose key does not exist and render the raw key as a widget heading.
	 */
	labelKey: MessageKey;
	descriptionKey: MessageKey;
	defaultWidth: WidgetWidth;
	adminOnly?: boolean;
}[] = [
	{
		type: 'summary',
		labelKey: 'widget.summary',
		descriptionKey: 'widget.summaryDesc',
		defaultWidth: 'full'
	},
	{
		type: 'events',
		labelKey: 'widget.events',
		descriptionKey: 'widget.eventsDesc',
		defaultWidth: 'full'
	},
	{
		type: 'attention',
		labelKey: 'widget.attention',
		descriptionKey: 'widget.attentionDesc',
		defaultWidth: 'half'
	},
	{
		type: 'uptime',
		labelKey: 'widget.uptime',
		descriptionKey: 'widget.uptimeDesc',
		defaultWidth: 'half'
	},
	{
		type: 'latency',
		labelKey: 'widget.latency',
		descriptionKey: 'widget.latencyDesc',
		defaultWidth: 'half'
	},
	{
		type: 'certificates',
		labelKey: 'widget.certificates',
		descriptionKey: 'widget.certificatesDesc',
		defaultWidth: 'half'
	},
	{
		type: 'maintenance',
		labelKey: 'widget.maintenance',
		descriptionKey: 'widget.maintenanceDesc',
		defaultWidth: 'half'
	},
	{
		type: 'activity',
		labelKey: 'widget.activity',
		descriptionKey: 'widget.activityDesc',
		defaultWidth: 'half',
		adminOnly: true
	}
];

const KNOWN = new Set<string>(WIDGET_CATALOGUE.map((w) => w.type));
const WIDTHS = new Set<string>(['third', 'half', 'full']);

/** Lookup for a catalogue entry; undefined for a type this build does not know. */
export function widgetSpec(type: WidgetType) {
	return WIDGET_CATALOGUE.find((w) => w.type === type);
}

/**
 * The layout a fresh install gets: Uptime Kuma's dashboard home — the quick
 * stats over the instance-wide event log. The monitor list is not a widget any
 * more; it sits permanently beside the board (see the (monitors) layout).
 */
export function defaultLayout(): Widget[] {
	return [
		{ type: 'summary', width: 'full' },
		{ type: 'events', width: 'full' }
	];
}

/**
 * Widget types renamed since a layout may have been stored, old → new.
 *
 * `monitors` was the card grid. When the list moved out of the board into the
 * monitor area's left column, the slot it held became the event log — which is
 * what Kuma shows in that place — so a stored [summary, monitors], i.e. every
 * untouched dashboard, reads as the new default instead of losing its second
 * widget.
 */
const RENAMED: Record<string, WidgetType> = { monitors: 'events' };

/**
 * Validates a stored layout, dropping anything unusable.
 *
 * This is the compatibility seam, and it runs on every read. A layout is written
 * by whatever version of the app the browser last loaded, and the same browser
 * may then load an older one — a rollback, a second instance, a cached bundle —
 * so it must survive:
 *
 *   - a widget type this build has never heard of (a newer version's), dropped
 *   - a width outside the three this build lays out, reset to the default
 *   - a duplicate type, kept once: each widget appears at most once, which is
 *     what lets the layout be keyed on type and lets "add" be idempotent
 *   - anything that is not an array of objects at all, replaced wholesale
 *
 * An EMPTY result is returned as empty, not as the default. A dashboard someone
 * deliberately emptied must stay empty — silently refilling it would make the
 * remove button look broken — and the UI offers a reset for the way back.
 */
export function sanitizeLayout(raw: unknown): Widget[] | undefined {
	if (!Array.isArray(raw)) return undefined;
	const seen = new Set<string>();
	const out: Widget[] = [];
	for (const entry of raw) {
		if (typeof entry !== 'object' || entry === null) continue;
		const { type: stored, width } = entry as { type?: unknown; width?: unknown };
		const type = typeof stored === 'string' ? (RENAMED[stored] ?? stored) : stored;
		if (typeof type !== 'string' || !KNOWN.has(type) || seen.has(type)) continue;
		seen.add(type);
		out.push({
			type: type as WidgetType,
			width:
				typeof width === 'string' && WIDTHS.has(width)
					? (width as WidgetWidth)
					: (widgetSpec(type as WidgetType)?.defaultWidth ?? 'full')
		});
	}
	return out;
}

/** Widget types not currently placed, in catalogue order. */
export function availableWidgets(layout: Widget[]): WidgetType[] {
	const placed = new Set(layout.map((w) => w.type));
	return WIDGET_CATALOGUE.filter((w) => !placed.has(w.type)).map((w) => w.type);
}

/** Appends a widget at its default width; a no-op when it is already placed. */
export function addWidget(layout: Widget[], type: WidgetType): Widget[] {
	if (layout.some((w) => w.type === type)) return layout;
	const spec = widgetSpec(type);
	if (!spec) return layout;
	return [...layout, { type, width: spec.defaultWidth }];
}

export function removeWidget(layout: Widget[], type: WidgetType): Widget[] {
	return layout.filter((w) => w.type !== type);
}

/** The next width in the cycle third → half → full → third. */
export function nextWidth(w: WidgetWidth): WidgetWidth {
	return w === 'third' ? 'half' : w === 'half' ? 'full' : 'third';
}

export function setWidth(layout: Widget[], type: WidgetType, width: WidgetWidth): Widget[] {
	return layout.map((w) => (w.type === type ? { ...w, width } : w));
}

/**
 * Reorders by putting `dragged` where `target` currently is.
 *
 * This is the ONE reordering primitive, used by both the drop handler and the
 * up/down buttons. The buttons could have been "move one slot in the stored
 * order", and that was wrong: the stored layout can hold a widget the current
 * user cannot see, and moving past it would change the saved order while
 * changing nothing on screen. Expressed as "put this one where that one is",
 * with the neighbour picked from what is rendered, both paths agree.
 *
 * Dropping a widget on itself, or either type being absent, leaves the layout
 * untouched — so an out-of-range move is a no-op rather than a wrap. A widget at
 * the top that jumps to the bottom on one more click of "up" is a control nobody
 * trusts twice.
 */
export function reorderWidget(layout: Widget[], dragged: WidgetType, target: WidgetType): Widget[] {
	if (dragged === target) return layout;
	const from = layout.findIndex((w) => w.type === dragged);
	const to = layout.findIndex((w) => w.type === target);
	if (from < 0 || to < 0) return layout;
	const next = [...layout];
	const [moved] = next.splice(from, 1);
	next.splice(to, 0, moved);
	return next;
}

/** Grid column span for a width, against the 6-column dashboard grid. */
export function columnSpan(w: WidgetWidth): number {
	return w === 'third' ? 2 : w === 'half' ? 3 : 6;
}
