<script lang="ts">
	// The foundational AMOLED surface. Everything builds on the shared
	// .ds-glass recipe (tokens/glass.css): solid near-black fill, thin
	// #1f1f1f border, inset top highlight, layered soft shadow. No blur.
	import type { Snippet } from 'svelte';
	import type { HTMLAttributes } from 'svelte/elements';

	interface Props extends HTMLAttributes<HTMLDivElement> {
		/** Surface depth — maps to the elevation (fill + shadow) scale. */
		blur?: 'thin' | 'regular' | 'thick';
		/** Corner treatment. */
		radius?: 'sm' | 'md' | 'lg' | 'pill';
		/** Subtle lift + brighter border on hover (for clickable panels). */
		interactive?: boolean;
		children?: Snippet;
	}

	let {
		blur = 'regular',
		radius = 'md',
		interactive = false,
		children,
		class: className = '',
		...rest
	}: Props = $props();
</script>

<div
	class="panel ds-glass {className}"
	class:ds-glass--thin={blur === 'thin'}
	class:ds-glass--thick={blur === 'thick'}
	class:interactive
	data-radius={radius}
	{...rest}
>
	{@render children?.()}
</div>

<style>
	.panel[data-radius='sm'] {
		border-radius: var(--radius-squircle-sm);
	}
	.panel[data-radius='md'] {
		border-radius: var(--radius-squircle-md);
	}
	.panel[data-radius='lg'] {
		border-radius: var(--radius-squircle-lg);
	}
	.panel[data-radius='pill'] {
		border-radius: var(--radius-pill);
	}

	.interactive {
		transition:
			transform var(--duration-md) var(--ease-spring),
			border-color var(--duration-sm) ease,
			box-shadow var(--duration-md) var(--ease-out-expo);
	}
	.interactive:hover {
		transform: translateY(-2px) scale(1.005);
		border-color: var(--glass-border-strong);
		box-shadow: var(--elevation-glass-3);
	}
	.interactive:active {
		transform: translateY(0) scale(0.99);
	}

	@media (prefers-reduced-motion: reduce) {
		.interactive,
		.interactive:hover,
		.interactive:active {
			transition: none;
			transform: none;
		}
	}
</style>
