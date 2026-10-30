import { describe, it, expect } from 'vitest';
import {
	addWidget,
	availableWidgets,
	columnSpan,
	defaultLayout,
	nextWidth,
	removeWidget,
	reorderWidget,
	sanitizeLayout,
	setWidth,
	widgetSpec,
	WIDGET_CATALOGUE,
	type Widget
} from './layout';

const L = (...types: string[]): Widget[] =>
	types.map((t) => ({ type: t as Widget['type'], width: 'half' }));

describe('defaultLayout', () => {
	// Kuma's dashboard home: quick stats over the event log. The monitor list
	// is no longer a widget; it sits beside the board.
	it('is the summary over the event log, both full width', () => {
		expect(defaultLayout()).toEqual([
			{ type: 'summary', width: 'full' },
			{ type: 'events', width: 'full' }
		]);
	});

	it('only names widgets the catalogue carries', () => {
		for (const w of defaultLayout()) expect(widgetSpec(w.type)).toBeDefined();
	});
});

describe('sanitizeLayout', () => {
	// The card grid left the board; its slot became the event log. An untouched
	// stored dashboard — [summary, monitors] — must read as the new default
	// rather than silently losing its second widget.
	it('reads the retired monitors widget as the event log, keeping its width', () => {
		expect(
			sanitizeLayout([
				{ type: 'summary', width: 'full' },
				{ type: 'monitors', width: 'half' }
			])
		).toEqual([
			{ type: 'summary', width: 'full' },
			{ type: 'events', width: 'half' }
		]);
	});

	it('keeps one event log when both the old and the new name are stored', () => {
		expect(sanitizeLayout(L('events', 'monitors'))?.map((w) => w.type)).toEqual(['events']);
	});

	it('accepts a well-formed layout unchanged', () => {
		const layout: Widget[] = [
			{ type: 'summary', width: 'full' },
			{ type: 'latency', width: 'third' }
		];
		expect(sanitizeLayout(layout)).toEqual(layout);
	});

	/**
	 * The compatibility case this function exists for: the same browser can load a
	 * newer build, arrange a widget that build invented, then load an older one —
	 * a rollback, a second instance, a cached bundle.
	 */
	it('drops a widget type this build does not know', () => {
		expect(sanitizeLayout([{ type: 'from-the-future', width: 'full' }])).toEqual([]);
	});

	it('resets an unknown width to the widget default rather than dropping the widget', () => {
		expect(sanitizeLayout([{ type: 'latency', width: 'gigantic' }])).toEqual([
			{ type: 'latency', width: widgetSpec('latency')!.defaultWidth }
		]);
	});

	it('keeps only the first of a duplicated type', () => {
		const out = sanitizeLayout([
			{ type: 'summary', width: 'full' },
			{ type: 'summary', width: 'third' }
		]);
		expect(out).toEqual([{ type: 'summary', width: 'full' }]);
	});

	it('rejects anything that is not an array', () => {
		for (const bad of [undefined, null, 'summary', 42, { type: 'summary' }]) {
			expect(sanitizeLayout(bad)).toBeUndefined();
		}
	});

	it('skips entries that are not objects', () => {
		expect(sanitizeLayout(['summary', null, 7, { type: 'summary', width: 'full' }])).toEqual([
			{ type: 'summary', width: 'full' }
		]);
	});

	/**
	 * An emptied board stays empty. Refilling it with the default would make the
	 * remove button look broken on the last widget; the UI offers a reset instead.
	 */
	it('returns an empty layout as empty, not as the default', () => {
		expect(sanitizeLayout([])).toEqual([]);
	});
});

describe('mutations', () => {
	it('adds a widget at its catalogue default width', () => {
		const out = addWidget(L('summary'), 'latency');
		expect(out).toHaveLength(2);
		expect(out[1]).toEqual({ type: 'latency', width: widgetSpec('latency')!.defaultWidth });
	});

	it('is idempotent: adding a placed widget changes nothing', () => {
		const layout = L('summary', 'latency');
		expect(addWidget(layout, 'latency')).toBe(layout);
	});

	it('removes by type', () => {
		expect(removeWidget(L('summary', 'latency'), 'summary')).toEqual(L('latency'));
	});

	it('removing an absent widget is a no-op', () => {
		expect(removeWidget(L('summary'), 'latency')).toEqual(L('summary'));
	});

	it('reorders by dropping one widget onto another', () => {
		const out = reorderWidget(L('summary', 'latency', 'uptime'), 'uptime', 'summary');
		expect(out.map((w) => w.type)).toEqual(['uptime', 'summary', 'latency']);
	});

	// The up/down buttons are this same call against the neighbour on screen, so
	// a one-slot swap in either direction has to come out exactly right.
	it('swaps neighbours in both directions', () => {
		const layout = L('summary', 'events', 'latency');
		expect(reorderWidget(layout, 'summary', 'events').map((w) => w.type)) //
			.toEqual(['events', 'summary', 'latency']);
		expect(reorderWidget(layout, 'latency', 'events').map((w) => w.type)) //
			.toEqual(['summary', 'latency', 'events']);
	});

	/**
	 * Moving past a widget the viewer cannot see is what the buttons must NOT do:
	 * the dashboard picks the neighbour from what is rendered and reorders against
	 * that, so a hidden widget in between is carried along rather than swapped
	 * with. This pins the primitive that behaviour rests on.
	 */
	it('moves across a gap in one step', () => {
		const layout = L('summary', 'activity', 'events');
		expect(reorderWidget(layout, 'events', 'summary').map((w) => w.type)) //
			.toEqual(['events', 'summary', 'activity']);
	});

	it('ignores a drop onto itself or onto an absent widget', () => {
		const layout = L('summary', 'latency');
		expect(reorderWidget(layout, 'summary', 'summary')).toBe(layout);
		expect(reorderWidget(layout, 'summary', 'maintenance')).toBe(layout);
	});

	it('cycles width third → half → full → third', () => {
		expect(nextWidth('third')).toBe('half');
		expect(nextWidth('half')).toBe('full');
		expect(nextWidth('full')).toBe('third');
	});

	it('sets width on one widget only', () => {
		const out = setWidth(L('summary', 'latency'), 'latency', 'full');
		expect(out.map((w) => w.width)).toEqual(['half', 'full']);
	});
});

describe('availableWidgets', () => {
	it('lists what is not placed, in catalogue order', () => {
		const placed = L('summary', 'events');
		const out = availableWidgets(placed);
		expect(out).not.toContain('summary');
		expect(out).toEqual(
			WIDGET_CATALOGUE.filter((w) => w.type !== 'summary' && w.type !== 'events').map((w) => w.type)
		);
	});

	it('is empty once every widget is on the board', () => {
		expect(
			availableWidgets(WIDGET_CATALOGUE.map((w) => ({ ...w, width: 'half' as const })))
		).toEqual([]);
	});
});

describe('columnSpan', () => {
	// The grid is six columns wide so a third, a half and a full width all line
	// up against each other.
	it('divides the six-column grid evenly', () => {
		expect(columnSpan('third')).toBe(2);
		expect(columnSpan('half')).toBe(3);
		expect(columnSpan('full')).toBe(6);
		expect(6 % columnSpan('third')).toBe(0);
		expect(6 % columnSpan('half')).toBe(0);
	});
});
