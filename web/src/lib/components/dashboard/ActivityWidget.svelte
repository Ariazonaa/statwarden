<script lang="ts">
	/**
	 * The last few audit entries, as a compact feed.
	 *
	 * Admin-only, because the endpoint behind it is: GET /audit-log answers 403
	 * for anyone else. The dashboard skips this widget for a non-admin rather than
	 * rendering it and letting the fetch fail — a permanent error tile is worse
	 * than an absent one — but it is not stripped from the stored layout, so an
	 * admin who is temporarily demoted finds their dashboard as they left it.
	 */
	import type { AuditEntry } from '$lib/api';
	import RelativeTime from '$lib/components/RelativeTime.svelte';
	import { t } from '$lib/i18n';
	import { EmptyState, Skeleton } from '$lib/design-system';

	interface Props {
		entries: AuditEntry[];
		loaded: boolean;
		/** True when the fetch failed — which is not the same as "nothing here". */
		failed?: boolean;
		limit?: number;
	}
	let { entries, loaded, failed = false, limit = 6 }: Props = $props();

	const rows = $derived(entries.slice(0, limit));
	// "monitor.create" reads better as "monitor create" in a one-line feed; the
	// activity screen does the same, and the two must not diverge.
	const verb = (action: string) => action.split('.').join(' ').replace(/_/g, ' ');
</script>

{#if failed}
	<EmptyState title={t('widget.activityUnavailable')} />
{:else if !loaded}
	<Skeleton lines={4} />
{:else if rows.length === 0}
	<EmptyState title={t('activity.emptyTitle')} />
{:else}
	<ul class="rows">
		{#each rows as e (e.id)}
			<li>
				<span class="line">
					<strong>{e.actor}</strong>
					{verb(e.action)}
					{#if e.target}<span class="target">{e.target}</span>{/if}
				</span>
				<RelativeTime value={e.at} class="wts" />
			</li>
		{/each}
	</ul>
	<a class="all" href="/activity">{t('widget.activityAll')}</a>
{/if}

<style>
	.rows {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.rows li {
		display: flex;
		align-items: baseline;
		gap: var(--space-3);
		padding: var(--space-2) 0;
		min-width: 0;
	}
	.rows li + li {
		border-top: 1px solid var(--hairline);
	}
	.line {
		flex: 1 1 auto;
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		font-size: var(--text-footnote);
		color: var(--color-text-secondary);
	}
	.line strong {
		color: var(--color-text-primary);
	}
	.target {
		color: var(--color-accent);
	}
	/* `.wts`, not `.ts`: RelativeTime renders in ITS own component, so styling it
	   needs :global — and a :global rule is document-wide. The activity screen
	   already claims :global(.act-ts) for its own feed, and two components silently
	   restyling each other's timestamps is a collision waiting to be debugged. */
	:global(.wts) {
		flex: 0 0 auto;
		font-size: var(--text-caption-1);
		color: var(--color-text-tertiary);
	}
	/* An <a>, not a <button>: this navigates, so it has to behave like a link —
	   ctrl-click, middle-click, "copy link address". */
	.all {
		display: inline-block;
		margin-top: var(--space-3);
		font-size: var(--text-caption-1);
		color: var(--color-accent);
		text-decoration: none;
	}
	.all:hover {
		text-decoration: underline;
	}
</style>
