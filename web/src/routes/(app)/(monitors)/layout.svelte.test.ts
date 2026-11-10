/**
 * The monitor area keeps its layout — and therefore the page component in the
 * right pane — alive across navigation. SvelteKit reuses a page component when
 * only its route parameter changes, so going from /monitors/A to /monitors/B
 * ran no `onMount` on the detail page: it went on showing A (name, chart,
 * events, live subscription) while its `id` — and with it Delete, Pause and
 * Check now — already pointed at B. Deleting "the monitor on screen" deleted a
 * different one.
 *
 * The layout owns the fix, so this asserts on the layout: a change of the
 * monitor id must give the pane a fresh instance of whatever it renders.
 */
import { describe, it, expect, vi } from 'vitest';
import { render } from '@testing-library/svelte';
import { createRawSnippet, tick } from 'svelte';

const route = $state({ id: 'a' as string | undefined, pathname: '/monitors/a' });

vi.mock('$app/state', () => ({
	page: {
		get params() {
			return route.id === undefined ? {} : { id: route.id };
		},
		get url() {
			return new URL(`http://localhost${route.pathname}`);
		}
	}
}));
vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
// The list is the left column and has nothing to do with the pane; the real one
// starts the monitors store and a WebSocket.
vi.mock('$lib/components/monitorlist/MonitorList.svelte', () => ({ default: () => {} }));

import Layout from './+layout.svelte';

function pane(container: HTMLElement): HTMLElement {
	return container.querySelector('[data-probe]') as HTMLElement;
}

describe('monitor area layout', () => {
	const children = createRawSnippet(() => ({ render: () => '<div data-probe></div>' }));

	it('re-creates the pane when the monitor id changes', async () => {
		route.id = 'a';
		route.pathname = '/monitors/a';
		const { container } = render(Layout, { props: { children } });
		const first = pane(container);
		expect(first).not.toBeNull();

		route.id = 'b';
		route.pathname = '/monitors/b';
		await tick();

		const second = pane(container);
		expect(second).not.toBeNull();
		expect(second).not.toBe(first);
	});

	it('keeps the pane when the id stays the same', async () => {
		route.id = 'a';
		route.pathname = '/monitors/a';
		const { container } = render(Layout, { props: { children } });
		const first = pane(container);

		// Same monitor, different sub-route: nothing about its identity changed.
		route.pathname = '/monitors/a/edit';
		await tick();

		expect(pane(container)).toBe(first);
	});
});
