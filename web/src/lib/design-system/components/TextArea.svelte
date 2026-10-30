<script lang="ts">
	// Labelled multi-line input — same glass styling as TextField.
	import type { HTMLTextareaAttributes } from 'svelte/elements';

	interface Props extends Omit<HTMLTextareaAttributes, 'value'> {
		value?: string;
		label: string;
		hint?: string;
		error?: string;
	}

	let {
		value = $bindable(''),
		label,
		hint,
		error,
		disabled = false,
		rows = 4,
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
	<label for={id}>{label}</label>
	<textarea
		{...rest}
		{id}
		{rows}
		{disabled}
		bind:value
		class="input ds-focusable"
		class:invalid={!!error}
		aria-invalid={error ? true : undefined}
		aria-describedby={describedBy}
	></textarea>
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

	.input {
		appearance: none;
		width: 100%;
		font-family: inherit;
		font-size: var(--text-subheadline);
		line-height: var(--leading-normal);
		color: var(--color-text-primary);
		padding: var(--space-4) var(--space-5);
		border-radius: var(--radius-squircle-sm);
		border: 1px solid var(--hairline);
		background: var(--glass-bg-thin);
		box-shadow: inset 0 1px 2px rgba(var(--glass-shadow-color), 0.06);
		resize: vertical;
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
