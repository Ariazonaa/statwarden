<script lang="ts">
	// Styled native checkbox (keeps built-in semantics) with a springy
	// check pop.
	interface Props {
		checked?: boolean;
		label: string;
		disabled?: boolean;
		/**
		 * Renders the label to assistive tech only, for the box-in-a-dense-row
		 * case where surrounding content already names the target. The label stays
		 * REQUIRED: a checkbox whose text is hidden still needs an accessible
		 * name, so this hides it rather than letting a caller omit it.
		 */
		labelHidden?: boolean;
		onchange?: (checked: boolean) => void;
		class?: string;
	}

	let {
		checked = $bindable(false),
		label,
		disabled = false,
		labelHidden = false,
		onchange,
		class: className = ''
	}: Props = $props();
</script>

<label class="row {className}" class:disabled>
	<span class="box">
		<input
			type="checkbox"
			bind:checked
			{disabled}
			class="ds-focusable"
			onchange={() => onchange?.(checked)}
		/>
		<svg class="tick" viewBox="0 0 16 16" aria-hidden="true">
			<path
				d="M3.5 8.5 6.5 11.5 12.5 5"
				fill="none"
				stroke="currentColor"
				stroke-width="2.4"
				stroke-linecap="round"
				stroke-linejoin="round"
			/>
		</svg>
	</span>
	<span class="text" class:sr-only={labelHidden}>{label}</span>
</label>

<style>
	.row {
		display: inline-flex;
		align-items: center;
		gap: var(--space-4);
		cursor: pointer;
		user-select: none;
	}
	.row.disabled {
		cursor: not-allowed;
		opacity: 0.55;
	}

	.box {
		position: relative;
		display: inline-grid;
		place-items: center;
		width: 1.375rem;
		height: 1.375rem;
		flex-shrink: 0;
	}

	input {
		appearance: none;
		margin: 0;
		width: 100%;
		height: 100%;
		border-radius: 0.4375rem;
		border: 1.5px solid var(--hairline-strong);
		background: var(--glass-bg-thin);
		box-shadow: inset 0 1px 2px rgba(var(--glass-shadow-color), 0.08);
		cursor: inherit;
		transition:
			background-color var(--duration-sm) ease,
			border-color var(--duration-sm) ease,
			transform var(--duration-sm) var(--ease-spring);
	}
	input:checked {
		background: var(--color-accent-gradient);
		border-color: transparent;
		box-shadow: inset 0 1px 0 rgba(255, 255, 255, 0.35);
	}
	input:active:not(:disabled) {
		transform: scale(0.9);
	}
	input:focus-visible {
		outline: none;
		box-shadow: var(--focus-ring);
	}

	.tick {
		position: absolute;
		width: 0.875rem;
		height: 0.875rem;
		color: #fff;
		pointer-events: none;
		transform: scale(0);
		transition: transform var(--duration-md) var(--ease-spring);
	}
	input:checked + .tick {
		transform: scale(1);
	}

	.text {
		font-size: var(--text-subheadline);
		color: var(--color-text-primary);
	}
	/* Still in the accessibility tree, just not painted — see `labelHidden`. */
	.text.sr-only {
		position: absolute;
		width: 1px;
		height: 1px;
		margin: -1px;
		padding: 0;
		overflow: hidden;
		white-space: nowrap;
		clip-path: inset(50%);
		border: 0;
	}
	.row:has(.text.sr-only) {
		gap: 0;
	}

	@media (prefers-reduced-motion: reduce) {
		input,
		.tick {
			transition: none;
		}
	}
</style>
