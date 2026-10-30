<script lang="ts">
	// The iOS switch: role="switch" button with a spring-sliding knob.
	interface Props {
		checked?: boolean;
		disabled?: boolean;
		/** Visible label text (rendered next to the switch). */
		label?: string;
		/** Accessible name when no visible label is given. */
		ariaLabel?: string;
		onchange?: (checked: boolean) => void;
		class?: string;
	}

	let {
		checked = $bindable(false),
		disabled = false,
		label,
		ariaLabel,
		onchange,
		class: className = ''
	}: Props = $props();

	function toggle() {
		if (disabled) return;
		checked = !checked;
		onchange?.(checked);
	}
</script>

<label class="row {className}" class:disabled>
	{#if label}<span class="text">{label}</span>{/if}
	<button
		type="button"
		class="switch ds-focusable"
		role="switch"
		aria-checked={checked}
		aria-label={label ? undefined : ariaLabel}
		{disabled}
		onclick={toggle}
	>
		<span class="knob" aria-hidden="true"></span>
	</button>
</label>

<style>
	.row {
		display: inline-flex;
		align-items: center;
		gap: var(--space-4);
		cursor: pointer;
	}
	.row.disabled {
		cursor: not-allowed;
		opacity: 0.55;
	}
	.text {
		font-size: var(--text-subheadline);
		color: var(--color-text-primary);
	}

	.switch {
		position: relative;
		width: 3.125rem;
		height: 1.9375rem;
		flex-shrink: 0;
		border-radius: var(--radius-pill);
		border: 1px solid var(--hairline);
		background: var(--glass-bg-thin);
		box-shadow: inset 0 1px 3px rgba(var(--glass-shadow-color), 0.12);
		cursor: inherit;
		padding: 0;
		-webkit-tap-highlight-color: transparent;
		transition:
			background-color var(--duration-md) ease,
			border-color var(--duration-md) ease;
	}
	.switch[aria-checked='true'] {
		background: var(--status-up);
		border-color: transparent;
		box-shadow:
			inset 0 1px 0 rgba(255, 255, 255, 0.3),
			inset 0 1px 3px rgba(0, 0, 0, 0.1);
	}
	.switch:focus-visible {
		outline: none;
		box-shadow: var(--focus-ring);
	}

	.knob {
		position: absolute;
		top: 2px;
		left: 2px;
		width: calc(1.9375rem - 6px);
		height: calc(1.9375rem - 6px);
		border-radius: 50%;
		background: #fff;
		box-shadow:
			0 1px 2px rgba(0, 0, 0, 0.2),
			0 3px 8px rgba(0, 0, 0, 0.15);
		transition: transform var(--duration-md) var(--ease-spring);
		will-change: transform;
	}
	.switch[aria-checked='true'] .knob {
		transform: translateX(1.1875rem);
	}
	.switch:active:not(:disabled) .knob {
		transform: scaleX(1.12);
		transform-origin: left center;
	}
	.switch[aria-checked='true']:active:not(:disabled) .knob {
		transform: translateX(1.1875rem) scaleX(1.12);
		transform-origin: right center;
	}

	@media (prefers-reduced-motion: reduce) {
		.switch,
		.knob {
			transition: none;
		}
	}
</style>
