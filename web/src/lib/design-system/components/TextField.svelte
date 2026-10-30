<script lang="ts">
	// Labelled single-line input on a glass surface, with hint and
	// error messaging wired up via aria-describedby.
	import type { HTMLInputAttributes } from 'svelte/elements';

	interface Props extends Omit<HTMLInputAttributes, 'value'> {
		value?: string;
		label: string;
		/**
		 * Renders the label to assistive tech only, for repeated rows where a
		 * visible label on every one would be noise. The label stays REQUIRED —
		 * the input still needs an accessible name.
		 */
		labelHidden?: boolean;
		hint?: string;
		error?: string;
	}

	let {
		value = $bindable(''),
		label,
		labelHidden = false,
		hint,
		error,
		disabled = false,
		type = 'text',
		class: className = '',
		...rest
	}: Props = $props();

	const id = $props.id();
	const describedBy = $derived(
		[error ? `${id}-error` : null, hint ? `${id}-hint` : null].filter(Boolean).join(' ') ||
			undefined
	);
</script>

<div class="field {className}" class:disabled>
	<label for={id} class:sr-only={labelHidden}>{label}</label>
	<input
		{...rest}
		{id}
		{type}
		{disabled}
		bind:value
		class="input ds-focusable"
		class:invalid={!!error}
		aria-invalid={error ? true : undefined}
		aria-describedby={describedBy}
	/>
	{#if error}
		<p class="msg error" id="{id}-error">{error}</p>
	{:else if hint}
		<p class="msg hint" id="{id}-hint">{hint}</p>
	{/if}
</div>

<style>
	.field {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	.field.disabled {
		opacity: 0.55;
	}

	label {
		font-size: var(--text-footnote);
		font-weight: var(--weight-medium);
		color: var(--color-text-secondary);
	}
	/* Still in the accessibility tree, just not painted — see `labelHidden`. */
	label.sr-only {
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

	.input {
		appearance: none;
		width: 100%;
		font-family: inherit;
		font-size: var(--text-subheadline);
		color: var(--color-text-primary);
		padding: var(--space-3) var(--space-5);
		border-radius: var(--radius-squircle-sm);
		border: 1px solid var(--hairline);
		background: var(--glass-bg-thin);
		box-shadow: inset 0 1px 2px rgba(var(--glass-shadow-color), 0.06);
		transition:
			border-color var(--duration-sm) ease,
			box-shadow var(--duration-md) var(--ease-out-expo);
	}
	.input::placeholder {
		color: var(--color-text-tertiary);
	}
	.input:hover:not(:disabled) {
		border-color: var(--hairline-strong);
	}
	.input:focus-visible {
		outline: none;
		border-color: transparent;
		box-shadow: var(--focus-ring);
	}
	.input.invalid {
		border-color: var(--color-danger);
	}
	.input:disabled {
		cursor: not-allowed;
	}

	.msg {
		margin: 0;
		font-size: var(--text-caption-1);
	}
	.hint {
		color: var(--color-text-tertiary);
	}
	.error {
		color: var(--color-danger);
		font-weight: var(--weight-medium);
	}

	@media (prefers-reduced-motion: reduce) {
		.input {
			transition: none;
		}
	}
</style>
