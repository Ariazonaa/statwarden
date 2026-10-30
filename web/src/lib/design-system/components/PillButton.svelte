<script lang="ts">
	// Pill-shaped button; springy press morph, four variants, loading state.
	import type { Snippet } from 'svelte';
	import type { HTMLButtonAttributes } from 'svelte/elements';

	interface Props extends HTMLButtonAttributes {
		variant?: 'primary' | 'secondary' | 'destructive' | 'ghost';
		size?: 'sm' | 'md' | 'lg';
		loading?: boolean;
		/** Optional leading icon. */
		icon?: Snippet;
		children?: Snippet;
	}

	let {
		variant = 'primary',
		size = 'md',
		loading = false,
		disabled = false,
		type = 'button',
		icon,
		children,
		class: className = '',
		...rest
	}: Props = $props();
</script>

<button
	{...rest}
	{type}
	class="btn ds-focusable {variant} {size} {className}"
	class:loading
	disabled={disabled || loading}
	aria-busy={loading || undefined}
>
	{#if loading}
		<span class="spinner" aria-hidden="true"></span>
	{:else if icon}
		<span class="icon" aria-hidden="true">{@render icon()}</span>
	{/if}
	<span class="label">{@render children?.()}</span>
</button>

<style>
	.btn {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		gap: var(--space-3);
		border-radius: var(--radius-pill);
		border: 1px solid transparent;
		font-family: inherit;
		font-weight: var(--weight-semibold);
		letter-spacing: var(--tracking-normal);
		cursor: pointer;
		user-select: none;
		/* A pill that breaks onto two lines stops being a pill. */
		white-space: nowrap;
		flex-shrink: 0;
		-webkit-tap-highlight-color: transparent;
		transition:
			transform var(--duration-md) var(--ease-spring),
			background-color var(--duration-sm) ease,
			border-color var(--duration-sm) ease,
			box-shadow var(--duration-md) var(--ease-out-expo),
			opacity var(--duration-sm) ease;
	}
	.btn:hover:not(:disabled) {
		transform: scale(1.03);
	}
	.btn:active:not(:disabled) {
		transform: scale(0.96);
	}
	.btn:disabled {
		cursor: not-allowed;
		opacity: 0.55;
	}
	.btn:focus-visible {
		outline: none;
		box-shadow: var(--focus-ring);
	}

	/* Sizes */
	.sm {
		padding: var(--space-2) var(--space-4);
		font-size: var(--text-footnote);
		min-height: 1.875rem;
	}
	.md {
		padding: var(--space-3) var(--space-6);
		font-size: var(--text-subheadline);
		min-height: 2.375rem;
	}
	.lg {
		padding: var(--space-4) var(--space-8);
		font-size: var(--text-body);
		min-height: 2.875rem;
	}

	/* Variants */
	.primary {
		color: var(--color-text-on-accent);
		background: var(--color-accent-gradient);
		box-shadow:
			inset 0 1px 0 rgba(255, 255, 255, 0.35),
			0 8px 20px -8px rgba(79, 70, 229, 0.6);
	}
	.primary:hover:not(:disabled) {
		box-shadow:
			inset 0 1px 0 rgba(255, 255, 255, 0.45),
			0 12px 26px -8px rgba(79, 70, 229, 0.7);
	}

	.secondary {
		color: var(--color-text-primary);
		background: var(--glass-bg-regular);
		border-color: var(--glass-border);
		box-shadow: var(--elevation-glass-1);
	}
	.secondary:hover:not(:disabled) {
		border-color: var(--glass-border-strong);
	}

	.destructive {
		color: #fff;
		background: linear-gradient(135deg, #f87171, #dc2626 60%, #b91c1c);
		box-shadow:
			inset 0 1px 0 rgba(255, 255, 255, 0.35),
			0 8px 20px -8px rgba(220, 38, 38, 0.6);
	}

	.ghost {
		color: var(--color-accent);
		background: transparent;
	}
	.ghost:hover:not(:disabled) {
		background: var(--color-accent-tint);
	}

	/* Loading spinner */
	.spinner {
		width: 1em;
		height: 1em;
		border-radius: 50%;
		border: 2px solid currentColor;
		border-top-color: transparent;
		animation: spin 0.7s linear infinite;
	}
	@keyframes spin {
		to {
			transform: rotate(360deg);
		}
	}

	.icon {
		display: inline-flex;
		align-items: center;
	}
	.icon :global(svg) {
		width: 1em;
		height: 1em;
	}

	@media (prefers-reduced-motion: reduce) {
		.btn,
		.btn:hover:not(:disabled),
		.btn:active:not(:disabled) {
			transition: none;
			transform: none;
		}
		.spinner {
			animation-duration: 1.6s;
		}
	}
</style>
