<script lang="ts">
	// One row of the persistent monitor list, laid out like Uptime Kuma's:
	// the 24-hour uptime as a solid status pill, the name and its tags on the
	// left, the per-check bar on the right. The whole row is a link (a
	// stretched <a>); the favourite star and the selection checkbox sit above
	// it and take their own clicks.
	import { page } from '$app/state';
	import { prefs } from '$lib/stores/prefs.svelte';
	import { t, formatNumber } from '$lib/i18n';
	import { Checkbox, HeartbeatBar, StatusPill } from '$lib/design-system';
	import type { Monitor, Tag } from '$lib/api';

	let {
		monitor,
		tags = [],
		indent = false,
		selectable = false,
		selected = false,
		draggable = false,
		onselect
	}: {
		monitor: Monitor;
		tags?: Tag[];
		/** Inside a folder: indented under its header, as Kuma nests group children. */
		indent?: boolean;
		selectable?: boolean;
		selected?: boolean;
		draggable?: boolean;
		onselect?: (checked: boolean) => void;
	} = $props();

	const monitorTags = $derived(tags.filter((tg) => monitor.tagIds.includes(tg.id)));
	const current = $derived(page.params.id === monitor.id);
	const compact = $derived(prefs.density === 'compact');
	const uptime = $derived(
		monitor.uptime24h === undefined
			? '–'
			: `${formatNumber(monitor.uptime24h, { maximumFractionDigits: monitor.uptime24h === 100 ? 0 : 2 })} %`
	);

	function toggleFav(e: MouseEvent) {
		e.preventDefault();
		e.stopPropagation();
		prefs.toggleFavorite(monitor.id);
	}
</script>

<div class="mrow" class:current class:indent class:compact class:paused={!monitor.active}>
	<a
		class="stretch ds-focusable"
		href="/monitors/{monitor.id}"
		aria-current={current ? 'page' : undefined}
		aria-label={t('monitorCard.open', { name: monitor.name })}
	></a>
	{#if selectable}
		<span class="sel">
			<Checkbox
				checked={selected}
				label={t('dashboard.selectMonitor', { name: monitor.name })}
				labelHidden
				onchange={(c) => onselect?.(c)}
			/>
		</span>
	{/if}
	{#if draggable}
		<span class="grip" aria-hidden="true" title={t('monitorCard.dragToReorder')}>
			<svg viewBox="0 0 16 16"
				><circle cx="5" cy="4" r="1.2" /><circle cx="11" cy="4" r="1.2" /><circle
					cx="5"
					cy="8"
					r="1.2"
				/><circle cx="11" cy="8" r="1.2" /><circle cx="5" cy="12" r="1.2" /><circle
					cx="11"
					cy="12"
					r="1.2"
				/></svg
			>
		</span>
	{/if}

	<div class="info">
		<div class="line">
			<StatusPill status={monitor.status} label={uptime} />
			<span class="name">{monitor.name}</span>
			<button
				class="fav ds-focusable"
				class:on={prefs.isFavorite(monitor.id)}
				aria-label={prefs.isFavorite(monitor.id) ? t('monitorCard.unpin') : t('monitorCard.pin')}
				aria-pressed={prefs.isFavorite(monitor.id)}
				onclick={toggleFav}
			>
				<svg viewBox="0 0 24 24" aria-hidden="true"
					><path
						d="m12 3 2.6 5.3 5.9.9-4.2 4.1 1 5.8L12 16.9 6.7 19.9l1-5.8L3.5 9.9l5.9-.9L12 3Z"
						fill={prefs.isFavorite(monitor.id) ? 'currentColor' : 'none'}
						stroke="currentColor"
						stroke-width="1.6"
						stroke-linejoin="round"
					/></svg
				>
			</button>
		</div>
		{#if monitorTags.length > 0 && !compact}
			<div class="tags">
				{#each monitorTags as tag (tag.id)}
					<span class="tag" style="--tag: {tag.color}">{tag.name}</span>
				{/each}
			</div>
		{/if}
	</div>

	<HeartbeatBar beats={monitor.recentBeats} slots={30} size="sm" class="bar" />
</div>

<style>
	.mrow {
		position: relative;
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: var(--space-3) var(--space-4);
		min-height: 3.5rem;
		padding: var(--space-4) var(--space-5);
		border-bottom: 1px solid var(--hairline);
		transition: background-color var(--duration-sm) ease;
	}
	.mrow:hover {
		background: var(--glass-bg-regular);
	}
	.mrow.current {
		background: var(--glass-bg-thick);
		box-shadow: inset 3px 0 0 var(--color-accent);
	}
	.mrow.indent {
		padding-left: calc(var(--space-5) + var(--space-7));
	}
	.mrow.compact {
		min-height: 2.75rem;
		padding-top: var(--space-3);
		padding-bottom: var(--space-3);
	}
	.mrow.paused .name {
		color: var(--color-text-secondary);
	}

	.stretch {
		position: absolute;
		inset: 0;
		z-index: 0;
		border-radius: 0;
	}
	.sel,
	.grip,
	.fav {
		position: relative;
		z-index: 1;
	}
	.grip {
		display: inline-flex;
		color: var(--color-text-secondary);
		cursor: grab;
	}
	.grip svg {
		width: 1rem;
		height: 1rem;
		fill: currentColor;
	}

	.info {
		flex: 1 1 10rem;
		min-width: 0;
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		pointer-events: none;
	}
	.line {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		min-width: 0;
	}
	.name {
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		font-size: var(--text-subheadline);
		font-weight: var(--weight-medium);
		color: var(--color-text-primary);
	}
	.fav {
		pointer-events: auto;
		display: inline-grid;
		place-items: center;
		width: 1.75rem;
		height: 1.75rem;
		margin-left: auto;
		flex-shrink: 0;
		padding: 0;
		border: 0;
		border-radius: var(--radius-pill);
		background: transparent;
		color: var(--color-text-secondary);
		cursor: pointer;
		opacity: 0;
		transition: opacity var(--duration-sm) ease;
	}
	.fav svg {
		width: 1rem;
		height: 1rem;
	}
	.fav.on {
		opacity: 1;
		color: var(--status-pending);
	}
	.mrow:hover .fav,
	.fav:focus-visible {
		opacity: 1;
	}
	/* Touch screens have no hover to reveal it. */
	@media (hover: none) {
		.fav {
			opacity: 1;
		}
	}
	.tags {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-2);
	}
	.tag {
		font-size: var(--text-caption-2);
		font-weight: var(--weight-semibold);
		padding: 1px var(--space-3);
		border-radius: var(--radius-pill);
		color: var(--tag);
		background: color-mix(in srgb, var(--tag) 16%, transparent);
	}

	.mrow :global(.bar) {
		flex: 0 1 50%;
		min-width: 7.5rem;
		pointer-events: none;
	}
	/* On a phone the bar wraps under the name; give it the whole line. */
	@media (max-width: 767px) {
		.mrow :global(.bar) {
			flex-basis: 100%;
		}
	}
</style>
