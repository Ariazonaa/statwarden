<script lang="ts">
	// Ctrl/Cmd+K command overlay: glass panel, fuzzy-ish filtering,
	// grouped results, full keyboard navigation.
	import { fade, scale } from 'svelte/transition';
	import { backOut } from 'svelte/easing';
	import { safeDuration } from '../motion/motion.svelte.js';
	import Kbd from './Kbd.svelte';

	export interface Command {
		id: string;
		label: string;
		group?: string;
		hint?: string;
		/** e.g. "⌘D" — display only. */
		kbd?: string;
		action: () => void;
	}

	interface Props {
		commands: Command[];
		open?: boolean;
		/** Register the global Ctrl/Cmd+K shortcut. */
		shortcut?: boolean;
		placeholder?: string;
		/** Empty-state text; `{query}` is replaced with the current search. */
		emptyText?: string;
		class?: string;
	}

	let {
		commands,
		open = $bindable(false),
		shortcut = true,
		placeholder = 'Type a command or search…',
		emptyText = 'No results for “{query}”',
		class: className = ''
	}: Props = $props();

	const baseId = $props.id();
	let query = $state('');
	let highlighted = $state(0);
	let inputEl: HTMLInputElement | undefined = $state();
	let listEl: HTMLElement | undefined = $state();

	const filtered = $derived.by(() => {
		const q = query.trim().toLowerCase();
		if (!q) return commands;
		return commands.filter(
			(c) => c.label.toLowerCase().includes(q) || c.group?.toLowerCase().includes(q)
		);
	});

	// Stable display order grouped by `group`.
	const grouped = $derived.by(() => {
		const map = new Map<string, Command[]>();
		for (const c of filtered) {
			const g = c.group ?? '';
			if (!map.has(g)) map.set(g, []);
			map.get(g)!.push(c);
		}
		return [...map.entries()];
	});

	const flat = $derived(grouped.flatMap(([, cmds]) => cmds));

	$effect(() => {
		void filtered;
		highlighted = 0;
	});

	$effect(() => {
		if (open) {
			query = '';
			highlighted = 0;
			requestAnimationFrame(() => inputEl?.focus());
			document.body.style.overflow = 'hidden';
			return () => {
				document.body.style.overflow = '';
			};
		}
	});

	function run(cmd: Command | undefined) {
		if (!cmd) return;
		open = false;
		cmd.action();
	}

	function scrollHighlightedIntoView() {
		requestAnimationFrame(() => {
			listEl
				?.querySelector<HTMLElement>(`[data-index="${highlighted}"]`)
				?.scrollIntoView({ block: 'nearest' });
		});
	}

	function onkeydown(e: KeyboardEvent) {
		switch (e.key) {
			case 'Escape':
				e.preventDefault();
				open = false;
				break;
			case 'ArrowDown':
				e.preventDefault();
				highlighted = (highlighted + 1) % Math.max(flat.length, 1);
				scrollHighlightedIntoView();
				break;
			case 'ArrowUp':
				e.preventDefault();
				highlighted = (highlighted - 1 + flat.length) % Math.max(flat.length, 1);
				scrollHighlightedIntoView();
				break;
			case 'Enter':
				e.preventDefault();
				run(flat[highlighted]);
				break;
		}
	}

	function globalShortcut(e: KeyboardEvent) {
		if (shortcut && e.key.toLowerCase() === 'k' && (e.metaKey || e.ctrlKey)) {
			e.preventDefault();
			open = !open;
		}
	}
</script>

<svelte:window onkeydown={globalShortcut} />

{#if open}
	<div class="layer" role="presentation" {onkeydown}>
		<div
			class="scrim"
			transition:fade={{ duration: safeDuration(180) }}
			onclick={() => (open = false)}
			aria-hidden="true"
		></div>
		<div
			class="palette ds-glass ds-glass--thick {className}"
			role="dialog"
			aria-modal="true"
			aria-label="Command palette"
			transition:scale={{ start: 0.94, duration: safeDuration(240), easing: backOut }}
		>
			<div class="inputrow">
				<svg class="lens" viewBox="0 0 16 16" aria-hidden="true">
					<circle
						cx="6.75"
						cy="6.75"
						r="4.75"
						fill="none"
						stroke="currentColor"
						stroke-width="1.8"
					/>
					<path
						d="M10.5 10.5 14 14"
						stroke="currentColor"
						stroke-width="1.8"
						stroke-linecap="round"
					/>
				</svg>
				<input
					bind:this={inputEl}
					bind:value={query}
					type="text"
					{placeholder}
					role="combobox"
					aria-expanded="true"
					aria-controls="{baseId}-list"
					aria-activedescendant={flat[highlighted]
						? `${baseId}-opt-${flat[highlighted].id}`
						: undefined}
					aria-autocomplete="list"
					autocomplete="off"
					spellcheck="false"
				/>
				<Kbd>Esc</Kbd>
			</div>

			<div
				class="results"
				bind:this={listEl}
				id="{baseId}-list"
				role="listbox"
				aria-label="Commands"
			>
				{#if flat.length === 0}
					<p class="none">{emptyText.replace('{query}', query)}</p>
				{:else}
					{#each grouped as [group, cmds] (group)}
						{#if group}
							<p class="group" role="presentation">{group}</p>
						{/if}
						{#each cmds as cmd (cmd.id)}
							{@const idx = flat.indexOf(cmd)}
							<!-- svelte-ignore a11y_interactive_supports_focus, a11y_click_events_have_key_events
							     -- APG combobox: focus stays in the input; options are
							     referenced via aria-activedescendant and keyboard
							     selection happens on the input's keydown. -->
							<div
								class="cmd"
								role="option"
								id="{baseId}-opt-{cmd.id}"
								data-index={idx}
								aria-selected={idx === highlighted}
								class:highlighted={idx === highlighted}
								onpointerenter={() => (highlighted = idx)}
								onpointerdown={(e) => e.preventDefault()}
								onclick={() => run(cmd)}
							>
								<span class="label">{cmd.label}</span>
								{#if cmd.hint}<span class="hint">{cmd.hint}</span>{/if}
								{#if cmd.kbd}<Kbd>{cmd.kbd}</Kbd>{/if}
							</div>
						{/each}
					{/each}
				{/if}
			</div>
		</div>
	</div>
{/if}

<style>
	.layer {
		position: fixed;
		inset: 0;
		z-index: 150;
		display: grid;
		place-items: start center;
		padding: 12dvh var(--space-6) var(--space-6);
	}

	.scrim {
		position: absolute;
		inset: 0;
		background: var(--scrim);
	}

	.palette {
		position: relative;
		width: min(34rem, 100%);
		border-radius: var(--radius-squircle-lg);
		box-shadow: var(--elevation-glass-3);
		overflow: hidden;
		transform-origin: top center;
	}

	.inputrow {
		display: flex;
		align-items: center;
		gap: var(--space-4);
		padding: var(--space-5) var(--space-6);
		border-bottom: 1px solid var(--hairline);
	}
	.lens {
		width: 1.125rem;
		height: 1.125rem;
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
		font-size: var(--text-body);
		color: var(--color-text-primary);
		outline: none;
	}
	input::placeholder {
		color: var(--color-text-tertiary);
	}

	.results {
		max-height: min(46dvh, 22rem);
		overflow-y: auto;
		padding: var(--space-3);
	}

	.none {
		margin: 0;
		padding: var(--space-7);
		text-align: center;
		font-size: var(--text-subheadline);
		color: var(--color-text-tertiary);
	}

	.group {
		margin: var(--space-3) 0 var(--space-1);
		padding: 0 var(--space-4);
		font-size: var(--text-caption-1);
		font-weight: var(--weight-semibold);
		text-transform: uppercase;
		letter-spacing: var(--tracking-wide);
		color: var(--color-text-tertiary);
	}

	.cmd {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-3) var(--space-4);
		border-radius: var(--radius-squircle-sm);
		cursor: pointer;
		user-select: none;
	}
	.cmd.highlighted {
		background: var(--color-accent-tint);
	}
	.cmd.highlighted .label {
		color: var(--color-accent);
	}

	.label {
		font-size: var(--text-subheadline);
		font-weight: var(--weight-medium);
		color: var(--color-text-primary);
	}
	.hint {
		margin-left: auto;
		font-size: var(--text-caption-1);
		color: var(--color-text-tertiary);
	}
	.hint + :global(kbd),
	.label + :global(kbd) {
		margin-left: auto;
	}
	.hint + :global(kbd) {
		margin-left: var(--space-2);
	}
</style>
