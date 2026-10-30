<script lang="ts">
	// Tinted status pill for every monitor state, with icon and
	// (for "down") an attention pulse.
	import type { MonitorStatus } from '../tokens/tokens.js';
	import { t } from '$lib/i18n';

	interface Props {
		status: MonitorStatus;
		/** Override the default label (defaults to the localized status name). */
		label?: string;
		size?: 'sm' | 'md';
		/** Pulse animation on the dot (defaults to true for "down"). */
		pulse?: boolean;
		class?: string;
	}

	let { status, label, size = 'md', pulse, class: className = '' }: Props = $props();

	const text = $derived(label ?? t(`status.${status}`));
	const showPulse = $derived(pulse ?? status === 'down');
</script>

<span class="badge {size} {className}" data-status={status}>
	<span class="dot" class:pulse={showPulse} aria-hidden="true"></span>
	<svg class="icon" viewBox="0 0 16 16" aria-hidden="true">
		{#if status === 'up'}
			<path
				d="M3.5 8.5 6.5 11.5 12.5 5"
				fill="none"
				stroke="currentColor"
				stroke-width="2"
				stroke-linecap="round"
				stroke-linejoin="round"
			/>
		{:else if status === 'down'}
			<path
				d="M5 5 11 11 M11 5 5 11"
				stroke="currentColor"
				stroke-width="2"
				stroke-linecap="round"
			/>
		{:else if status === 'pending'}
			<circle cx="8" cy="8" r="5.5" fill="none" stroke="currentColor" stroke-width="1.8" />
			<path
				d="M8 5.5V8l2 1.5"
				fill="none"
				stroke="currentColor"
				stroke-width="1.6"
				stroke-linecap="round"
			/>
		{:else if status === 'maintenance'}
			<path
				d="M9.7 2.8a3.4 3.4 0 0 0-4.4 4.2L2.6 9.7a1.5 1.5 0 0 0 2.1 2.1l2.7-2.7a3.4 3.4 0 0 0 4.2-4.4L9.8 6.5 8.1 6.2 7.8 4.5Z"
				fill="none"
				stroke="currentColor"
				stroke-width="1.5"
				stroke-linejoin="round"
			/>
		{:else}
			<path d="M6 4.5v7 M10 4.5v7" stroke="currentColor" stroke-width="2" stroke-linecap="round" />
		{/if}
	</svg>
	{text}
</span>

<style>
	.badge {
		display: inline-flex;
		align-items: center;
		gap: var(--space-2);
		border-radius: var(--radius-pill);
		font-weight: var(--weight-semibold);
		letter-spacing: var(--tracking-normal);
		border: 1px solid transparent;
		white-space: nowrap;
	}
	.md {
		font-size: var(--text-footnote);
		padding: var(--space-2) var(--space-4);
	}
	.sm {
		font-size: var(--text-caption-1);
		padding: var(--space-1) var(--space-3);
	}

	.badge[data-status='up'] {
		color: var(--status-up-text);
		background: var(--status-up-tint);
		border-color: color-mix(in srgb, var(--status-up) 30%, transparent);
	}
	.badge[data-status='down'] {
		color: var(--status-down-text);
		background: var(--status-down-tint);
		border-color: color-mix(in srgb, var(--status-down) 30%, transparent);
	}
	.badge[data-status='pending'] {
		color: var(--status-pending-text);
		background: var(--status-pending-tint);
		border-color: color-mix(in srgb, var(--status-pending) 30%, transparent);
	}
	.badge[data-status='maintenance'] {
		color: var(--status-maintenance-text);
		background: var(--status-maintenance-tint);
		border-color: color-mix(in srgb, var(--status-maintenance) 30%, transparent);
	}
	.badge[data-status='paused'] {
		color: var(--status-paused-text);
		background: var(--status-paused-tint);
		border-color: color-mix(in srgb, var(--status-paused) 30%, transparent);
	}

	.dot {
		width: 0.4375rem;
		height: 0.4375rem;
		border-radius: 50%;
		background: currentColor;
		flex-shrink: 0;
	}
	.dot.pulse {
		animation: pulse 1.6s var(--ease-in-out) infinite;
	}
	@keyframes pulse {
		0%,
		100% {
			box-shadow: 0 0 0 0 color-mix(in srgb, currentColor 45%, transparent);
		}
		50% {
			box-shadow: 0 0 0 5px transparent;
		}
	}

	.icon {
		width: 0.8125rem;
		height: 0.8125rem;
		flex-shrink: 0;
	}

	@media (prefers-reduced-motion: reduce) {
		.dot.pulse {
			animation: none;
		}
	}
</style>
