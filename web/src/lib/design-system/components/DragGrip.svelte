<script lang="ts">
	// Drag handle for sortable rows. Renders as a button so reordering has a
	// keyboard path: Space/Enter lifts, arrows move, Escape cancels.
	import type { HTMLButtonAttributes } from 'svelte/elements';

	interface Props extends HTMLButtonAttributes {
		label: string;
		/** True while the row is lifted; surfaces as aria-pressed. */
		grabbed?: boolean;
		/** Temporarily inactive (e.g. a commit is in flight). Uses aria-disabled,
		 *  never the native attribute, so a keyboard user mid-reorder keeps focus. */
		inert?: boolean;
	}

	let { label, grabbed = false, inert = false, class: className = '', ...rest }: Props = $props();
</script>

<button
	type="button"
	class="grip ds-focusable {className}"
	class:grabbed
	aria-label={label}
	aria-pressed={grabbed}
	aria-disabled={inert || undefined}
	{...rest}
>
	<svg viewBox="0 0 16 16" aria-hidden="true"
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
</button>

<style>
	.grip {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		width: 2rem;
		height: 2rem;
		padding: 0;
		border: 1px solid transparent;
		border-radius: var(--radius-squircle-sm);
		background: transparent;
		color: var(--color-text-tertiary);
		cursor: grab;
		user-select: none;
		-webkit-tap-highlight-color: transparent;
		/* Mandatory: without it the browser claims the gesture and scrolls
		   instead of letting pointermove drive the drag. */
		touch-action: none;
		transition:
			color var(--duration-sm) ease,
			background-color var(--duration-sm) ease,
			border-color var(--duration-sm) ease,
			transform var(--duration-md) var(--ease-spring);
	}
	.grip:hover:not([aria-disabled='true']) {
		color: var(--color-text-primary);
		background: var(--glass-bg-thin);
		border-color: var(--glass-border);
	}
	.grip:active:not([aria-disabled='true']),
	.grip.grabbed {
		cursor: grabbing;
	}
	.grip.grabbed {
		color: var(--color-accent);
		background: var(--color-accent-tint);
		transform: scale(1.08);
	}
	.grip[aria-disabled='true'] {
		cursor: default;
		opacity: 0.5;
	}
	.grip:focus-visible {
		outline: none;
		box-shadow: var(--focus-ring);
	}

	svg {
		width: 1rem;
		height: 1rem;
		fill: currentColor;
		pointer-events: none;
	}

	@media (prefers-reduced-motion: reduce) {
		.grip,
		.grip.grabbed {
			transition: none;
			transform: none;
		}
	}
</style>
