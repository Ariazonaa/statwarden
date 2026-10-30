import { describe, expect, it } from 'vitest';
import { moveGroup, moveMonitor, toOrder, type Container } from './layout';

/** A fresh editor model: ungrouped + two real groups. */
function model(): Container[] {
	return [
		{
			id: null,
			name: 'Ungrouped',
			monitors: [
				{ id: '1', name: 'api' },
				{ id: '2', name: 'web' }
			]
		},
		{ id: '10', name: 'Core', monitors: [{ id: '3', name: 'db' }] },
		{
			id: '11',
			name: 'Edge',
			monitors: [
				{ id: '4', name: 'cdn' },
				{ id: '5', name: 'dns' }
			]
		}
	];
}

/** Compact view of a model, so a failure prints the order rather than objects. */
const shape = (cs: Container[]) =>
	cs.map((c) => `${c.id}[${c.monitors.map((m) => m.id).join(',')}]`);

describe('toOrder', () => {
	it('splits containers[0] out as ungrouped and keeps group display order', () => {
		expect(toOrder(model())).toEqual({
			ungrouped: ['1', '2'],
			groups: [
				{ groupId: '10', monitorIds: ['3'] },
				{ groupId: '11', monitorIds: ['4', '5'] }
			]
		});
	});

	it('emits empty lists for an empty model rather than throwing', () => {
		expect(toOrder([])).toEqual({ ungrouped: [], groups: [] });
	});

	it('keeps a group with no monitors in the payload', () => {
		const cs: Container[] = [
			{ id: null, name: 'Ungrouped', monitors: [] },
			{ id: '10', name: 'Core', monitors: [] }
		];
		expect(toOrder(cs)).toEqual({ ungrouped: [], groups: [{ groupId: '10', monitorIds: [] }] });
	});
});

describe('moveGroup', () => {
	it('moves a group down and shifts the displaced one up', () => {
		expect(shape(moveGroup(model(), 1, 2))).toEqual(['null[1,2]', '11[4,5]', '10[3]']);
	});

	it('moves a group up', () => {
		expect(shape(moveGroup(model(), 2, 1))).toEqual(['null[1,2]', '11[4,5]', '10[3]']);
	});

	it('does not mutate its input', () => {
		const cs = model();
		const before = shape(cs);
		moveGroup(cs, 1, 2);
		expect(shape(cs)).toEqual(before);
	});

	it('returns the same array identity on a no-op, so callers can test by ===', () => {
		const cs = model();
		expect(moveGroup(cs, 1, 1)).toBe(cs);
	});

	it('never moves the ungrouped pseudo-group out of slot 0', () => {
		// from=0 clamps up to 1; the ungrouped block stays put either way.
		expect(shape(moveGroup(model(), 0, 2))[0]).toBe('null[1,2]');
	});

	it('never lets another group take slot 0', () => {
		expect(shape(moveGroup(model(), 2, 0))).toEqual(['null[1,2]', '11[4,5]', '10[3]']);
	});

	it('clamps an out-of-range target to the last slot instead of splicing past the end', () => {
		expect(shape(moveGroup(model(), 1, 99))).toEqual(['null[1,2]', '11[4,5]', '10[3]']);
	});

	it('is a no-op with fewer than two real groups', () => {
		const cs: Container[] = [
			{ id: null, name: 'Ungrouped', monitors: [] },
			{ id: '10', name: 'Core', monitors: [] }
		];
		expect(moveGroup(cs, 1, 1)).toBe(cs);
		expect(moveGroup(cs, 0, 1)).toBe(cs);
		expect(moveGroup([], 0, 0)).toEqual([]);
	});
});

describe('moveMonitor', () => {
	it('reorders within one container', () => {
		expect(shape(moveMonitor(model(), 0, 0, 0, 1))).toEqual(['null[2,1]', '10[3]', '11[4,5]']);
	});

	it('indexes the target list after the removal on a same-container move', () => {
		// Moving 4 to index 1 of its own group must land it after 5, not back where
		// it started — the removal shifts 5 up first.
		expect(shape(moveMonitor(model(), 2, 0, 2, 1))).toEqual(['null[1,2]', '10[3]', '11[5,4]']);
	});

	it('moves a monitor across containers at the requested index', () => {
		expect(shape(moveMonitor(model(), 0, 1, 2, 0))).toEqual(['null[1]', '10[3]', '11[2,4,5]']);
	});

	it('appends when the target index is past the end of the target list', () => {
		expect(shape(moveMonitor(model(), 0, 0, 1, 99))).toEqual(['null[2]', '10[3,1]', '11[4,5]']);
	});

	it('moves into an empty container', () => {
		const cs: Container[] = [
			{ id: null, name: 'Ungrouped', monitors: [{ id: '1', name: 'api' }] },
			{ id: '10', name: 'Core', monitors: [] }
		];
		expect(shape(moveMonitor(cs, 0, 0, 1, 0))).toEqual(['null[]', '10[1]']);
	});

	it('does not mutate its input', () => {
		const cs = model();
		const before = shape(cs);
		moveMonitor(cs, 0, 1, 2, 0);
		expect(shape(cs)).toEqual(before);
	});

	it('returns the same array identity when the monitor lands where it already was', () => {
		const cs = model();
		expect(moveMonitor(cs, 0, 0, 0, 0)).toBe(cs);
	});

	it('is a no-op when the source container is empty', () => {
		const cs: Container[] = [
			{ id: null, name: 'Ungrouped', monitors: [] },
			{ id: '10', name: 'Core', monitors: [{ id: '1', name: 'api' }] }
		];
		expect(moveMonitor(cs, 0, 0, 1, 0)).toBe(cs);
	});

	it('is a no-op on an empty model', () => {
		expect(moveMonitor([], 0, 0, 0, 0)).toEqual([]);
	});

	it('clamps out-of-range container indices rather than splicing undefined', () => {
		// fromC/toC clamp into [0, length - 1]: this is the last container moving
		// its first monitor to its own end, i.e. a no-op that must not throw.
		expect(shape(moveMonitor(model(), 99, 0, 99, 0))).toEqual(['null[1,2]', '10[3]', '11[4,5]']);
	});

	it('clamps a negative source index to the first monitor', () => {
		expect(shape(moveMonitor(model(), 2, -5, 0, 0))).toEqual(['null[4,1,2]', '10[3]', '11[5]']);
	});
});

describe('move primitives feed toOrder', () => {
	it('round-trips a cross-container move into the wire payload', () => {
		const moved = moveMonitor(model(), 1, 0, 0, 0);
		expect(toOrder(moved)).toEqual({
			ungrouped: ['3', '1', '2'],
			groups: [
				{ groupId: '10', monitorIds: [] },
				{ groupId: '11', monitorIds: ['4', '5'] }
			]
		});
	});
});
