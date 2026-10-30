<script lang="ts">
	// Brand-colour picker: a native colour swatch plus a hex text input and a row
	// of preset swatches. Emits lowercase #rrggbb or empty.
	import PillButton from './PillButton.svelte';

	interface Props {
		value?: string;
		label: string;
		hint?: string;
		error?: string;
		disabled?: boolean;
		presets?: string[];
		clearLabel?: string;
		class?: string;
	}

	let {
		value = $bindable(''),
		label,
		hint,
		error,
		disabled = false,
		presets = ['#4f46e5', '#0ea5e9', '#10b981', '#f59e0b', '#ef4444', '#8b5cf6'],
		clearLabel = 'Reset',
		class: className = ''
	}: Props = $props();

	const id = $props.id();

	/** Set when the typed hex was rejected; a consumer error still wins. */
	let invalid = $state(false);
	let hexInput = $state<HTMLInputElement>();

	const errorText = $derived(error || (invalid ? 'Enter a hex colour like #4f46e5.' : undefined));
	const describedBy = $derived(
		[errorText ? `${id}-error` : null, hint ? `${id}-hint` : null].filter(Boolean).join(' ') ||
			undefined
	);

	const hexRe = /^#([0-9a-fA-F]{3}|[0-9a-fA-F]{6})$/;

	/**
	 * Mirrors normalizeHexColor in internal/api/logos.go: #rgb or #rrggbb in any
	 * case becomes lowercase #rrggbb, anything else is null. Accepting exactly
	 * what the API accepts keeps a shorthand the server would have taken from
	 * being refused here.
	 */
	function normalizeHex(v: string): string | null {
		const s = v.trim().toLowerCase();
		if (!hexRe.test(s)) return null;
		return s.length === 4 ? `#${s[1]}${s[1]}${s[2]}${s[2]}${s[3]}${s[3]}` : s;
	}

	// The native colour input has no empty state, so fall back to the product
	// accent when nothing is set.
	const swatch = $derived(normalizeHex(value) ?? '#4f46e5');

	function commit(v: string) {
		const next = v.trim() === '' ? '' : normalizeHex(v);
		if (next === null) {
			// Reject loudly rather than dropping the input on the floor.
			invalid = true;
			syncHex();
			return;
		}
		invalid = false;
		value = next;
		// Put the committed value back on screen. `value` may be unchanged (a
		// rejection, or a retyped `#F80` that normalises to what is already
		// stored), and then the {value} attribute does not re-render, leaving the
		// typed text sitting in the box disagreeing with what was committed.
		syncHex();
	}

	/** Force the hex box back to `value`, whatever it currently displays. */
	function syncHex() {
		if (hexInput && hexInput.value !== value) hexInput.value = value;
	}
</script>

<div class="field {className}" class:disabled>
	<label for={id}>{label}</label>
	<div class="row">
		<input
			class="swatch ds-focusable"
			type="color"
			{id}
			value={swatch}
			{disabled}
			aria-describedby={describedBy}
			oninput={(e) => commit(e.currentTarget.value)}
		/>
		<input
			class="hex ds-focusable"
			bind:this={hexInput}
			type="text"
			inputmode="text"
			maxlength="7"
			spellcheck="false"
			placeholder="#4f46e5"
			{value}
			{disabled}
			aria-label={label}
			aria-invalid={errorText ? 'true' : undefined}
			onchange={(e) => commit(e.currentTarget.value.trim())}
		/>
		{#each presets as p (p)}
			<button
				type="button"
				class="preset ds-focusable"
				style="--sw: {p}"
				aria-label={p}
				{disabled}
				onclick={() => commit(p)}
			></button>
		{/each}
		{#if value}
			<PillButton size="sm" variant="ghost" {disabled} onclick={() => commit('')}
				>{clearLabel}</PillButton
			>
		{/if}
	</div>
	{#if errorText}
		<p class="msg error" id="{id}-error">{errorText}</p>
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

	.row {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: var(--space-3);
	}

	.hex {
		appearance: none;
		width: 7.5rem;
		font-family: inherit;
		font-size: var(--text-subheadline);
		font-variant-numeric: tabular-nums;
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
	.hex::placeholder {
		color: var(--color-text-tertiary);
	}
	.hex:hover:not(:disabled) {
		border-color: var(--hairline-strong);
	}
	.hex:disabled,
	.swatch:disabled,
	.preset:disabled {
		cursor: not-allowed;
	}

	.swatch {
		appearance: none;
		-webkit-appearance: none;
		width: var(--space-8);
		height: var(--space-8);
		padding: 0;
		border: 1px solid var(--glass-border);
		border-radius: var(--radius-pill);
		background: none;
		cursor: pointer;
	}
	.swatch::-webkit-color-swatch-wrapper {
		padding: 0;
	}
	.swatch::-webkit-color-swatch {
		border: none;
		border-radius: var(--radius-pill);
	}

	.preset {
		width: var(--space-6);
		height: var(--space-6);
		padding: 0;
		border-radius: var(--radius-pill);
		background: var(--sw);
		border: 1px solid color-mix(in srgb, var(--sw) 30%, transparent);
		cursor: pointer;
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
		.hex {
			transition: none;
		}
	}
</style>
