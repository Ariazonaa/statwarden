<script lang="ts">
	// Small keyboard-shortcut chip (e.g. ⌘K).
	import type { Snippet } from 'svelte';

	let { children, class: className = '' }: { children?: Snippet; class?: string } = $props();
</script>

<kbd class={className}>{@render children?.()}</kbd>

<style>
	kbd {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		min-width: 1.5rem;
		padding: 0.125rem 0.375rem;
		font-family: var(--font-sans);
		font-size: var(--text-caption-1);
		font-weight: var(--weight-medium);
		color: var(--color-text-secondary);
		background: var(--glass-bg-thin);
		border: 1px solid var(--hairline);
		border-bottom-width: 2px;
		border-radius: 0.4375rem;
		box-shadow: inset 0 1px 0 var(--glass-highlight);
		white-space: nowrap;
	}
</style>
