<script lang="ts">
	// Test-only host for LayoutSorter.svelte (see LayoutSorter.test.ts). It exists
	// because the sorter's rollback signal is a timing contract with its parent:
	// `busy` must go true in the SAME tick the commit is handed over, or the
	// effect that watches for a revert clears itself before the request settles.
	// A testing-library `rerender` is async and cannot express that, and it
	// carries no `bind:` so the sorter's own writes never come back.
	//
	// This mirrors commitLayout in routes/(app)/status-pages/+page.svelte.
	import { untrack } from 'svelte';
	import LayoutSorter from './LayoutSorter.svelte';
	import type { Container } from './layout';
	import type { StatusPageLayoutOrder } from '$lib/api';

	interface Props {
		initial: Container[];
		startBusy?: boolean;
		oncommit: (order: StatusPageLayoutOrder) => void;
	}

	let { initial, startBusy = false, oncommit }: Props = $props();

	// Seeded once, then owned here — untrack() because taking the initial value on
	// purpose is exactly what the compiler warns about otherwise.
	let containers = $state(untrack(() => initial));
	let busy = $state(untrack(() => startBusy));

	/** Settle the in-flight commit; pass the order to restore for a rollback. */
	export function settle(rollback?: Container[]) {
		if (rollback) containers = rollback;
		busy = false;
	}

	/** A busy cycle with no commit behind it. */
	export function setBusy(v: boolean) {
		busy = v;
	}
</script>

<LayoutSorter
	bind:containers
	{busy}
	oncommit={(order) => {
		busy = true;
		oncommit(order);
	}}
/>
