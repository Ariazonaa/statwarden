<script lang="ts">
	/**
	 * A short, ranked list of monitors with one metric each.
	 *
	 * Three widgets share this shape — "needs attention", "lowest uptime",
	 * "slowest" — and they differ only in how they pick and sort. Keeping one
	 * component means the three cannot drift apart in row height, empty state or
	 * link behaviour, which is exactly the kind of drift nobody notices until the
	 * dashboard looks assembled from parts.
	 *
	 * The list is derived, never fetched: every field comes from the monitors
	 * store, which the WebSocket already keeps live. A widget that polled would
	 * put the dashboard's refresh rate back under the operator's nose.
	 */
	import type { Monitor } from '$lib/api';
	import { t } from '$lib/i18n';
	import { EmptyState, Sparkline, StatusBadge } from '$lib/design-system';

	/* Plain <a href>, no click handler. SvelteKit's client router already
	   intercepts an internal link; calling preventDefault and goto() by hand does
	   the same navigation but also swallows ctrl-click and middle-click, so the
	   row silently loses "open in a new tab". */
	interface Props {
		monitors: Monitor[];
		/** Empty-state line when nothing qualifies. */
		emptyText: string;
		/** What to show on the right of each row. */
		metric: 'uptime' | 'latency' | 'status';
		/** Rows to show. */
		limit?: number;
		/** Draw the trailing latency series behind the row. */
		sparkline?: boolean;
	}

	let { monitors, emptyText, metric, limit = 6, sparkline = false }: Props = $props();

	const rows = $derived(monitors.slice(0, limit));

	function metricText(m: Monitor): string {
		if (metric === 'uptime') {
			// undefined means the window held no conclusive heartbeat — a real and
			// different answer from 0%, so it is not rendered as a number.
			return m.uptime24h === undefined ? '—' : `${m.uptime24h.toFixed(2)}%`;
		}
		if (metric === 'latency') {
			return m.latencyMs === undefined ? '—' : `${m.latencyMs} ms`;
		}
		return '';
	}
</script>

{#if rows.length === 0}
	<EmptyState title={emptyText} />
{:else}
	<ul class="rows">
		{#each rows as m (m.id)}
			<li>
				<a href="/monitors/{m.id}">
					<StatusBadge status={m.status} size="sm" />
					<span class="name" title={m.name}>{m.name}</span>
					{#if sparkline && m.sparkline.length > 1}
						<span class="trend">
							<Sparkline
								data={m.sparkline}
								color={m.status}
								height={18}
								label={t('monitorCard.latencyTrend', { name: m.name })}
							/>
						</span>
					{/if}
					{#if metric !== 'status'}
						<span class="metric">{metricText(m)}</span>
					{/if}
				</a>
			</li>
		{/each}
	</ul>
	{#if monitors.length > rows.length}
		<p class="more">{t('widget.andMore', { n: monitors.length - rows.length })}</p>
	{/if}
{/if}

<style>
	.rows {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.rows li + li {
		border-top: 1px solid var(--hairline);
	}
	a {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-3) 0;
		color: inherit;
		text-decoration: none;
		min-width: 0;
	}
	a:hover .name {
		color: var(--color-accent);
	}
	.name {
		flex: 1 1 auto;
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		font-size: var(--text-subheadline);
		color: var(--color-text-primary);
	}
	/* Sparkline measures its host, so the wrapper needs a definite width; without
	   one it renders at zero and the row silently loses its trend line. Named
	   `.trend` rather than `.spark` only so the wrapper and the component's own
	   root are distinguishable when reading the DOM. */
	.trend {
		flex: 0 0 4rem;
		width: 4rem;
		opacity: 0.75;
	}
	.metric {
		flex: 0 0 auto;
		font-variant-numeric: tabular-nums;
		font-size: var(--text-footnote);
		color: var(--color-text-secondary);
	}
	.more {
		margin: var(--space-3) 0 0;
		font-size: var(--text-caption-1);
		color: var(--color-text-tertiary);
	}
</style>
