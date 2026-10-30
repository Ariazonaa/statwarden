<script lang="ts">
	// Hover/focus tooltip bubble. Wraps its trigger content and links it
	// via aria-describedby. Pointer hover is delayed; keyboard focus is
	// immediate. Escape hides.
	import type { Snippet } from 'svelte';
	import { scale } from 'svelte/transition';
	import { backOut } from 'svelte/easing';
	import { safeDuration } from '../motion/motion.svelte.js';

	interface Props {
		text: string;
		placement?: 'top' | 'bottom';
		delay?: number;
		children?: Snippet;
		class?: string;
	}

	let { text, placement = 'top', delay = 350, children, class: className = '' }: Props = $props();

	const id = $props.id();
	let visible = $state(false);
	let timer: ReturnType<typeof setTimeout> | undefined;

	function show(immediate = false) {
		clearTimeout(timer);
		if (immediate) visible = true;
		else timer = setTimeout(() => (visible = true), delay);
	}
	function hide() {
		clearTimeout(timer);
		visible = false;
	}
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -- the wrapper is a
     hover/focus proxy for the interactive trigger it wraps; the tooltip is
     equally reachable via keyboard focus (focusin/focusout). -->
<span
	class="wrap {className}"
	onmouseenter={() => show()}
	onmouseleave={hide}
	onfocusin={() => show(true)}
	onfocusout={hide}
	onkeydown={(e) => {
		if (e.key === 'Escape') hide();
	}}
	aria-describedby={visible ? id : undefined}
>
	{@render children?.()}
	{#if visible}
		<span
			class="bubble ds-glass ds-glass--thick"
			data-placement={placement}
			role="tooltip"
			{id}
			transition:scale={{ start: 0.86, duration: safeDuration(200), easing: backOut }}
		>
			{text}
		</span>
	{/if}
</span>

<style>
	.wrap {
		position: relative;
		display: inline-flex;
	}

	.bubble {
		position: absolute;
		left: 50%;
		translate: -50% 0;
		width: max-content;
		max-width: 16rem;
		padding: var(--space-2) var(--space-4);
		border-radius: var(--radius-squircle-sm);
		font-size: var(--text-footnote);
		font-weight: var(--weight-medium);
		color: var(--color-text-primary);
		box-shadow: var(--elevation-glass-2);
		pointer-events: none;
		z-index: 50;
	}
	.bubble[data-placement='top'] {
		bottom: calc(100% + var(--space-3));
	}
	.bubble[data-placement='bottom'] {
		top: calc(100% + var(--space-3));
	}
</style>
