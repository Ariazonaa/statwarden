<script lang="ts">
	/**
	 * Renders a post-mortem's Markdown subset.
	 *
	 * Every string below goes through Svelte's normal text interpolation, so it is
	 * escaped. There is no `{@html}` here and there must never be one: the source
	 * is operator-authored text on a page anonymous visitors read. See
	 * ./markdown.ts for why the subset is as small as it is.
	 */
	import { parseBlocks, type Inline } from './markdown';

	interface Props {
		source: string;
		/** Extra class for the wrapper, so a host page can scope its own type scale. */
		class?: string;
	}
	let { source, class: cls = '' }: Props = $props();

	const blocks = $derived(parseBlocks(source));
</script>

{#snippet inline(runs: Inline[])}
	{#each runs as run, i (i)}
		{#if run.kind === 'bold'}<strong>{run.text}</strong>
		{:else if run.kind === 'code'}<code>{run.text}</code>
		{:else}{run.text}{/if}
	{/each}
{/snippet}

<div class="pm {cls}">
	{#each blocks as b, i (i)}
		{#if b.kind === 'heading' && b.level === 1}
			<h3>{@render inline(b.content)}</h3>
		{:else if b.kind === 'heading' && b.level === 2}
			<h4>{@render inline(b.content)}</h4>
		{:else if b.kind === 'heading'}
			<h5>{@render inline(b.content)}</h5>
		{:else if b.kind === 'list'}
			<ul>
				{#each b.items as item, j (j)}
					<li>{@render inline(item)}</li>
				{/each}
			</ul>
		{:else}
			<p>{@render inline(b.content)}</p>
		{/if}
	{/each}
</div>

<style>
	/* The heading levels start at h3 because this is always rendered inside an
	   incident article that already owns h2 — a document that jumped from h2 to
	   h1 would read as a new top-level section to a screen reader. */
	.pm {
		font-size: var(--text-subheadline);
		line-height: 1.6;
		color: var(--color-text-secondary);
	}
	.pm :is(h3, h4, h5) {
		margin: var(--space-4) 0 var(--space-2);
		color: var(--color-text-primary);
		line-height: 1.3;
	}
	.pm > :is(h3, h4, h5):first-child {
		margin-top: 0;
	}
	.pm h3 {
		font-size: var(--text-headline);
	}
	.pm h4 {
		font-size: var(--text-subheadline);
		font-weight: 650;
	}
	.pm h5 {
		font-size: var(--text-footnote);
		font-weight: 650;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		color: var(--color-text-tertiary);
	}
	.pm p {
		margin: 0 0 var(--space-3);
	}
	.pm ul {
		margin: 0 0 var(--space-3);
		padding-left: 1.15rem;
	}
	.pm li {
		margin-bottom: var(--space-1);
	}
	.pm > :last-child {
		margin-bottom: 0;
	}
	.pm strong {
		color: var(--color-text-primary);
		font-weight: 650;
	}
	.pm code {
		font-family: var(--font-mono);
		font-size: 0.92em;
		padding: 0.1em 0.35em;
		border-radius: var(--radius-squircle-sm);
		background: var(--color-accent-tint);
	}
</style>
