<script lang="ts">
	// Content card: GlassPanel + generous squircle rounding + padding,
	// with optional title/subtitle header row.
	import type { Snippet } from 'svelte';
	import GlassPanel from './GlassPanel.svelte';

	interface Props {
		title?: string;
		subtitle?: string;
		blur?: 'thin' | 'regular' | 'thick';
		/** Slight lift on hover for clickable cards. */
		interactive?: boolean;
		padding?: 'none' | 'sm' | 'md' | 'lg';
		/** Rendered top-right of the header row. */
		actions?: Snippet;
		children?: Snippet;
		class?: string;
	}

	let {
		title,
		subtitle,
		blur = 'regular',
		interactive = false,
		padding = 'md',
		actions,
		children,
		class: className = ''
	}: Props = $props();
</script>

<GlassPanel {blur} {interactive} radius="lg" class="card pad-{padding} {className}">
	{#if title || subtitle || actions}
		<header class="head">
			<div class="titles">
				{#if title}<h3 class="title">{title}</h3>{/if}
				{#if subtitle}<p class="subtitle">{subtitle}</p>{/if}
			</div>
			{#if actions}<div class="actions">{@render actions()}</div>{/if}
		</header>
	{/if}
	{@render children?.()}
</GlassPanel>

<style>
	:global(.card.pad-sm) {
		padding: var(--space-4);
	}
	:global(.card.pad-md) {
		padding: var(--space-6);
	}
	:global(.card.pad-lg) {
		padding: var(--space-8);
	}

	.head {
		display: flex;
		align-items: flex-start;
		justify-content: space-between;
		gap: var(--space-4);
		margin-bottom: var(--space-4);
	}
	.title {
		margin: 0;
		font-size: var(--text-headline);
		font-weight: var(--weight-semibold);
		letter-spacing: var(--tracking-normal);
	}
	.subtitle {
		margin: var(--space-1) 0 0;
		font-size: var(--text-footnote);
		color: var(--color-text-secondary);
	}
	.actions {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		flex-shrink: 0;
	}
</style>
