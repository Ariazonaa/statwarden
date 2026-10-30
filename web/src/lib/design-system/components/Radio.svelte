<script lang="ts">
	// Styled native radio button; bind `group` across radios sharing `name`.
	interface Props {
		group?: string;
		value: string;
		label: string;
		name: string;
		disabled?: boolean;
		onchange?: (value: string) => void;
		class?: string;
	}

	let {
		group = $bindable(''),
		value,
		label,
		name,
		disabled = false,
		onchange,
		class: className = ''
	}: Props = $props();
</script>

<label class="row {className}" class:disabled>
	<span class="circle">
		<input
			type="radio"
			bind:group
			{value}
			{name}
			{disabled}
			class="ds-focusable"
			onchange={() => onchange?.(value)}
		/>
		<span class="dot" aria-hidden="true"></span>
	</span>
	<span class="text">{label}</span>
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

	.circle {
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
		border-radius: 50%;
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

	.dot {
		position: absolute;
		width: 0.5rem;
		height: 0.5rem;
		border-radius: 50%;
		background: #fff;
		pointer-events: none;
		transform: scale(0);
		transition: transform var(--duration-md) var(--ease-spring);
	}
	input:checked + .dot {
		transform: scale(1);
	}

	.text {
		font-size: var(--text-subheadline);
		color: var(--color-text-primary);
	}

	@media (prefers-reduced-motion: reduce) {
		input,
		.dot {
			transition: none;
		}
	}
</style>
