<script lang="ts">
	// Designed empty state with icon, copy and call-to-action slot.
	import type { Snippet } from 'svelte';

	interface Props {
		title: string;
		description?: string;
		icon?: Snippet;
		action?: Snippet;
		class?: string;
	}

	let { title, description, icon, action, class: className = '' }: Props = $props();
</script>

<div class="empty {className}">
	{#if icon}
		<div class="icon" aria-hidden="true">{@render icon()}</div>
	{/if}
	<h3 class="title">{title}</h3>
	{#if description}
		<p class="desc">{description}</p>
	{/if}
	{#if action}
		<div class="action">{@render action()}</div>
	{/if}
</div>

<style>
	.empty {
		display: flex;
		flex-direction: column;
		align-items: center;
		text-align: center;
		padding: var(--space-10) var(--space-7);
	}

	.icon {
		display: grid;
		place-items: center;
		width: 3.5rem;
		height: 3.5rem;
		margin-bottom: var(--space-5);
		border-radius: var(--radius-squircle-sm);
		color: var(--color-accent);
		background: var(--color-accent-tint);
		box-shadow: inset 0 1px 0 var(--glass-highlight);
	}
	.icon :global(svg) {
		width: 1.75rem;
		height: 1.75rem;
	}

	.title {
		margin: 0;
		font-size: var(--text-title-3);
		font-weight: var(--weight-semibold);
		letter-spacing: var(--tracking-tight);
	}

	.desc {
		margin: var(--space-2) 0 0;
		max-width: 28rem;
		font-size: var(--text-subheadline);
		line-height: var(--leading-relaxed);
		color: var(--color-text-secondary);
	}

	.action {
		margin-top: var(--space-6);
	}
</style>
