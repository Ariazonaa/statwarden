<script lang="ts">
	// Segmented switch (e.g. 24h / 7d / 30d): radiogroup semantics with
	// arrow-key navigation and a spring-gliding glass thumb.
	import { createSpring } from '../motion/motion.svelte.js';

	interface Segment {
		value: string;
		label: string;
	}

	interface Props {
		segments: Segment[];
		value?: string;
		onchange?: (value: string) => void;
		label?: string;
		size?: 'sm' | 'md';
		class?: string;
	}

	let {
		segments,
		value = $bindable(segments[0]?.value ?? ''),
		onchange,
		label,
		size = 'md',
		class: className = ''
	}: Props = $props();

	let trackEl: HTMLElement | undefined = $state();
	const thumb = createSpring({ x: 0, w: 0 }, 'snappy');
	let measured = $state(false);

	function measure(animate: boolean) {
		if (!trackEl) return;
		const el = trackEl.querySelector<HTMLElement>(`[data-value="${CSS.escape(value)}"]`);
		if (!el) return;
		const target = { x: el.offsetLeft, w: el.offsetWidth };
		if (animate) thumb.target = target;
		else thumb.set(target, { instant: true });
		measured = true;
	}

	$effect(() => {
		void value;
		void segments;
		measure(measured);
	});

	function select(v: string) {
		if (v === value) return;
		value = v;
		onchange?.(v);
	}

	function onkeydown(e: KeyboardEvent) {
		const idx = segments.findIndex((s) => s.value === value);
		let next = -1;
		if (e.key === 'ArrowRight' || e.key === 'ArrowDown') next = (idx + 1) % segments.length;
		else if (e.key === 'ArrowLeft' || e.key === 'ArrowUp')
			next = (idx - 1 + segments.length) % segments.length;
		else if (e.key === 'Home') next = 0;
		else if (e.key === 'End') next = segments.length - 1;
		if (next >= 0) {
			e.preventDefault();
			select(segments[next].value);
			trackEl
				?.querySelector<HTMLElement>(`[data-value="${CSS.escape(segments[next].value)}"]`)
				?.focus();
		}
	}
</script>

<svelte:window onresize={() => measure(false)} />

<!-- svelte-ignore a11y_interactive_supports_focus -- roving tabindex: focus
     lives on the active radio button; the group only delegates arrow keys. -->
<div
	class="segmented ds-glass ds-glass--thin {size} {className}"
	role="radiogroup"
	aria-label={label}
	bind:this={trackEl}
	{onkeydown}
>
	<span
		class="thumb"
		class:visible={measured}
		style="transform: translateX({thumb.current.x}px); width: {thumb.current.w}px"
		aria-hidden="true"
	></span>
	{#each segments as seg (seg.value)}
		<button
			class="seg ds-focusable"
			role="radio"
			data-value={seg.value}
			aria-checked={value === seg.value}
			class:active={value === seg.value}
			tabindex={value === seg.value ? 0 : -1}
			onclick={() => select(seg.value)}
		>
			{seg.label}
		</button>
	{/each}
</div>

<style>
	.segmented {
		position: relative;
		display: inline-flex;
		gap: var(--space-1);
		padding: var(--space-1);
		border-radius: var(--radius-pill);
	}

	.thumb {
		position: absolute;
		top: var(--space-1);
		left: 0;
		height: calc(100% - 2 * var(--space-1));
		border-radius: var(--radius-pill);
		background: var(--glass-bg-thick);
		border: 1px solid var(--glass-border-strong);
		box-shadow: var(--elevation-glass-1);
		opacity: 0;
		will-change: transform, width;
	}
	.thumb.visible {
		opacity: 1;
	}

	.seg {
		position: relative;
		z-index: 1;
		appearance: none;
		border: 0;
		background: transparent;
		font-family: inherit;
		font-weight: var(--weight-medium);
		color: var(--color-text-secondary);
		border-radius: var(--radius-pill);
		cursor: pointer;
		-webkit-tap-highlight-color: transparent;
		transition: color var(--duration-md) ease;
	}
	.md .seg {
		font-size: var(--text-subheadline);
		padding: var(--space-2) var(--space-5);
	}
	.sm .seg {
		font-size: var(--text-footnote);
		padding: var(--space-1) var(--space-4);
	}
	.seg:hover {
		color: var(--color-text-primary);
	}
	.seg.active {
		color: var(--color-text-primary);
		font-weight: var(--weight-semibold);
	}
	.seg:focus-visible {
		outline: none;
		box-shadow: var(--focus-ring);
	}
</style>
