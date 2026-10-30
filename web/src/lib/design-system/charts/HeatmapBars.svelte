<script lang="ts">
	// Status-page style heatmap bars: one rounded tick per interval,
	// coloured by monitor status, with a glass hover/focus tooltip.
	// Status is never colour-alone — the tooltip names it and an
	// aria-label carries it for assistive tech.
	import type { MonitorStatus } from '../tokens/tokens.js';
	import { t } from '$lib/i18n';

	export interface HeatmapBar {
		/** e.g. "Jul 18" */
		label: string;
		status: MonitorStatus;
		/**
		 * Names the status in the tooltip and the aria-label, in place of the
		 * `status.*` translation of `status`.
		 *
		 * `status` picks the COLOUR from the monitor-status palette, and a caller
		 * whose own vocabulary is wider has to collapse into that palette — the
		 * public status page maps a partially-degraded bucket onto `pending` and a
		 * bucket with no data onto `paused`. That is right for the colour and wrong
		 * for the words: the monitor was never paused, and `pending` means
		 * "retrying" here. Since the tooltip and aria-label are the whole reason
		 * status is not conveyed by colour alone, they have to be able to say
		 * something the palette cannot.
		 */
		statusLabel?: string;
		/** e.g. uptime percentage for the interval. */
		detail?: string;
	}

	interface Props {
		bars: HeatmapBar[];
		/** Accessible name for the whole strip, e.g. "Uptime, last 90 days". */
		label: string;
		height?: number;
		/** Gap between adjacent bars, in px. Tighten it for dense (hourly) ranges. */
		gap?: number;
		/** Minimum width of a single bar, in px, so every bucket stays hoverable. */
		minBarWidth?: number;
		class?: string;
	}

	let {
		bars,
		label,
		height = 32,
		gap = 2,
		minBarWidth = 3,
		class: className = ''
	}: Props = $props();

	let hovered = $state(-1);

	function statusName(bar: HeatmapBar): string {
		return bar.statusLabel ?? t(`status.${bar.status}`);
	}

	function barLabel(bar: HeatmapBar): string {
		return `${bar.label}: ${statusName(bar)}${bar.detail ? `, ${bar.detail}` : ''}`;
	}
</script>

<div
	class="strip {className}"
	role="img"
	aria-label={label}
	style="gap: {gap}px; --hm-min: {minBarWidth}px;"
>
	{#each bars as bar, i (i)}
		<button
			class="bar"
			data-status={bar.status}
			style:height="{height}px"
			aria-label={barLabel(bar)}
			class:dim={hovered !== -1 && hovered !== i}
			onpointerenter={() => (hovered = i)}
			onpointerleave={() => (hovered = -1)}
			onfocus={() => (hovered = i)}
			onblur={() => (hovered = -1)}
		>
			{#if hovered === i}
				<span class="tip ds-glass ds-glass--thick" role="tooltip">
					<span class="tip-label">{bar.label}</span>
					<span class="tip-status" data-status={bar.status}>
						<span class="tip-dot" aria-hidden="true"></span>
						{statusName(bar)}{bar.detail ? ` · ${bar.detail}` : ''}
					</span>
				</span>
			{/if}
		</button>
	{/each}
</div>

<style>
	.strip {
		display: flex;
		align-items: stretch;
		gap: 2px; /* default surface gap between adjacent fills; overridable via style */
		width: 100%;
	}

	.bar {
		position: relative;
		flex: 1 1 0;
		min-width: var(--hm-min, 3px);
		appearance: none;
		border: 0;
		padding: 0;
		border-radius: 2px;
		cursor: pointer;
		transition:
			transform var(--duration-sm) var(--ease-spring),
			opacity var(--duration-sm) ease;
	}
	.bar:hover,
	.bar:focus-visible {
		transform: scaleY(1.15);
	}
	.bar.dim {
		opacity: 0.55;
	}
	.bar:focus-visible {
		outline: none;
		box-shadow: var(--focus-ring);
	}

	.bar[data-status='up'] {
		background: var(--status-up);
	}
	.bar[data-status='down'] {
		background: var(--status-down);
	}
	.bar[data-status='pending'] {
		background: var(--status-pending);
	}
	.bar[data-status='maintenance'] {
		background: var(--status-maintenance);
	}
	.bar[data-status='paused'] {
		background: var(--status-paused);
	}

	.tip {
		position: absolute;
		bottom: calc(100% + var(--space-3));
		left: 50%;
		translate: -50% 0;
		display: flex;
		flex-direction: column;
		align-items: flex-start;
		gap: 1px;
		padding: var(--space-2) var(--space-4);
		border-radius: var(--radius-squircle-sm);
		box-shadow: var(--elevation-glass-2);
		pointer-events: none;
		white-space: nowrap;
		z-index: 5;
	}
	.tip-label {
		font-size: var(--text-caption-1);
		color: var(--color-text-secondary);
	}
	.tip-status {
		display: inline-flex;
		align-items: center;
		gap: var(--space-2);
		font-size: var(--text-footnote);
		font-weight: var(--weight-semibold);
		color: var(--color-text-primary);
	}
	.tip-dot {
		width: 0.375rem;
		height: 0.375rem;
		border-radius: 50%;
	}
	.tip-status[data-status='up'] .tip-dot {
		background: var(--status-up);
	}
	.tip-status[data-status='down'] .tip-dot {
		background: var(--status-down);
	}
	.tip-status[data-status='pending'] .tip-dot {
		background: var(--status-pending);
	}
	.tip-status[data-status='maintenance'] .tip-dot {
		background: var(--status-maintenance);
	}
	.tip-status[data-status='paused'] .tip-dot {
		background: var(--status-paused);
	}

	@media (prefers-reduced-motion: reduce) {
		.bar,
		.bar:hover,
		.bar:focus-visible {
			transition: none;
			transform: none;
		}
	}
</style>
