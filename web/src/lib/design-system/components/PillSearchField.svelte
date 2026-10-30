<script lang="ts">
	// Pill-shaped search field on a glass background, with a clear button
	// and optional keyboard-shortcut hint chip.
	import Kbd from './Kbd.svelte';

	interface Props {
		value?: string;
		placeholder?: string;
		/** Accessible name. */
		label?: string;
		/** e.g. "⌘K" — shown as a KBD chip while the field is empty. */
		shortcutHint?: string;
		oninput?: (value: string) => void;
		onsubmit?: (value: string) => void;
		class?: string;
	}

	let {
		value = $bindable(''),
		placeholder = 'Search…',
		label = 'Search',
		shortcutHint,
		oninput,
		onsubmit,
		class: className = ''
	}: Props = $props();

	let inputEl: HTMLInputElement | undefined = $state();

	export function focus() {
		inputEl?.focus();
	}

	function clear() {
		value = '';
		oninput?.('');
		inputEl?.focus();
	}
</script>

<div class="search ds-glass ds-glass--thin {className}">
	<svg class="lens" viewBox="0 0 16 16" aria-hidden="true">
		<circle cx="6.75" cy="6.75" r="4.75" fill="none" stroke="currentColor" stroke-width="1.8" />
		<path d="M10.5 10.5 14 14" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" />
	</svg>
	<input
		bind:this={inputEl}
		bind:value
		type="search"
		aria-label={label}
		{placeholder}
		oninput={() => oninput?.(value)}
		onkeydown={(e) => {
			if (e.key === 'Enter') onsubmit?.(value);
			if (e.key === 'Escape' && value) {
				e.stopPropagation();
				clear();
			}
		}}
	/>
	{#if value}
		<button class="clear ds-focusable" aria-label="Clear search" onclick={clear}>
			<svg viewBox="0 0 16 16" aria-hidden="true">
				<path
					d="M4.5 4.5 11.5 11.5 M11.5 4.5 4.5 11.5"
					stroke="currentColor"
					stroke-width="1.8"
					stroke-linecap="round"
				/>
			</svg>
		</button>
	{:else if shortcutHint}
		<Kbd>{shortcutHint}</Kbd>
	{/if}
</div>

<style>
	.search {
		display: inline-flex;
		align-items: center;
		gap: var(--space-3);
		border-radius: var(--radius-pill);
		padding: var(--space-2) var(--space-4) var(--space-2) var(--space-5);
		min-width: 15rem;
		transition:
			border-color var(--duration-sm) ease,
			box-shadow var(--duration-md) var(--ease-out-expo);
	}
	.search:focus-within {
		border-color: var(--glass-border-strong);
		box-shadow: var(--highlight-top), var(--shadow-2), var(--focus-ring);
	}

	.lens {
		width: 1rem;
		height: 1rem;
		flex-shrink: 0;
		color: var(--color-text-tertiary);
	}

	input {
		flex: 1;
		min-width: 0;
		appearance: none;
		border: 0;
		background: transparent;
		font-family: inherit;
		font-size: var(--text-subheadline);
		color: var(--color-text-primary);
		padding: var(--space-2) 0;
		outline: none;
	}
	input::placeholder {
		color: var(--color-text-tertiary);
	}
	input::-webkit-search-cancel-button {
		display: none;
	}

	.clear {
		display: inline-grid;
		place-items: center;
		width: 1.375rem;
		height: 1.375rem;
		border: 0;
		border-radius: 50%;
		background: var(--hairline);
		color: var(--color-text-secondary);
		cursor: pointer;
		transition: transform var(--duration-sm) var(--ease-spring);
	}
	.clear:hover {
		transform: scale(1.12);
	}
	.clear:focus-visible {
		outline: none;
		box-shadow: var(--focus-ring);
	}
	.clear svg {
		width: 0.625rem;
		height: 0.625rem;
	}

	@media (prefers-reduced-motion: reduce) {
		.clear,
		.clear:hover {
			transition: none;
			transform: none;
		}
	}
</style>
