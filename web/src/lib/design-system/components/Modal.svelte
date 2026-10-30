<script lang="ts">
	// Adaptive modal: centred glass dialog on desktop, bottom sheet
	// (with grab handle) on small screens. Focus is trapped; Escape and
	// scrim click dismiss.
	import type { Snippet } from 'svelte';
	import { fade, fly, scale } from 'svelte/transition';
	import { safeDuration } from '../motion/motion.svelte.js';
	import { breakpoints } from '../tokens/tokens.js';

	interface Props {
		open?: boolean;
		title?: string;
		/** Called after the modal is dismissed (Escape, scrim, close button). */
		onclose?: () => void;
		/** Hide the top-right close button. */
		dismissable?: boolean;
		footer?: Snippet;
		children?: Snippet;
		class?: string;
	}

	let {
		open = $bindable(false),
		title,
		onclose,
		dismissable = true,
		footer,
		children,
		class: className = ''
	}: Props = $props();

	const titleId = $props.id();
	let panelEl: HTMLElement | undefined = $state();
	let previouslyFocused: HTMLElement | null = null;

	// Whether the modal renders as a bottom sheet, tracked live so a rotation or
	// a window resize across the breakpoint re-lays it out.
	//
	// In an $effect for its CLEANUP, not for its reactivity. Setup code adds the
	// listener once per component INSTANCE and there was nothing to remove it,
	// so every modal ever opened left a live `change` handler behind, holding a
	// closure over the destroyed component's state. Modals are used in twelve
	// places here; on a dashboard left open for a day the listeners accumulate
	// and each viewport crossing runs all of them. The module-level watcher in
	// motion.svelte.ts is fine by contrast — that one is a singleton, added once
	// for the life of the page.
	//
	// $effect also replaces the `browser` guard: effects do not run during
	// prerendering.
	let isSheet = $state(false);
	$effect(() => {
		const mq = window.matchMedia(`(max-width: ${breakpoints.md - 1}px)`);
		isSheet = mq.matches;
		const onChange = (e: MediaQueryListEvent) => (isSheet = e.matches);
		mq.addEventListener('change', onChange);
		return () => mq.removeEventListener('change', onChange);
	});

	function close() {
		open = false;
		onclose?.();
	}

	const FOCUSABLE =
		'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

	function trapFocus(e: KeyboardEvent) {
		if (e.key === 'Escape') {
			e.stopPropagation();
			close();
			return;
		}
		if (e.key !== 'Tab' || !panelEl) return;
		const focusables = [...panelEl.querySelectorAll<HTMLElement>(FOCUSABLE)];
		if (focusables.length === 0) {
			e.preventDefault();
			return;
		}
		const first = focusables[0];
		const last = focusables[focusables.length - 1];
		const active = document.activeElement;
		if (e.shiftKey && (active === first || !panelEl.contains(active))) {
			e.preventDefault();
			last.focus();
		} else if (!e.shiftKey && active === last) {
			e.preventDefault();
			first.focus();
		}
	}

	$effect(() => {
		if (open && panelEl) {
			previouslyFocused = document.activeElement as HTMLElement | null;
			const target = panelEl.querySelector<HTMLElement>(FOCUSABLE) ?? panelEl;
			target.focus();
			document.body.style.overflow = 'hidden';
			return () => {
				document.body.style.overflow = '';
				previouslyFocused?.focus();
			};
		}
	});
</script>

{#if open}
	<div class="layer" class:sheet={isSheet} onkeydown={trapFocus} role="presentation">
		<div
			class="scrim"
			transition:fade={{ duration: safeDuration(200) }}
			onclick={close}
			aria-hidden="true"
		></div>
		{#if isSheet}
			<div
				bind:this={panelEl}
				class="panel ds-glass ds-glass--thick sheet-panel {className}"
				role="dialog"
				aria-modal="true"
				aria-labelledby={title ? titleId : undefined}
				tabindex="-1"
				transition:fly={{ y: 320, duration: safeDuration(380), opacity: 1 }}
			>
				<div class="grab" aria-hidden="true"></div>
				{@render inner()}
			</div>
		{:else}
			<div
				bind:this={panelEl}
				class="panel ds-glass ds-glass--thick dialog-panel {className}"
				role="dialog"
				aria-modal="true"
				aria-labelledby={title ? titleId : undefined}
				tabindex="-1"
				transition:scale={{ start: 0.92, duration: safeDuration(260) }}
			>
				{@render inner()}
			</div>
		{/if}
	</div>
{/if}

{#snippet inner()}
	{#if title || dismissable}
		<header class="head">
			{#if title}<h2 class="title" id={titleId}>{title}</h2>{/if}
			{#if dismissable}
				<button class="close ds-focusable" aria-label="Close dialog" onclick={close}>
					<svg viewBox="0 0 16 16" aria-hidden="true">
						<path
							d="M4.5 4.5 11.5 11.5 M11.5 4.5 4.5 11.5"
							stroke="currentColor"
							stroke-width="1.8"
							stroke-linecap="round"
						/>
					</svg>
				</button>
			{/if}
		</header>
	{/if}
	<div class="body">
		{@render children?.()}
	</div>
	{#if footer}
		<footer class="foot">
			{@render footer()}
		</footer>
	{/if}
{/snippet}

<style>
	.layer {
		position: fixed;
		inset: 0;
		z-index: 100;
		display: grid;
		place-items: center;
		padding: var(--space-6);
	}
	.layer.sheet {
		place-items: end stretch;
		padding: 0;
	}

	.scrim {
		position: absolute;
		inset: 0;
		background: var(--scrim);
	}

	.panel {
		position: relative;
		display: flex;
		flex-direction: column;
		max-height: min(85dvh, 46rem);
		outline: none;
	}
	.dialog-panel {
		width: min(28rem, 100%);
		border-radius: var(--radius-squircle-lg);
		box-shadow: var(--elevation-glass-3);
	}
	.sheet-panel {
		width: 100%;
		border-radius: var(--radius-squircle-lg) var(--radius-squircle-lg) 0 0;
		border-bottom: 0;
		box-shadow:
			var(--highlight-top),
			0 -18px 60px rgba(var(--glass-shadow-color), 0.35);
		padding-bottom: env(safe-area-inset-bottom, 0);
	}

	.grab {
		width: 2.25rem;
		height: 0.3125rem;
		border-radius: var(--radius-pill);
		background: var(--hairline-strong);
		margin: var(--space-3) auto 0;
		flex-shrink: 0;
	}

	.head {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-4);
		padding: var(--space-6) var(--space-7) 0;
		flex-shrink: 0;
	}
	.title {
		margin: 0;
		font-size: var(--text-title-3);
		font-weight: var(--weight-semibold);
		letter-spacing: var(--tracking-tight);
	}
	.close {
		display: inline-grid;
		place-items: center;
		width: 1.75rem;
		height: 1.75rem;
		margin-left: auto;
		border: 0;
		border-radius: 50%;
		background: var(--hairline);
		color: var(--color-text-secondary);
		cursor: pointer;
		flex-shrink: 0;
		transition: transform var(--duration-sm) var(--ease-spring);
	}
	.close:hover {
		transform: scale(1.1);
	}
	.close:focus-visible {
		outline: none;
		box-shadow: var(--focus-ring);
	}
	.close svg {
		width: 0.75rem;
		height: 0.75rem;
	}

	.body {
		padding: var(--space-5) var(--space-7) var(--space-7);
		overflow-y: auto;
		color: var(--color-text-primary);
	}

	.foot {
		display: flex;
		justify-content: flex-end;
		gap: var(--space-4);
		padding: 0 var(--space-7) var(--space-7);
		flex-shrink: 0;
	}

	@media (prefers-reduced-motion: reduce) {
		.close,
		.close:hover {
			transition: none;
			transform: none;
		}
	}
</style>
