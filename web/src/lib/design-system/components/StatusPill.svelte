<script lang="ts">
	// Solid status pill: the monitor-status colour as the FILL, dark text on
	// top. StatusBadge is the tinted, icon-carrying variant for prose and
	// tables; this one is for the dense places where the colour has to read at
	// a glance — the uptime figure in a monitor-list row, the big state pill on
	// the monitor detail page.
	//
	// Every status colour is a mid-light tone, so the near-black base passes
	// 4.5:1 on all five; white would fail on up and pending.
	import type { MonitorStatus } from '../tokens/tokens.js';

	interface Props {
		status: MonitorStatus;
		/** The text inside the pill — a percentage, a status name. */
		label: string;
		size?: 'sm' | 'lg';
		class?: string;
	}

	let { status, label, size = 'sm', class: className = '' }: Props = $props();
</script>

<span class="pill {size} {className}" data-status={status}>{label}</span>

<style>
	.pill {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		flex-shrink: 0;
		box-sizing: border-box;
		border-radius: var(--radius-pill);
		color: var(--color-bg-base);
		font-weight: var(--weight-bold);
		font-variant-numeric: tabular-nums;
		white-space: nowrap;
		background: var(--status-paused);
	}
	.sm {
		min-width: 3.75rem;
		padding: 2px var(--space-3);
		font-size: var(--text-caption-1);
	}
	.lg {
		padding: var(--space-2) var(--space-7);
		font-size: var(--text-title-3);
	}
	.pill[data-status='up'] {
		background: var(--status-up);
	}
	.pill[data-status='down'] {
		background: var(--status-down);
	}
	.pill[data-status='pending'] {
		background: var(--status-pending);
	}
	.pill[data-status='maintenance'] {
		background: var(--status-maintenance);
	}
</style>
