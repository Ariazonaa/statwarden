<script lang="ts">
	// Slides in when the live WebSocket connection drops; reflects the
	// auto-reconnect/backoff state from the ws store.
	import { fly } from 'svelte/transition';
	import { ws } from '$lib/ws/store.svelte';
	import { t } from '$lib/i18n';
	import { safeDuration } from '$lib/design-system';

	const visible = $derived(ws.connection === 'reconnecting' || ws.connection === 'connecting');
	const message = $derived(
		ws.attempt > 1 ? t('connection.lostAttempt', { n: ws.attempt }) : t('connection.lost')
	);
</script>

{#if visible}
	<div class="banner" role="status" transition:fly={{ y: -20, duration: safeDuration(240) }}>
		<span class="spinner" aria-hidden="true"></span>
		<span>{message}</span>
	</div>
{/if}

<style>
	.banner {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		margin-bottom: var(--space-5);
		padding: var(--space-3) var(--space-5);
		border-radius: var(--radius-pill);
		background: var(--status-pending-tint);
		border: 1px solid color-mix(in srgb, var(--status-pending) 30%, transparent);
		color: var(--status-pending-text);
		font-size: var(--text-footnote);
		font-weight: var(--weight-medium);
	}
	.spinner {
		width: 0.85rem;
		height: 0.85rem;
		border-radius: 50%;
		border: 2px solid currentColor;
		border-top-color: transparent;
		animation: spin 0.7s linear infinite;
		flex-shrink: 0;
	}
	@keyframes spin {
		to {
			transform: rotate(360deg);
		}
	}
	@media (prefers-reduced-motion: reduce) {
		.spinner {
			animation-duration: 1.6s;
		}
	}
</style>
