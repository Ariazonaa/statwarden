<script lang="ts">
	/**
	 * TLS certificates by remaining validity, soonest first.
	 *
	 * The point of the tile is the ONE certificate that is about to lapse, so the
	 * list is sorted by urgency rather than by monitor, and already-expired rows
	 * sort to the very top: an expired certificate is not "0 days left", it is an
	 * outage that is already happening and that a percentage-based uptime figure
	 * will not show, because the endpoint is answering perfectly well to anything
	 * that skips verification.
	 *
	 * Only monitors whose last check actually inspected a certificate appear here
	 * — the server derives the list from `cert_info`, so a plain TCP monitor is
	 * absent rather than listed as "unknown".
	 */
	import type { CertificateRow } from '$lib/api';
	import { t } from '$lib/i18n';
	import { EmptyState, Skeleton } from '$lib/design-system';

	interface Props {
		rows: CertificateRow[];
		loaded: boolean;
		/** True when the fetch failed — which is not the same as "nothing here". */
		failed?: boolean;
		limit?: number;
	}
	let { rows, loaded, failed = false, limit = 6 }: Props = $props();

	const sorted = $derived([...rows].sort((a, b) => a.daysRemaining - b.daysRemaining));
	const shown = $derived(sorted.slice(0, limit));

	/**
	 * Urgency band, and the only place the thresholds live.
	 *
	 * They mirror the certificate-expiry alert defaults (21/14/7/3/1 days) so the
	 * tile turns amber at roughly the point the first mail goes out. A tile that
	 * still looked calm while an alert was already in someone's inbox would teach
	 * people to distrust it.
	 */
	function band(days: number): 'expired' | 'critical' | 'warn' | 'ok' {
		if (days < 0) return 'expired';
		if (days <= 7) return 'critical';
		if (days <= 21) return 'warn';
		return 'ok';
	}

	function remainingLabel(row: CertificateRow): string {
		if (row.expired || row.daysRemaining < 0) return t('widget.certExpired');
		if (row.daysRemaining === 0) return t('widget.certToday');
		return t('widget.certDays', { n: row.daysRemaining });
	}
</script>

{#if failed}
	<EmptyState title={t('widget.certUnavailable')} />
{:else if !loaded}
	<Skeleton lines={4} />
{:else if shown.length === 0}
	<EmptyState title={t('widget.certEmpty')} />
{:else}
	<ul class="rows">
		{#each shown as c (c.monitorId)}
			<li>
				<a href="/monitors/{c.monitorId}">
					<span class="txt">
						<span class="name" title={c.monitorName}>{c.monitorName}</span>
						{#if c.subject}<span class="sub" title={c.subject}>{c.subject}</span>{/if}
					</span>
					<span class="days" data-band={band(c.daysRemaining)}>{remainingLabel(c)}</span>
				</a>
			</li>
		{/each}
	</ul>
	{#if sorted.length > shown.length}
		<p class="more">{t('widget.andMore', { n: sorted.length - shown.length })}</p>
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
	.txt {
		display: flex;
		flex-direction: column;
		min-width: 0;
		flex: 1 1 auto;
	}
	.name,
	.sub {
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.name {
		font-size: var(--text-subheadline);
		color: var(--color-text-primary);
	}
	.sub {
		font-size: var(--text-caption-1);
		color: var(--color-text-tertiary);
	}
	.days {
		flex: 0 0 auto;
		font-variant-numeric: tabular-nums;
		font-size: var(--text-footnote);
		color: var(--color-text-secondary);
	}
	/* Colour is a second signal, never the only one: the cell also says how many
	   days are left in words, so the row still reads correctly in monochrome and
	   to anyone who cannot separate the two warm hues. */
	.days[data-band='warn'] {
		color: var(--status-pending);
	}
	.days[data-band='critical'],
	.days[data-band='expired'] {
		color: var(--status-down);
		font-weight: var(--weight-semibold);
	}
	.more {
		margin: var(--space-3) 0 0;
		font-size: var(--text-caption-1);
		color: var(--color-text-tertiary);
	}
</style>
