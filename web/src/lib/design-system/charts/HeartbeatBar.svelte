<script lang="ts">
	// Per-check status bar: one rounded pill per heartbeat, oldest left, newest
	// right. The strip is right-aligned and clips on the LEFT, so however narrow
	// the container, the checks that remain visible are the most recent ones —
	// the ones an operator scanning the list is asking about.
	//
	// Fewer beats than `slots` are padded with empty pills at the start, so a
	// monitor created a minute ago does not render as a stubby half-bar that
	// reads as "the bar is broken".
	//
	// Colour is not the only signal: the strip carries an aria-label that
	// counts the checks per status.
	import type { MonitorStatus } from '../tokens/tokens.js';
	import { t } from '$lib/i18n';

	interface Props {
		/** Statuses, oldest first. */
		beats: MonitorStatus[];
		/** How many pills to lay out, padding with empty ones. */
		slots?: number;
		size?: 'sm' | 'md';
		class?: string;
	}

	let { beats, slots = 50, size = 'md', class: className = '' }: Props = $props();

	const shown = $derived(beats.slice(-slots));
	const padding = $derived(Math.max(0, slots - shown.length));

	const summary = $derived.by(() => {
		const counts = new Map<MonitorStatus, number>();
		for (const b of shown) counts.set(b, (counts.get(b) ?? 0) + 1);
		return [...counts].map(([s, n]) => `${n} ${t(`status.${s}`)}`).join(', ');
	});
	const label = $derived(
		shown.length === 0
			? t('heartbeatBar.empty')
			: t('heartbeatBar.label', { n: shown.length, summary })
	);
</script>

<div class="hb {size} {className}" role="img" aria-label={label}>
	{#each Array(padding) as _, i (i)}
		<span class="beat empty"></span>
	{/each}
	{#each shown as status, i (i)}
		<span class="beat" data-status={status}></span>
	{/each}
</div>

<style>
	.hb {
		display: flex;
		justify-content: flex-end;
		align-items: center;
		gap: 5px;
		min-width: 0;
		overflow: hidden;
	}
	.hb.sm {
		gap: 3px;
	}
	.beat {
		flex: 0 0 auto;
		width: 10px;
		height: 30px;
		border-radius: var(--radius-pill);
		background: var(--hairline-strong);
	}
	.sm .beat {
		width: 6px;
		height: 20px;
	}
	.beat[data-status='up'] {
		background: var(--status-up);
	}
	.beat[data-status='down'] {
		background: var(--status-down);
	}
	.beat[data-status='pending'] {
		background: var(--status-pending);
	}
	.beat[data-status='maintenance'] {
		background: var(--status-maintenance);
	}
	.beat[data-status='paused'] {
		background: var(--status-paused);
	}
</style>
