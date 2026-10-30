<script lang="ts">
	// Custom dropdown/select rendered as a glass panel, with full listbox
	// keyboard support (arrows, Home/End, type-ahead, Enter/Escape).
	import { scale } from 'svelte/transition';
	import { backOut } from 'svelte/easing';
	import { safeDuration } from '../motion/motion.svelte.js';

	interface Option {
		value: string;
		label: string;
		disabled?: boolean;
	}

	interface Props {
		options: Option[];
		value?: string;
		placeholder?: string;
		label?: string;
		disabled?: boolean;
		onchange?: (value: string) => void;
		class?: string;
	}

	let {
		options,
		value = $bindable(''),
		placeholder = 'Select…',
		label,
		disabled = false,
		onchange,
		class: className = ''
	}: Props = $props();

	const baseId = $props.id();
	let open = $state(false);
	let highlighted = $state(-1);
	let rootEl: HTMLElement | undefined = $state();
	let listEl: HTMLElement | undefined = $state();
	let buttonEl: HTMLButtonElement | undefined = $state();
	let typeahead = '';
	let typeaheadTimer: ReturnType<typeof setTimeout> | undefined;

	const selected = $derived(options.find((o) => o.value === value));

	// Portaled, fixed-position menu geometry (see `portal` + `updatePosition`).
	const MENU_GAP = 8; // px between trigger and menu
	const MENU_EDGE = 8; // px min gap to the viewport edge
	const MENU_MAX = 256; // px hard cap (matches the 16rem design max)
	let menuTop = $state(0);
	let menuLeft = $state(0);
	let menuWidth = $state(0);
	let menuMaxHeight = $state(MENU_MAX);
	let flipUp = $state(false);

	const listStyle = $derived(
		`top:${menuTop}px;left:${menuLeft}px;width:${menuWidth}px;max-height:${menuMaxHeight}px;`
	);

	// Move the open list to <body> so no ancestor `overflow:hidden|auto`
	// (GlassCard, modal body, scroll container) can clip it.
	function portal(node: HTMLElement) {
		document.body.appendChild(node);
		return {
			destroy() {
				node.remove();
			}
		};
	}

	// Position the (body-portaled) list against the trigger in viewport
	// coordinates so no ancestor overflow can clip it. Flip above the trigger
	// when there isn't room below.
	function updatePosition() {
		if (!buttonEl) return;
		const rect = buttonEl.getBoundingClientRect();
		const spaceBelow = window.innerHeight - rect.bottom - MENU_GAP - MENU_EDGE;
		const spaceAbove = rect.top - MENU_GAP - MENU_EDGE;
		// Natural content height once the list is mounted; the full cap otherwise.
		const natural = listEl ? listEl.scrollHeight : MENU_MAX;
		const desired = Math.min(MENU_MAX, natural);

		flipUp = spaceBelow < desired && spaceAbove > spaceBelow;
		const avail = Math.max(flipUp ? spaceAbove : spaceBelow, 0);
		menuMaxHeight = Math.min(MENU_MAX, Math.max(avail, 96));

		const height = Math.min(desired, menuMaxHeight);
		menuWidth = rect.width;
		menuLeft = rect.left;
		menuTop = flipUp ? rect.top - MENU_GAP - height : rect.bottom + MENU_GAP;
	}

	function openList() {
		if (disabled) return;
		open = true;
		highlighted = Math.max(
			0,
			options.findIndex((o) => o.value === value)
		);
		updatePosition();
	}

	function closeList(refocus = true) {
		open = false;
		highlighted = -1;
		if (refocus) buttonEl?.focus();
	}

	function commit(idx: number) {
		const opt = options[idx];
		if (!opt || opt.disabled) return;
		value = opt.value;
		onchange?.(opt.value);
		closeList();
	}

	function move(delta: number) {
		if (!options.length) return;
		let idx = highlighted;
		for (let i = 0; i < options.length; i++) {
			idx = (idx + delta + options.length) % options.length;
			if (!options[idx].disabled) break;
		}
		highlighted = idx;
		scrollHighlightedIntoView();
	}

	function scrollHighlightedIntoView() {
		requestAnimationFrame(() => {
			listEl
				?.querySelector<HTMLElement>(`[data-index="${highlighted}"]`)
				?.scrollIntoView({ block: 'nearest' });
		});
	}

	function onkeydown(e: KeyboardEvent) {
		if (!open) {
			if (['Enter', ' ', 'ArrowDown', 'ArrowUp'].includes(e.key)) {
				e.preventDefault();
				openList();
			}
			return;
		}
		switch (e.key) {
			case 'Escape':
				e.stopPropagation();
				closeList();
				break;
			case 'Tab':
				closeList(false);
				break;
			case 'ArrowDown':
				e.preventDefault();
				move(1);
				break;
			case 'ArrowUp':
				e.preventDefault();
				move(-1);
				break;
			case 'Home':
				e.preventDefault();
				highlighted = options.findIndex((o) => !o.disabled);
				scrollHighlightedIntoView();
				break;
			case 'End':
				e.preventDefault();
				highlighted = options.length - 1 - [...options].reverse().findIndex((o) => !o.disabled);
				scrollHighlightedIntoView();
				break;
			case 'Enter':
			case ' ':
				e.preventDefault();
				commit(highlighted);
				break;
			default:
				if (e.key.length === 1 && !e.metaKey && !e.ctrlKey && !e.altKey) {
					typeahead += e.key.toLowerCase();
					clearTimeout(typeaheadTimer);
					typeaheadTimer = setTimeout(() => (typeahead = ''), 500);
					const idx = options.findIndex(
						(o) => !o.disabled && o.label.toLowerCase().startsWith(typeahead)
					);
					if (idx >= 0) {
						highlighted = idx;
						scrollHighlightedIntoView();
					}
				}
		}
	}

	function onDocumentPointerDown(e: PointerEvent) {
		// The list is portaled to <body>, so it is NOT inside rootEl — a naive
		// "outside the component" test would treat clicking an option as an
		// outside click and close the list before its `click` commits. Exclude
		// listEl too, and let genuine outside clicks / Escape / selection close.
		const target = e.target as Node;
		if (open && !rootEl?.contains(target) && !listEl?.contains(target)) closeList(false);
	}

	// While open, keep the menu glued to the trigger: a scroll in ANY ancestor
	// (capture phase) or a viewport resize must reposition it.
	$effect(() => {
		if (!open) return;
		updatePosition();
		const reposition = () => updatePosition();
		window.addEventListener('scroll', reposition, true);
		window.addEventListener('resize', reposition);
		return () => {
			window.removeEventListener('scroll', reposition, true);
			window.removeEventListener('resize', reposition);
		};
	});
</script>

<svelte:document onpointerdown={onDocumentPointerDown} />

<!-- svelte-ignore a11y_no_static_element_interactions -- keydown is delegated
     from the focusable combobox trigger inside; the wrapper itself never
     receives focus. -->
<div class="root {className}" bind:this={rootEl} {onkeydown}>
	<button
		bind:this={buttonEl}
		type="button"
		class="trigger ds-glass ds-glass--thin ds-focusable"
		role="combobox"
		aria-haspopup="listbox"
		aria-expanded={open}
		aria-controls="{baseId}-list"
		aria-activedescendant={open && highlighted >= 0 ? `${baseId}-opt-${highlighted}` : undefined}
		aria-label={label}
		{disabled}
		onclick={() => (open ? closeList() : openList())}
	>
		<span class="value" class:placeholder={!selected}>{selected?.label ?? placeholder}</span>
		<svg class="chevron" class:flip={open} viewBox="0 0 16 16" aria-hidden="true">
			<path
				d="M4 6.5 8 10.5 12 6.5"
				fill="none"
				stroke="currentColor"
				stroke-width="1.8"
				stroke-linecap="round"
				stroke-linejoin="round"
			/>
		</svg>
	</button>

	{#if open}
		<ul
			bind:this={listEl}
			use:portal
			class="list ds-glass ds-glass--thick"
			class:flip-up={flipUp}
			style={listStyle}
			role="listbox"
			id="{baseId}-list"
			aria-label={label}
			transition:scale={{ start: 0.94, duration: safeDuration(200), easing: backOut }}
		>
			{#each options as opt, i (opt.value)}
				<!-- svelte-ignore a11y_click_events_have_key_events -- APG listbox:
				     keyboard selection (arrows/Enter) is handled on the combobox
				     trigger; options are pointer targets only. -->
				<li
					class="option"
					role="option"
					id="{baseId}-opt-{i}"
					data-index={i}
					aria-selected={opt.value === value}
					aria-disabled={opt.disabled || undefined}
					class:highlighted={i === highlighted}
					class:disabled={opt.disabled}
					onpointerenter={() => {
						if (!opt.disabled) highlighted = i;
					}}
					onpointerdown={(e) => e.preventDefault()}
					onclick={() => commit(i)}
				>
					<span class="check" aria-hidden="true">
						{#if opt.value === value}
							<svg viewBox="0 0 16 16">
								<path
									d="M3.5 8.5 6.5 11.5 12.5 5"
									fill="none"
									stroke="currentColor"
									stroke-width="2"
									stroke-linecap="round"
									stroke-linejoin="round"
								/>
							</svg>
						{/if}
					</span>
					{opt.label}
				</li>
			{/each}
		</ul>
	{/if}
</div>

<style>
	.root {
		position: relative;
		display: inline-block;
		min-width: 12rem;
	}

	.trigger {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-3);
		width: 100%;
		padding: var(--space-3) var(--space-4) var(--space-3) var(--space-5);
		border-radius: var(--radius-squircle-sm);
		font-family: inherit;
		font-size: var(--text-subheadline);
		color: var(--color-text-primary);
		cursor: pointer;
		text-align: left;
		transition: border-color var(--duration-sm) ease;
	}
	.trigger:hover:not(:disabled) {
		border-color: var(--glass-border-strong);
	}
	.trigger:disabled {
		cursor: not-allowed;
		opacity: 0.55;
	}

	/* One line, ellipsised: a trigger that wraps grows taller than its row. */
	.value {
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.value.placeholder {
		color: var(--color-text-tertiary);
	}

	.chevron {
		width: 0.875rem;
		height: 0.875rem;
		flex-shrink: 0;
		color: var(--color-text-tertiary);
		transition: transform var(--duration-md) var(--ease-spring);
	}
	.chevron.flip {
		transform: rotate(180deg);
	}

	.list {
		/* Portaled to <body>; positioned in viewport coords via inline style
		   (top/left/width/max-height) so no ancestor overflow can clip it.
		   z-index sits above the modal layer (100) and command palette (150). */
		position: fixed;
		z-index: 200;
		margin: 0;
		padding: var(--space-2);
		list-style: none;
		border-radius: var(--radius-squircle-md);
		box-shadow: var(--elevation-glass-3);
		overflow-y: auto;
		transform-origin: top center;
	}
	.list.flip-up {
		transform-origin: bottom center;
	}

	.option {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-3) var(--space-4);
		border-radius: var(--radius-squircle-sm);
		font-size: var(--text-subheadline);
		color: var(--color-text-primary);
		cursor: pointer;
		user-select: none;
	}
	.option.highlighted {
		background: var(--color-accent-tint);
		color: var(--color-accent);
	}
	.option.disabled {
		color: var(--color-text-tertiary);
		cursor: not-allowed;
	}
	.option[aria-selected='true'] {
		font-weight: var(--weight-semibold);
	}

	.check {
		display: inline-flex;
		width: 1rem;
		flex-shrink: 0;
	}
	.check svg {
		width: 0.875rem;
		height: 0.875rem;
	}

	@media (prefers-reduced-motion: reduce) {
		.chevron {
			transition: none;
		}
	}
</style>
