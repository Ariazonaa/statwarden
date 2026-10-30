<script lang="ts">
	// Glass tab strip with proper tablist semantics, roving tabindex and
	// a spring-gliding active indicator. The panel renders the `panel`
	// snippet with the active tab id.
	import type { Snippet } from 'svelte';
	import { createSpring } from '../motion/motion.svelte.js';

	interface Tab {
		id: string;
		label: string;
	}

	interface Props {
		tabs: Tab[];
		active?: string;
		onchange?: (id: string) => void;
		label?: string;
		panel?: Snippet<[string]>;
		class?: string;
	}

	let {
		tabs,
		active = $bindable(tabs[0]?.id ?? ''),
		onchange,
		label,
		panel,
		class: className = ''
	}: Props = $props();

	const baseId = $props.id();
	let stripEl: HTMLElement | undefined = $state();
	const indicator = createSpring({ x: 0, w: 0 }, 'snappy');
	let measured = $state(false);

	function measure(animate: boolean) {
		if (!stripEl) return;
		const el = stripEl.querySelector<HTMLElement>(`[data-id="${CSS.escape(active)}"]`);
		if (!el) return;
		const target = { x: el.offsetLeft, w: el.offsetWidth };
		if (animate) indicator.target = target;
		else indicator.set(target, { instant: true });
		measured = true;
	}

	$effect(() => {
		void active;
		void tabs;
		measure(measured);
	});

	function select(id: string) {
		if (id === active) return;
		active = id;
		onchange?.(id);
	}

	function onkeydown(e: KeyboardEvent) {
		const idx = tabs.findIndex((t) => t.id === active);
		let next = -1;
		if (e.key === 'ArrowRight') next = (idx + 1) % tabs.length;
		else if (e.key === 'ArrowLeft') next = (idx - 1 + tabs.length) % tabs.length;
		else if (e.key === 'Home') next = 0;
		else if (e.key === 'End') next = tabs.length - 1;
		if (next >= 0) {
			e.preventDefault();
			select(tabs[next].id);
			stripEl?.querySelector<HTMLElement>(`[data-id="${CSS.escape(tabs[next].id)}"]`)?.focus();
		}
	}
</script>

<svelte:window onresize={() => measure(false)} />

<div class={className}>
	<!-- svelte-ignore a11y_interactive_supports_focus -- roving tabindex: focus
	     lives on the active tab; the tablist only delegates arrow keys. -->
	<div
		class="strip ds-glass ds-glass--thin"
		role="tablist"
		aria-label={label}
		bind:this={stripEl}
		{onkeydown}
	>
		<span
			class="indicator"
			class:visible={measured}
			style="transform: translateX({indicator.current.x}px); width: {indicator.current.w}px"
			aria-hidden="true"
		></span>
		{#each tabs as tab (tab.id)}
			<button
				class="tab ds-focusable"
				role="tab"
				id="{baseId}-tab-{tab.id}"
				data-id={tab.id}
				aria-selected={active === tab.id}
				aria-controls={panel ? `${baseId}-panel-${tab.id}` : undefined}
				class:active={active === tab.id}
				tabindex={active === tab.id ? 0 : -1}
				onclick={() => select(tab.id)}
			>
				{tab.label}
			</button>
		{/each}
	</div>

	{#if panel}
		<div
			class="panel"
			role="tabpanel"
			id="{baseId}-panel-{active}"
			aria-labelledby="{baseId}-tab-{active}"
			tabindex="0"
		>
			{@render panel(active)}
		</div>
	{/if}
</div>

<style>
	.strip {
		position: relative;
		display: inline-flex;
		gap: var(--space-1);
		padding: var(--space-1);
		border-radius: var(--radius-pill);
	}

	.indicator {
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
	.indicator.visible {
		opacity: 1;
	}

	.tab {
		position: relative;
		z-index: 1;
		appearance: none;
		border: 0;
		background: transparent;
		font-family: inherit;
		font-size: var(--text-subheadline);
		font-weight: var(--weight-medium);
		color: var(--color-text-secondary);
		padding: var(--space-2) var(--space-5);
		border-radius: var(--radius-pill);
		cursor: pointer;
		-webkit-tap-highlight-color: transparent;
		transition: color var(--duration-md) ease;
	}
	.tab:hover {
		color: var(--color-text-primary);
	}
	.tab.active {
		color: var(--color-text-primary);
		font-weight: var(--weight-semibold);
	}
	.tab:focus-visible {
		outline: none;
		box-shadow: var(--focus-ring);
	}

	.panel {
		margin-top: var(--space-5);
		border-radius: var(--radius-squircle-sm);
	}
	.panel:focus-visible {
		outline: none;
		box-shadow: var(--focus-ring);
	}
</style>
