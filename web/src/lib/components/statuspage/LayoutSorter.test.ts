/**
 * The keyboard half of the layout editor, plus the rollback announcement.
 *
 * Everything here goes through the grips as a screen-reader user would: focus a
 * grip, Space to lift, arrows to move, Space to drop. The pointer path is
 * deliberately not simulated — it is all offsetTop/offsetHeight geometry, which
 * jsdom reports as 0, so a "passing" pointer test would only prove that
 * measuring nothing reorders nothing.
 */
import { beforeEach, describe, expect, it, vi, type Mock } from 'vitest';
import { tick } from 'svelte';
import { fireEvent, render, screen } from '@testing-library/svelte';
import type { StatusPageLayoutOrder } from '$lib/api';
import LayoutSorterHarness from './LayoutSorterHarness.svelte';
import type { Container } from './layout';

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
		{ id: '11', name: 'Edge', monitors: [{ id: '4', name: 'cdn' }] }
	];
}

let commits: StatusPageLayoutOrder[];
let oncommit: Mock<(order: StatusPageLayoutOrder) => void>;
let host: { settle: (rollback?: Container[]) => void; setBusy: (v: boolean) => void };
let root: HTMLElement;

/**
 * Renders the sorter inside the harness that stands in for the real parent:
 * two-way bound containers, and a `busy` flag flipped in the same tick the
 * commit is handed over. See LayoutSorterHarness.svelte for why a plain
 * `render` + `rerender` cannot express that.
 */
function setup(initial = model(), startBusy = false) {
	const view = render(LayoutSorterHarness, { props: { initial, startBusy, oncommit } });
	host = view.component as typeof host;
	root = view.container;
	return view;
}

const grip = (name: string) => screen.getByRole('button', { name });
const live = () => root.querySelector('[aria-live="polite"]') as HTMLElement;

/** Send a key to whatever currently holds focus, then let effects settle. */
async function press(key: string) {
	await fireEvent.keyDown(document.activeElement as HTMLElement, { key });
	await tick();
}

/** The rows of every container, in display order — a readable failure message. */
function shape(): string[] {
	return [...root.querySelectorAll('ul.monitors')].map((ul) =>
		[...ul.querySelectorAll('.name')].map((n) => n.textContent).join(',')
	);
}

/** Group headings in display order. */
function groups(): string[] {
	return [...root.querySelectorAll('.head h4')].map((h) => h.textContent ?? '');
}

beforeEach(() => {
	commits = [];
	oncommit = vi.fn((order: StatusPageLayoutOrder) => {
		commits.push(order);
	});
});

describe('LayoutSorter keyboard reordering', () => {
	it('renders the model as containers of rows', () => {
		setup();
		expect(groups()).toEqual(['Ungrouped', 'Core', 'Edge']);
		expect(shape()).toEqual(['api,web', 'db', 'cdn']);
	});

	it('offers no grip for the ungrouped pseudo-group, which never moves', () => {
		setup();
		expect(
			screen.queryByRole('button', { name: 'Reorder group Ungrouped' })
		).not.toBeInTheDocument();
		expect(grip('Reorder group Core')).toBeInTheDocument();
	});

	it('lifts a row on Space and announces where it is', async () => {
		setup();
		grip('Reorder api').focus();
		await press(' ');
		expect(grip('Reorder api')).toHaveAttribute('aria-pressed', 'true');
		expect(live()).toHaveTextContent('api picked up. Position 1 of 2 in Ungrouped.');
		expect(oncommit).not.toHaveBeenCalled();
	});

	it('moves a lifted row with the arrows without committing until it is dropped', async () => {
		setup();
		grip('Reorder api').focus();
		await press(' ');
		await press('ArrowDown');
		expect(shape()[0]).toBe('web,api');
		expect(live()).toHaveTextContent('api moved to position 2 of 2 in Ungrouped.');
		expect(oncommit).not.toHaveBeenCalled();

		await press(' ');
		expect(live()).toHaveTextContent('api dropped at position 2 of 2.');
		expect(oncommit).toHaveBeenCalledOnce();
		expect(commits[0]).toEqual({
			ungrouped: ['2', '1'],
			groups: [
				{ groupId: '10', monitorIds: ['3'] },
				{ groupId: '11', monitorIds: ['4'] }
			]
		});
	});

	it('drops with Enter as well as Space', async () => {
		setup();
		grip('Reorder api').focus();
		await press('Enter');
		await press('ArrowDown');
		await press('Enter');
		expect(oncommit).toHaveBeenCalledOnce();
	});

	it('keeps focus on the grip as the row moves under it', async () => {
		setup();
		grip('Reorder api').focus();
		await press(' ');
		await press('ArrowDown');
		await tick();
		expect(document.activeElement).toBe(grip('Reorder api'));
	});

	it('steps off the end of a group into the next one', async () => {
		// The only route between containers without a pointer.
		setup();
		grip('Reorder web').focus();
		await press(' ');
		await press('ArrowDown');
		expect(shape()).toEqual(['api', 'web,db', 'cdn']);
		expect(live()).toHaveTextContent('web moved to position 1 of 2 in Core.');
	});

	it('steps off the top of a group into the end of the one above', async () => {
		setup();
		grip('Reorder db').focus();
		await press(' ');
		await press('ArrowUp');
		expect(shape()).toEqual(['api,web,db', '', 'cdn']);
		expect(live()).toHaveTextContent('db moved to position 3 of 3 in Ungrouped.');
	});

	it('renders a drop slot for a group a move emptied', async () => {
		setup();
		grip('Reorder db').focus();
		await press(' ');
		await press('ArrowUp');
		expect(screen.getByText('Empty group — drop a monitor here')).toBeInTheDocument();
	});

	it('does nothing at the very top and commits nothing', async () => {
		setup();
		grip('Reorder api').focus();
		await press(' ');
		await press('ArrowUp');
		expect(shape()).toEqual(['api,web', 'db', 'cdn']);
		await press(' ');
		// The drop is announced, but a no-op must not send a PATCH.
		expect(oncommit).not.toHaveBeenCalled();
	});

	it('does nothing at the very bottom', async () => {
		setup();
		grip('Reorder cdn').focus();
		await press(' ');
		await press('ArrowDown');
		expect(shape()).toEqual(['api,web', 'db', 'cdn']);
	});

	it('does not commit a move that ends where it started', async () => {
		setup();
		grip('Reorder api').focus();
		await press(' ');
		await press('ArrowDown');
		await press('ArrowUp');
		await press(' ');
		expect(shape()).toEqual(['api,web', 'db', 'cdn']);
		expect(oncommit).not.toHaveBeenCalled();
	});

	it('jumps a row to the start and end of its own container with Home/End', async () => {
		setup();
		grip('Reorder api').focus();
		await press(' ');
		await press('End');
		expect(shape()[0]).toBe('web,api');
		await press('Home');
		expect(shape()[0]).toBe('api,web');
	});

	it('reorders groups and reports the display index, which skips the ungrouped block', async () => {
		setup();
		grip('Reorder group Core').focus();
		await press(' ');
		expect(live()).toHaveTextContent('Group Core picked up. Position 1 of 2.');
		await press('ArrowDown');
		expect(groups()).toEqual(['Ungrouped', 'Edge', 'Core']);
		expect(live()).toHaveTextContent('Group Core moved to position 2 of 2.');
		await press(' ');
		expect(commits[0]).toEqual({
			ungrouped: ['1', '2'],
			groups: [
				{ groupId: '11', monitorIds: ['4'] },
				{ groupId: '10', monitorIds: ['3'] }
			]
		});
	});

	it('never lets a group move above the ungrouped block', async () => {
		setup();
		grip('Reorder group Core').focus();
		await press(' ');
		await press('ArrowUp');
		expect(groups()).toEqual(['Ungrouped', 'Core', 'Edge']);
		await press('Home');
		expect(groups()).toEqual(['Ungrouped', 'Core', 'Edge']);
		await press(' ');
		expect(oncommit).not.toHaveBeenCalled();
	});

	it('restores the original order on Escape and commits nothing', async () => {
		setup();
		grip('Reorder api').focus();
		await press(' ');
		await press('ArrowDown');
		expect(shape()[0]).toBe('web,api');
		await press('Escape');
		expect(shape()[0]).toBe('api,web');
		expect(live()).toHaveTextContent('Reorder cancelled. api returned to its original position.');
		expect(oncommit).not.toHaveBeenCalled();
	});

	it('cancels a grab when focus leaves the grip entirely', async () => {
		setup();
		grip('Reorder api').focus();
		await press(' ');
		await press('ArrowDown');
		(document.activeElement as HTMLElement).blur();
		// onGripBlur waits a turn before cancelling, so that a move re-focusing the
		// grip at its new slot is not mistaken for focus leaving.
		await tick();
		await tick();
		expect(shape()[0]).toBe('api,web');
		expect(oncommit).not.toHaveBeenCalled();
	});
});

describe('LayoutSorter while a commit is in flight', () => {
	it('marks the grips aria-disabled without taking their focus away', async () => {
		setup(model(), true);
		const g = grip('Reorder api');
		expect(g).toHaveAttribute('aria-disabled', 'true');
		expect(g).not.toBeDisabled();
		g.focus();
		expect(document.activeElement).toBe(g);
	});

	it('refuses to lift a row', async () => {
		setup(model(), true);
		grip('Reorder api').focus();
		await press(' ');
		expect(grip('Reorder api')).toHaveAttribute('aria-pressed', 'false');
	});

	it('still honours Escape, so a lifted row is never trapped by an in-flight commit', async () => {
		setup();
		grip('Reorder api').focus();
		await press(' ');
		await press('ArrowDown');
		await press(' '); // commits, and the parent flips busy in the same tick
		expect(oncommit).toHaveBeenCalledOnce();

		grip('Reorder web').focus();
		await press(' ');
		expect(grip('Reorder web')).toHaveAttribute('aria-pressed', 'false');
	});
});

describe('LayoutSorter rollback', () => {
	/** Lift the first row, move it, drop it — the parent flips busy on commit. */
	async function moveAndCommit() {
		grip('Reorder api').focus();
		await press(' ');
		await press('ArrowDown');
		await press(' ');
		expect(oncommit).toHaveBeenCalledOnce();
	}

	it('announces the revert when the parent restores the old order after a 409', async () => {
		setup();
		await moveAndCommit();
		// What the parent does on a 409: put the server's order back, then clear
		// busy. The sorter only learns the request settled from busy going false.
		host.settle(model());
		await tick();
		expect(live()).toHaveTextContent('The new order could not be saved and was reverted.');
		expect(shape()[0]).toBe('api,web');
	});

	it('says nothing when the commit succeeded and the order stuck', async () => {
		setup();
		await moveAndCommit();
		host.settle();
		await tick();
		expect(live()).not.toHaveTextContent('The new order could not be saved and was reverted.');
		expect(shape()[0]).toBe('web,api');
	});

	it('does not re-announce a revert on a later unrelated busy cycle', async () => {
		setup();
		await moveAndCommit();
		host.settle(model());
		await tick();
		expect(live()).toHaveTextContent('The new order could not be saved and was reverted.');

		// A second busy cycle with no commit behind it: `sent` was consumed, so
		// there is nothing to compare and nothing to say.
		host.setBusy(true);
		await tick();
		host.settle(model());
		await tick();
		grip('Reorder api').focus();
		await press(' ');
		expect(live()).toHaveTextContent('api picked up. Position 1 of 2 in Ungrouped.');
	});

	it('announces a revert after a rejected group move too', async () => {
		setup();
		grip('Reorder group Core').focus();
		await press(' ');
		await press('ArrowDown');
		await press(' ');
		expect(oncommit).toHaveBeenCalledOnce();
		host.settle(model());
		await tick();
		expect(live()).toHaveTextContent('The new order could not be saved and was reverted.');
		expect(groups()).toEqual(['Ungrouped', 'Core', 'Edge']);
	});
});
