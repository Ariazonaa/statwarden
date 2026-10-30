<script lang="ts">
	// Renders the toast stack (bottom-centre) with springy fly-in and
	// FLIP re-stacking. Mount once in the app shell.
	import { fly } from 'svelte/transition';
	import { backOut } from 'svelte/easing';
	import { toast } from './toast.svelte.js';
	import { glassFlip, safeDuration } from '../motion/motion.svelte.js';
</script>

<div class="host" aria-live="polite" aria-atomic="false">
	{#each toast.items as item (item.id)}
		<div
			class="toast ds-glass ds-glass--thick"
			data-kind={item.kind}
			role="status"
			animate:glassFlip
			transition:fly={{ y: 24, duration: safeDuration(360), easing: backOut }}
		>
			<span class="dot" aria-hidden="true"></span>
			<span class="msg">{item.message}</span>
			<button
				class="close ds-focusable"
				aria-label="Dismiss notification"
				onclick={() => toast.dismiss(item.id)}
			>
				<svg viewBox="0 0 16 16" aria-hidden="true">
					<path
						d="M4.5 4.5 11.5 11.5 M11.5 4.5 4.5 11.5"
						stroke="currentColor"
						stroke-width="1.8"
						stroke-linecap="round"
					/>
				</svg>
			</button>
		</div>
	{/each}
</div>

<style>
	.host {
		position: fixed;
		bottom: calc(var(--space-7) + env(safe-area-inset-bottom, 0px));
		left: 50%;
		transform: translateX(-50%);
		z-index: 200;
		display: flex;
		flex-direction: column-reverse;
		align-items: center;
		gap: var(--space-3);
		pointer-events: none;
		max-width: min(24rem, calc(100vw - 2 * var(--space-6)));
	}

	.toast {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-3) var(--space-3) var(--space-3) var(--space-5);
		border-radius: var(--radius-pill);
		box-shadow: var(--elevation-glass-2);
		pointer-events: auto;
		max-width: 100%;
	}

	.dot {
		width: 0.5rem;
		height: 0.5rem;
		border-radius: 50%;
		flex-shrink: 0;
	}
	.toast[data-kind='info'] .dot {
		background: var(--color-accent);
	}
	.toast[data-kind='success'] .dot {
		background: var(--status-up);
	}
	.toast[data-kind='error'] .dot {
		background: var(--status-down);
	}

	.msg {
		font-size: var(--text-subheadline);
		font-weight: var(--weight-medium);
		color: var(--color-text-primary);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.close {
		display: inline-grid;
		place-items: center;
		width: 1.375rem;
		height: 1.375rem;
		border: 0;
		border-radius: 50%;
		background: var(--hairline);
		color: var(--color-text-secondary);
		cursor: pointer;
		flex-shrink: 0;
	}
	.close:focus-visible {
		outline: none;
		box-shadow: var(--focus-ring);
	}
	.close svg {
		width: 0.625rem;
		height: 0.625rem;
	}
</style>
