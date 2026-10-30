<script lang="ts">
	// Floating pill navigation bar (iOS-26 style): a thick glass pill with
	// a spring-animated indicator gliding under the active item.
	import { createSpring } from '../motion/motion.svelte.js';

	interface NavItem {
		id: string;
		label: string;
		href?: string;
	}

	interface Props {
		items: NavItem[];
		active?: string;
		onselect?: (id: string) => void;
		/** Accessible name for the nav landmark. */
		label?: string;
		class?: string;
	}

	let {
		items,
		active = $bindable(items[0]?.id ?? ''),
		onselect,
		label = 'Primary',
		class: className = ''
	}: Props = $props();

	let listEl: HTMLElement | undefined = $state();
	const indicator = createSpring({ x: 0, w: 0 }, 'snappy');
	let measured = $state(false);

	function measure(animate: boolean) {
		if (!listEl) return;
		const el = listEl.querySelector<HTMLElement>(`[data-id="${CSS.escape(active)}"]`);
		if (!el) return;
		const target = { x: el.offsetLeft, w: el.offsetWidth };
		if (animate) indicator.target = target;
		else indicator.set(target, { instant: true });
		measured = true;
	}

	$effect(() => {
		void active;
		void items;
		measure(measured);
	});

	function select(id: string) {
		active = id;
		onselect?.(id);
	}
</script>

<svelte:window onresize={() => measure(false)} />

<nav aria-label={label} class="pillnav ds-glass ds-glass--thick {className}">
	<div class="track" bind:this={listEl}>
		<span
			class="indicator"
			class:visible={measured}
			style="transform: translateX({indicator.current.x}px); width: {indicator.current.w}px"
			aria-hidden="true"
		></span>
		{#each items as item (item.id)}
			{#if item.href}
				<a
					class="item ds-focusable"
					data-id={item.id}
					href={item.href}
					aria-current={active === item.id ? 'page' : undefined}
					class:active={active === item.id}
					onclick={() => select(item.id)}
				>
					{item.label}
				</a>
			{:else}
				<button
					class="item ds-focusable"
					data-id={item.id}
					aria-current={active === item.id ? 'page' : undefined}
					class:active={active === item.id}
					onclick={() => select(item.id)}
				>
					{item.label}
				</button>
			{/if}
		{/each}
	</div>
</nav>

<style>
	.pillnav {
		display: inline-flex;
		border-radius: var(--radius-pill);
		padding: var(--space-2);
	}

	.track {
		position: relative;
		display: inline-flex;
		gap: var(--space-1);
	}

	.indicator {
		position: absolute;
		top: 0;
		left: 0;
		height: 100%;
		border-radius: var(--radius-pill);
		background: var(--color-accent-gradient);
		box-shadow:
			inset 0 1px 0 rgba(255, 255, 255, 0.35),
			0 6px 16px -6px rgba(79, 70, 229, 0.55);
		opacity: 0;
		will-change: transform, width;
	}
	.indicator.visible {
		opacity: 1;
	}

	.item {
		position: relative;
		z-index: 1;
		appearance: none;
		border: 0;
		background: transparent;
		font-family: inherit;
		font-size: var(--text-subheadline);
		font-weight: var(--weight-medium);
		letter-spacing: var(--tracking-normal);
		color: var(--color-text-secondary);
		text-decoration: none;
		padding: var(--space-3) var(--space-6);
		border-radius: var(--radius-pill);
		cursor: pointer;
		-webkit-tap-highlight-color: transparent;
		transition: color var(--duration-md) ease;
	}
	.item:hover {
		color: var(--color-text-primary);
	}
	.item.active {
		color: var(--color-text-on-accent);
	}
	.item:focus-visible {
		outline: none;
		box-shadow: var(--focus-ring);
	}
</style>
