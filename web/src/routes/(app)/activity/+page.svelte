<script lang="ts">
	import { onMount } from 'svelte';
	import { api, auditLogExportUrl, type AuditEntry } from '$lib/api';
	import RelativeTime from '$lib/components/RelativeTime.svelte';
	import { session } from '$lib/stores/session.svelte';
	import { t } from '$lib/i18n';
	import { EmptyState, GlassPanel, PillSearchField, Select, Skeleton } from '$lib/design-system';

	let items = $state<AuditEntry[]>([]);
	let loaded = $state(false);
	let query = $state('');

	// ---- CSV export ----------------------------------------------------------
	//
	// A same-origin download, so it is an <a download> pointing at the endpoint
	// rather than a fetch: the server streams the file with a Content-Disposition
	// name and the browser saves it. Nothing is held in memory here, which matters
	// because the export can run to tens of thousands of rows.
	//
	// The window is a plain choice of "how far back", defaulting to everything.
	// The endpoint takes RFC3339 bounds and caps the row count rather than the
	// span, so "everything" is a real answer here and not a way to hang the server.
	let exportRange = $state('all');
	const exportRanges = $derived([
		{ value: 'all', label: t('activity.exportAll') },
		{ value: '7d', label: t('activity.exportRange7d') },
		{ value: '30d', label: t('activity.exportRange30d') },
		{ value: '90d', label: t('activity.exportRange90d') },
		{ value: '1y', label: t('activity.exportRange1y') }
	]);
	const rangeDays: Record<string, number> = { '7d': 7, '30d': 30, '90d': 90, '1y': 365 };
	const exportHref = $derived.by(() => {
		const days = rangeDays[exportRange];
		if (days === undefined) return auditLogExportUrl();
		const from = new Date(Date.now() - days * 24 * 60 * 60 * 1000).toISOString();
		return auditLogExportUrl({ from });
	});

	onMount(async () => {
		items = await api.auditLog();
		loaded = true;
	});

	const filtered = $derived.by(() => {
		const q = query.trim().toLowerCase();
		if (!q) return items;
		return items.filter(
			(e) =>
				e.actor.toLowerCase().includes(q) ||
				e.action.toLowerCase().includes(q) ||
				e.target.toLowerCase().includes(q)
		);
	});

	const icon = (type: string) =>
		type === 'monitor'
			? 'M3 12h4l2.5-6 4 12 2.5-6h5'
			: type === 'user'
				? 'M16 20v-2a4 4 0 0 0-4-4H8a4 4 0 0 0-4 4v2M10 10a3 3 0 1 0 0-6 3 3 0 0 0 0 6Z'
				: 'M4 4h16v16H4zM4 9h16';

	const verb = (action: string) => action.split('.').join(' ').replace(/_/g, ' ');
</script>

<svelte:head><title>{t('activity.pageTitle')}</title></svelte:head>

<div class="pagehead">
	<p class="lead">{t('activity.lead')}</p>
	<div class="headtools">
		<PillSearchField bind:value={query} placeholder={t('activity.filterPlaceholder')} />
		<!-- Admin-only, matching the endpoint: a viewer gets a 403 from it, and an
		     always-visible button that always fails is worse than no button. -->
		{#if session.isAdmin}
			<Select
				value={exportRange}
				options={exportRanges}
				label={t('activity.exportRange')}
				onchange={(v) => (exportRange = String(v))}
			/>
			<!-- A real <a href download>, not a PillButton: that renders a <button>,
			     which cannot carry href. Styled to match, the same way the settings
			     screen renders its config and SLA downloads. -->
			<a class="dl" href={exportHref} download title={t('activity.exportHint')}>
				{t('activity.export')}
			</a>
		{/if}
	</div>
</div>

{#if !loaded}
	<Skeleton lines={6} />
{:else if filtered.length === 0}
	<GlassPanel radius="lg" class="act-wrap">
		<EmptyState title={t('activity.emptyTitle')} description={t('activity.emptyDescription')} />
	</GlassPanel>
{:else}
	<GlassPanel radius="lg" class="act-feed">
		<ol>
			{#each filtered as e (e.id)}
				<li>
					<span class="ico" aria-hidden="true">
						<svg viewBox="0 0 24 24"
							><path
								d={icon(e.targetType)}
								fill="none"
								stroke="currentColor"
								stroke-width="1.7"
								stroke-linecap="round"
								stroke-linejoin="round"
							/></svg
						>
					</span>
					<div class="body">
						<span class="line">
							<strong>{e.actor}</strong>
							{verb(e.action)}
							<span class="target">{e.target}</span>
						</span>
						<RelativeTime value={e.at} class="act-ts" />
					</div>
				</li>
			{/each}
		</ol>
	</GlassPanel>
{/if}

<style>
	.pagehead {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-4);
		flex-wrap: wrap;
		margin-bottom: var(--space-5);
	}
	.lead {
		margin: 0;
		color: var(--color-text-secondary);
		font-size: var(--text-subheadline);
	}
	.headtools {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		flex-wrap: wrap;
	}
	/* Matches the download links on the settings screen: PillButton renders a
	   <button>, which cannot carry href, so a download is a styled anchor. */
	.dl {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		white-space: nowrap;
		min-height: 1.875rem;
		padding: var(--space-2) var(--space-4);
		border-radius: var(--radius-pill);
		font-size: var(--text-footnote);
		font-weight: var(--weight-semibold);
		color: var(--color-text-primary);
		background: var(--glass-bg-regular);
		border: 1px solid var(--glass-border);
		box-shadow: var(--elevation-glass-1);
		text-decoration: none;
		transition:
			border-color var(--duration-sm) ease,
			transform var(--duration-md) var(--ease-spring);
	}
	.dl:hover {
		border-color: var(--glass-border-strong);
		transform: scale(1.03);
	}
	.dl:active {
		transform: scale(0.96);
	}
	@media (prefers-reduced-motion: reduce) {
		.dl {
			transition: none;
		}
		.dl:hover,
		.dl:active {
			transform: none;
		}
	}
	:global(.act-wrap) {
		padding: var(--space-5);
	}
	:global(.act-feed) {
		padding: var(--space-4) var(--space-6);
	}
	ol {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	li {
		display: flex;
		align-items: flex-start;
		gap: var(--space-4);
		padding: var(--space-4) 0;
		border-bottom: 1px solid var(--hairline);
	}
	li:last-child {
		border-bottom: 0;
	}
	.ico {
		display: grid;
		place-items: center;
		width: 2rem;
		height: 2rem;
		flex-shrink: 0;
		border-radius: var(--radius-squircle-sm);
		background: var(--color-accent-tint);
		color: var(--color-accent);
	}
	.ico svg {
		width: 1.1rem;
		height: 1.1rem;
	}
	.body {
		display: flex;
		flex-direction: column;
		gap: 2px;
	}
	.line {
		font-size: var(--text-subheadline);
		color: var(--color-text-secondary);
	}
	.line strong {
		color: var(--color-text-primary);
	}
	.target {
		color: var(--color-accent);
	}
	:global(.act-ts) {
		font-size: var(--text-caption-1);
		color: var(--color-text-tertiary);
	}
</style>
