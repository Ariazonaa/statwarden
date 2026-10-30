<script lang="ts">
	// The monitor area — dashboard, monitor detail, new and edit — laid out as
	// Uptime Kuma lays out its own: the monitor list stays on the left while the
	// right pane changes, so moving between monitors is one click instead of a
	// round trip through the dashboard.
	//
	// The layout outlives navigation between these routes, so the list keeps its
	// scroll position, filters and selection while the detail beside it changes.
	//
	// On a phone there is no room for both. The list then becomes a page of its
	// own at /list (reached from BottomNav), and every other route here shows
	// only its pane — Kuma's mobile layout too.
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { session } from '$lib/stores/session.svelte';
	import { t } from '$lib/i18n';
	import { PillButton } from '$lib/design-system';
	import MonitorList from '$lib/components/monitorlist/MonitorList.svelte';

	let { children } = $props();

	const onList = $derived(page.url.pathname === '/list');
</script>

<div class="area" class:onlist={onList}>
	<aside class="side">
		{#if session.canEdit}
			<div class="add">
				<PillButton onclick={() => goto('/monitors/new')}>
					<svg viewBox="0 0 24 24" aria-hidden="true" class="plus"
						><path
							d="M12 5v14M5 12h14"
							fill="none"
							stroke="currentColor"
							stroke-width="2.4"
							stroke-linecap="round"
						/></svg
					>
					{t('monitorList.add')}
				</PillButton>
			</div>
		{/if}
		<MonitorList />
	</aside>
	<div class="pane">
		{@render children()}
	</div>
</div>

<style>
	/* The side column is a fixed share of the row (Kuma's col-xl-4), never so
	   narrow that the bar shrinks to a handful of beats. */
	.area {
		display: grid;
		grid-template-columns: minmax(22rem, 32rem) minmax(0, 1fr);
		gap: var(--space-7);
		align-items: start;
	}
	.side {
		position: sticky;
		top: calc(4.5rem + var(--space-5));
		display: flex;
		flex-direction: column;
		gap: var(--space-5);
		max-height: calc(100dvh - 4.5rem - 2 * var(--space-6));
		min-height: 0;
	}
	.side > :global(.ml) {
		flex: 1 1 auto;
	}
	.add {
		flex-shrink: 0;
	}
	.plus {
		width: 1rem;
		height: 1rem;
	}
	.pane {
		min-width: 0;
		display: flex;
		flex-direction: column;
		gap: var(--space-6);
	}

	@media (max-width: 1279px) {
		.area {
			grid-template-columns: minmax(19rem, 26rem) minmax(0, 1fr);
			gap: var(--space-6);
		}
	}
	@media (max-width: 767px) {
		.area {
			grid-template-columns: minmax(0, 1fr);
		}
		.side {
			position: static;
			max-height: none;
			display: none;
		}
		.onlist .side {
			display: flex;
		}
		/* BottomNav carries "Add" on a phone. */
		.add {
			display: none;
		}
		.onlist .pane {
			display: none;
		}
	}
</style>
