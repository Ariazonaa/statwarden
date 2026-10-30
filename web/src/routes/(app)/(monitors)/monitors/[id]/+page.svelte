<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import {
		api,
		monitorTypeSpecs,
		problemMessage,
		problemStatus,
		type CertificateRow,
		type Heartbeat,
		type Monitor,
		type MonitorBadge,
		type StateEvent,
		type Tag
	} from '$lib/api';
	import { monitors } from '$lib/stores/monitors.svelte';
	import { session } from '$lib/stores/session.svelte';
	import { ws, type WsEnvelope } from '$lib/ws/store.svelte';
	import RelativeTime from '$lib/components/RelativeTime.svelte';
	import { humanDuration } from '$lib/util/time';
	import { renderMarkdown } from '$lib/util/markdown';
	import { t, formatDateTime, formatNumber } from '$lib/i18n';
	import {
		EmptyState,
		GlassCard,
		HeartbeatBar,
		LineChart,
		Modal,
		PillButton,
		SegmentedControl,
		Skeleton,
		StatusPill,
		toast
	} from '$lib/design-system';

	const id = $derived(page.params.id ?? '');

	let monitor = $state<Monitor | undefined>(undefined);
	let tags = $state<Tag[]>([]);
	let events = $state<StateEvent[]>([]);
	let heartbeats = $state<Heartbeat[]>([]);
	let range = $state<'24h' | '7d' | '30d' | '1y'>('24h');
	let loading = $state(true);
	let loadingChart = $state(false);
	let busy = $state(false);
	let confirmDelete = $state(false);
	// Set when the initial detail fetch fails, so the view can show an escape
	// (back/retry) instead of a skeleton that never resolves.
	let loadError = $state<string | undefined>(undefined);
	// Teardown for the live WS wiring, set once the detail has loaded. Held at
	// component scope so both the mount path and a later retry can install it and
	// the destroy cleanup can tear it down.
	let liveOff: (() => void) | undefined;

	// Embeddable SVG badge. The Monitor resource carries no badge field, so the
	// token is unknown until minted (create is idempotent server-side, returning a
	// stable token for a monitor that already has one). `undefined` = not created
	// yet in this view; `revoke` clears it back.
	let badge = $state<MonitorBadge | undefined>(undefined);
	let badgeBusy = $state(false);
	let confirmRevoke = $state(false);

	function badgeMarkdown(label: string, url: string): string {
		return `![${monitor?.name ?? ''} ${label}](${url})`;
	}

	async function createBadge() {
		badgeBusy = true;
		try {
			badge = await api.createMonitorBadge(id);
			toast.success(t('monitorDetail.badgeCreated'));
		} catch {
			toast.error(t('monitorDetail.badgeCreateError'));
		} finally {
			badgeBusy = false;
		}
	}
	async function revokeBadge() {
		badgeBusy = true;
		try {
			await api.revokeMonitorBadge(id);
			badge = undefined;
			confirmRevoke = false;
			toast.success(t('monitorDetail.badgeRevoked'));
		} catch {
			toast.error(t('monitorDetail.badgeRevokeError'));
		} finally {
			badgeBusy = false;
		}
	}
	async function copyText(text: string) {
		try {
			await navigator.clipboard.writeText(text);
			toast.success(t('monitorDetail.badgeCopied'));
		} catch {
			/* clipboard unavailable (insecure context / denied) — no-op */
		}
	}

	const monitorTags = $derived(tags.filter((t) => monitor?.tagIds.includes(t.id)));
	const typeLabel = $derived.by(() => {
		const spec = monitorTypeSpecs.find((s) => s.type === monitor?.type);
		return spec ? t(spec.label) : (monitor?.type ?? '');
	});

	// The certificate column of the stats row. Read from the instance-wide
	// overview because the Monitor resource carries no certificate; a monitor
	// without one (no TLS, or reporting unavailable) simply has no column.
	let cert = $state<CertificateRow | undefined>(undefined);

	/** Uptime as a percentage, or a dash for "no conclusive check in the window" — never 0. */
	function pct(v: number | undefined): string {
		return v === undefined
			? '—'
			: `${formatNumber(v, { maximumFractionDigits: v === 100 ? 0 : 2 })} %`;
	}
	const fullDate = (at: string) =>
		formatDateTime(at, {
			year: 'numeric',
			month: '2-digit',
			day: '2-digit',
			hour: '2-digit',
			minute: '2-digit',
			second: '2-digit'
		});

	// link_url / url are rendered as an <a href>. A writer could store a
	// "javascript:" (or "data:") URL that runs when an admin clicks it, so only
	// absolute http(s) URLs become a real link; anything else renders as inert
	// text. The server rejects such schemes for link_url too — this is the
	// matching client-side guard, and it also covers the derived url field.
	function safeHref(u: string | undefined): string | undefined {
		if (!u) return undefined;
		try {
			const scheme = new URL(u).protocol;
			return scheme === 'http:' || scheme === 'https:' ? u : undefined;
		} catch {
			return undefined;
		}
	}
	const linkHref = $derived(safeHref(monitor?.externalUrl ?? monitor?.url));

	// Initial fetch of the three resources the view needs. A rejection must not
	// strand the operator on the skeleton, so it is caught and surfaced;
	// `loading` is always cleared. Returns whether the view may proceed.
	//
	// A 404 means the monitor is gone — deleted after the list was fetched, or a
	// hand-typed / bookmarked id (the server still serves index.html for the
	// path, so this view is what has to notice). There is nothing to show, so
	// the operator is taken back to the dashboard with a short note. Every other
	// failure (5xx, network) stays put, where a retry is possible.
	async function loadDetail(): Promise<boolean> {
		try {
			const [m, tg, ev] = await Promise.all([api.getMonitor(id), api.listTags(), api.events(id)]);
			monitor = m;
			tags = tg;
			events = ev;
			loadError = undefined;
			// Not awaited and not fatal: the page is complete without it.
			api
				.certificateOverview()
				.then((rows) => (cert = rows.find((r) => r.monitorId === id)))
				.catch(() => (cert = undefined));
			return true;
		} catch (err) {
			if (problemStatus(err) === 404) {
				// Surface the note AND redirect: if the navigation is cancelled
				// the EmptyState below is the fallback, and a retry re-enters
				// this branch.
				loadError = t('monitor.notFound');
				toast.info(t('monitor.notFound'));
				goto('/dashboard').catch(() => {});
				return false;
			}
			loadError = problemMessage(err) ?? 'Could not load this monitor.';
			return false;
		} finally {
			loading = false;
		}
	}

	// Subscribe to this monitor's live topic for status/latency. Installed once,
	// after the detail is known to exist.
	function startLive() {
		const topic = `monitor:${id}`;
		const offTopic = ws.subscribe(topic);
		const offMsg = ws.on((env: WsEnvelope) => {
			if (env.topic !== topic || !monitor) return;
			if (env.type === 'heartbeat') {
				const hb = env.data as Heartbeat;
				if (hb.latencyMs != null) monitor.latencyMs = hb.latencyMs;
				monitor.lastCheckedAt = hb.checkedAt;
				if (hb.status) monitor.recentBeats = [...monitor.recentBeats.slice(-49), hb.status];
				// A heartbeat carries the live status, so it also reconciles a
				// state_change that was missed (e.g. dropped over a reconnect).
				if (hb.status) monitor.status = hb.status;
			} else if (env.type === 'state_change') {
				monitor.status = (env.data as { to: Monitor['status'] }).to;
			}
		});
		// Frames missed while offline never replay, so pull the detail and its
		// event log back from REST after a reconnect to resync.
		const offReconnect = ws.onReconnect(() => {
			refresh();
			loadChart();
		});
		liveOff = () => {
			offMsg();
			offTopic();
			offReconnect();
		};
	}

	async function retry() {
		loading = true;
		loadError = undefined;
		if (await loadDetail()) {
			await loadChart();
			// Only ever installed once: a retry runs solely after an initial
			// failure, where `startLive` never got to run.
			if (!liveOff) startLive();
		}
	}

	onMount(() => {
		(async () => {
			if (!(await loadDetail())) return;
			await loadChart();
			startLive();
		})();
		return () => liveOff?.();
	});

	async function loadChart() {
		loadingChart = true;
		try {
			heartbeats = await api.heartbeats(id, range);
		} finally {
			loadingChart = false;
		}
	}

	function changeRange(r: string) {
		range = r as typeof range;
		loadChart();
	}

	const chartX = $derived(
		heartbeats.map((h) => Math.floor(new Date(h.checkedAt).getTime() / 1000))
	);
	const chartY = $derived(heartbeats.map((h) => h.latencyMs));
	const stats = $derived.by(() => {
		const vals = heartbeats.map((h) => h.latencyMs).filter((v): v is number => v != null);
		if (vals.length === 0) return { min: 0, max: 0, avg: 0, count: 0 };
		const sum = vals.reduce((a, b) => a + b, 0);
		return {
			count: vals.length,
			min: Math.min(...vals),
			max: Math.max(...vals),
			avg: Math.round(sum / vals.length)
		};
	});

	async function refresh() {
		monitor = await api.getMonitor(id);
		events = await api.events(id);
	}

	async function checkNow() {
		busy = true;
		try {
			await monitors.checkNow(id);
			monitor = monitors.get(id) ?? monitor;
			toast.info(t('monitorDetail.checkTriggered'));
			await loadChart();
		} finally {
			busy = false;
		}
	}
	async function togglePause() {
		busy = true;
		try {
			if (monitor?.active) await monitors.pause(id);
			else await monitors.resume(id);
			monitor = monitors.get(id) ?? monitor;
		} finally {
			busy = false;
		}
	}
	async function del() {
		busy = true;
		try {
			await monitors.delete(id);
			toast.success(t('monitorDetail.deleted'));
			await goto('/dashboard');
		} finally {
			busy = false;
		}
	}
</script>

<svelte:head><title>{monitor?.name ?? t('title.monitor')} · Statwarden</title></svelte:head>

{#if loadError}
	<div class="loaderr ds-glass">
		<EmptyState title={loadError ?? ''}>
			{#snippet action()}
				<PillButton variant="secondary" size="sm" onclick={() => goto('/dashboard')}>
					{t('common.back')}
				</PillButton>
				<PillButton size="sm" onclick={retry}>Retry</PillButton>
			{/snippet}
		</EmptyState>
	</div>
{:else if loading || !monitor}
	<Skeleton variant="rect" height="12rem" />
{:else}
	<!-- Kuma's detail header: name, tags, target, then the actions. -->
	<header class="khead">
		<h1 class="name">{monitor.name}</h1>
		{#if monitorTags.length}
			<div class="tags">
				{#each monitorTags as tag (tag.id)}
					<span class="tag" style="--tag: {tag.color}">{tag.name}</span>
				{/each}
			</div>
		{/if}
		<p class="target">
			{#if monitor.url}{#if linkHref}<a class="url" href={linkHref}>{monitor.url}</a>{:else}<span
						class="url">{monitor.url}</span
					>{/if}{/if}
			<span class="kind">{typeLabel}</span>
		</p>
		{#if session.canEdit}
			<div class="tools">
				<PillButton
					variant={monitor.active ? 'secondary' : 'primary'}
					size="sm"
					disabled={busy}
					onclick={togglePause}
				>
					{monitor.active ? t('monitorDetail.pause') : t('monitorDetail.resume')}
				</PillButton>
				<PillButton variant="secondary" size="sm" onclick={() => goto(`/monitors/${id}/edit`)}>
					{t('monitorDetail.edit')}
				</PillButton>
				<PillButton variant="secondary" size="sm" onclick={() => goto(`/monitors/new?clone=${id}`)}>
					{t('monitorDetail.clone')}
				</PillButton>
				<PillButton variant="secondary" size="sm" disabled={busy} onclick={checkNow}>
					{t('monitorDetail.checkNow')}
				</PillButton>
				<PillButton variant="destructive" size="sm" onclick={() => (confirmDelete = true)}>
					{t('monitorDetail.delete')}
				</PillButton>
			</div>
		{/if}
	</header>

	<GlassCard class="kbeats">
		<div class="beatrow">
			<HeartbeatBar beats={monitor.recentBeats} class="bigbar" />
			<StatusPill status={monitor.status} label={t(`status.${monitor.status}`)} size="lg" />
		</div>
		<p class="every">
			{t('monitorDetail.checkEvery', { interval: humanDuration(monitor.interval) })}
		</p>
	</GlassCard>

	<GlassCard class="kstats">
		<dl class="stats">
			<div class="kpi">
				<dt>{t('monitorDetail.response')}<span>{t('monitorDetail.current')}</span></dt>
				<dd>{monitor.latencyMs != null ? `${monitor.latencyMs} ms` : '—'}</dd>
			</div>
			<div class="kpi">
				<dt>{t('monitorDetail.avgResponse')}<span>{range}</span></dt>
				<dd>{stats.count > 0 ? `${stats.avg} ms` : '—'}</dd>
			</div>
			<div class="kpi">
				<dt>{t('monitorDetail.uptime')}<span>{t('monitorDetail.period24h')}</span></dt>
				<dd>{pct(monitor.uptime24h)}</dd>
			</div>
			<div class="kpi">
				<dt>{t('monitorDetail.uptime')}<span>{t('monitorDetail.period30d')}</span></dt>
				<dd>{pct(monitor.uptime30d)}</dd>
			</div>
			<div class="kpi">
				<dt>{t('monitorDetail.uptime')}<span>{t('monitorDetail.period1y')}</span></dt>
				<dd>{pct(monitor.uptime1y)}</dd>
			</div>
			{#if cert}
				<div class="kpi">
					<dt>
						{t('monitorDetail.certExpiry')}<span
							>{formatDateTime(cert.validTo, {
								year: 'numeric',
								month: '2-digit',
								day: '2-digit'
							})}</span
						>
					</dt>
					<dd
						class:warn={cert.daysRemaining <= 21}
						class:bad={cert.expired || cert.daysRemaining <= 7}
					>
						{cert.expired
							? t('monitorDetail.certExpired')
							: t('monitorDetail.certDays', { n: cert.daysRemaining })}
					</dd>
				</div>
			{/if}
		</dl>
	</GlassCard>

	<GlassCard title={t('monitorDetail.responseTime')} class="chartcard">
		{#snippet actions()}
			<SegmentedControl
				size="sm"
				segments={[
					{ value: '24h', label: '24h' },
					{ value: '7d', label: '7d' },
					{ value: '30d', label: '30d' },
					{ value: '1y', label: '1y' }
				]}
				value={range}
				onchange={changeRange}
				label={t('monitorDetail.timeRange')}
			/>
		{/snippet}

		<!-- min/max/avg overlay -->
		<div class="overlay">
			<span class="o min">{t('monitorDetail.min')} <b>{stats.min} ms</b></span>
			<span class="o avg">{t('monitorDetail.avg')} <b>{stats.avg} ms</b></span>
			<span class="o max">{t('monitorDetail.max')} <b>{stats.max} ms</b></span>
		</div>

		{#if loadingChart}
			<Skeleton variant="rect" height="14rem" />
		{:else if heartbeats.length === 0}
			<p class="muted">{t('monitorDetail.noDataRange')}</p>
		{:else}
			<LineChart x={chartX} y={chartY} label={t('monitorDetail.responseTime')} height={240} />
		{/if}
	</GlassCard>

	<div class="cols">
		<!-- Kuma's event table: status, when, what the check said. The duration
		     of the state that ended is kept from the old timeline — it is the
		     one number an operator asks first after an outage. -->
		<GlassCard title={t('monitorDetail.importantEvents')} class="col wide">
			{#if events.length === 0}
				<p class="muted">{t('monitorDetail.noTransitions')}</p>
			{:else}
				<div class="evscroll">
					<table class="evtable">
						<thead>
							<tr>
								<th scope="col">{t('events.colStatus')}</th>
								<th scope="col">{t('events.colTime')}</th>
								<th scope="col">{t('events.colMessage')}</th>
							</tr>
						</thead>
						<tbody>
							{#each events as ev (ev.id)}
								<tr>
									<td><StatusPill status={ev.to} label={t(`status.${ev.to}`)} /></td>
									<td class="time">{fullDate(ev.at)}</td>
									<td class="msg">
										{ev.message ??
											(ev.from
												? '—'
												: t('monitorDetail.created', { status: t(`status.${ev.to}`) }))}
										{#if ev.durationSec && ev.from}<span class="lasted"
												>{t('monitorDetail.lastedPrev', {
													status: t(`status.${ev.from}`),
													duration: humanDuration(ev.durationSec)
												})}</span
											>{/if}
									</td>
								</tr>
							{/each}
						</tbody>
					</table>
				</div>
			{/if}
		</GlassCard>

		<GlassCard title={t('monitorDetail.configuration')} class="col">
			<dl class="config">
				<dt>{t('monitorDetail.type')}</dt>
				<dd>{monitor.type}</dd>
				<dt>{t('monitorDetail.interval')}</dt>
				<dd>{humanDuration(monitor.interval)}</dd>
				<dt>{t('monitorDetail.timeout')}</dt>
				<dd>{humanDuration(monitor.timeout)}</dd>
				<dt>{t('monitorDetail.retries')}</dt>
				<dd>{monitor.maxRetries}</dd>
				<dt>{t('monitorDetail.recoveryChecks')}</dt>
				<dd>{monitor.recoveryChecks}</dd>
				<dt>{t('monitorDetail.upsideDown')}</dt>
				<dd>{monitor.upsideDown ? t('common.yes') : t('common.no')}</dd>
				<dt>{t('monitorDetail.tags')}</dt>
				<dd>
					{#if monitorTags.length}
						<span class="tags">
							{#each monitorTags as tag (tag.id)}
								<span class="tag" style="--tag: {tag.color}">{tag.name}</span>
							{/each}
						</span>
					{:else}—{/if}
				</dd>
				<dt>{t('monitorDetail.createdAt')}</dt>
				<dd><RelativeTime value={monitor.createdAt} /></dd>
			</dl>
		</GlassCard>
	</div>

	{#if monitor.description}
		<GlassCard title={t('monitorDetail.notes')}>
			<!-- {@html} is safe here BECAUSE of renderMarkdown, not despite it: the
			     renderer escapes every input character before emitting any markup and
			     supports no raw-HTML or link syntax, so nothing an author types can
			     become a tag or an href. Never swap it for a general Markdown
			     library without a sanitiser — a description is written by a writer
			     and read by viewers. See $lib/util/markdown.ts. -->
			<div class="md">{@html renderMarkdown(monitor.description)}</div>
		</GlassCard>
	{/if}

	{#if session.canEdit}
		{#snippet badgeBlock(label: string, url: string)}
			<div class="badge-block">
				<div class="badge-head">
					<span class="badge-label">{label}</span>
					<img class="badge-preview" src={url} alt={label} />
				</div>
				<div class="badge-field">
					<span class="badge-key">{t('monitorDetail.badgeUrl')}</span>
					<code class="badge-val">{url}</code>
					<PillButton variant="secondary" size="sm" onclick={() => copyText(url)}>
						{t('monitorDetail.badgeCopyUrl')}
					</PillButton>
				</div>
				<div class="badge-field">
					<span class="badge-key">{t('monitorDetail.badgeMarkdown')}</span>
					<code class="badge-val">{badgeMarkdown(label, url)}</code>
					<PillButton
						variant="secondary"
						size="sm"
						onclick={() => copyText(badgeMarkdown(label, url))}
					>
						{t('monitorDetail.badgeCopyMarkdown')}
					</PillButton>
				</div>
			</div>
		{/snippet}

		<GlassCard title={t('monitorDetail.badge')} subtitle={t('monitorDetail.badgeSub')}>
			{#snippet actions()}
				{#if badge}
					<PillButton
						variant="destructive"
						size="sm"
						disabled={badgeBusy}
						onclick={() => (confirmRevoke = true)}
					>
						{t('monitorDetail.badgeRevoke')}
					</PillButton>
				{:else}
					<PillButton variant="primary" size="sm" disabled={badgeBusy} onclick={createBadge}>
						{t('monitorDetail.badgeCreate')}
					</PillButton>
				{/if}
			{/snippet}

			{#if badge}
				<div class="badges">
					{@render badgeBlock(t('monitorDetail.badgeStatusLabel'), badge.statusUrl)}
					{@render badgeBlock(t('monitorDetail.badgeUptimeLabel'), badge.uptimeUrl)}
				</div>
			{/if}
		</GlassCard>
	{/if}
{/if}

<Modal bind:open={confirmDelete} title={t('monitorDetail.deleteTitle')}>
	<p style="margin:0">{t('monitorDetail.deleteBody')}</p>
	{#snippet footer()}
		<PillButton variant="ghost" onclick={() => (confirmDelete = false)}
			>{t('common.cancel')}</PillButton
		>
		<PillButton variant="destructive" disabled={busy} onclick={del}>{t('common.delete')}</PillButton
		>
	{/snippet}
</Modal>

<Modal bind:open={confirmRevoke} title={t('monitorDetail.badgeRevokeTitle')}>
	<p style="margin:0">{t('monitorDetail.badgeRevokeBody')}</p>
	{#snippet footer()}
		<PillButton variant="ghost" onclick={() => (confirmRevoke = false)}
			>{t('common.cancel')}</PillButton
		>
		<PillButton variant="destructive" disabled={badgeBusy} onclick={revokeBadge}
			>{t('monitorDetail.badgeRevoke')}</PillButton
		>
	{/snippet}
</Modal>

<style>
	.loaderr {
		border-radius: var(--radius-squircle-lg);
		padding: var(--space-6);
	}
	/* EmptyState renders its CTA in a child .action; reach it to space the pair. */
	.loaderr :global(.action) {
		display: flex;
		flex-wrap: wrap;
		justify-content: center;
		gap: var(--space-3);
	}
	/* ---- Kuma header ---- */
	.khead {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}
	.name {
		margin: 0;
		font-size: var(--text-title-1);
		font-weight: var(--weight-bold);
		letter-spacing: var(--tracking-tight);
		overflow-wrap: anywhere;
	}
	.target {
		margin: 0;
		display: flex;
		flex-wrap: wrap;
		align-items: baseline;
		gap: var(--space-2) var(--space-4);
	}
	.url {
		font-size: var(--text-subheadline);
		font-weight: var(--weight-medium);
		color: var(--color-accent);
		overflow-wrap: anywhere;
	}
	.kind {
		font-size: var(--text-footnote);
		color: var(--color-text-secondary);
	}
	.tools {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-2);
		margin-top: var(--space-2);
	}

	/* ---- per-check bar + state pill ---- */
	.beatrow {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: var(--space-5);
	}
	.beatrow :global(.bigbar) {
		flex: 1 1 18rem;
	}
	.every {
		margin: var(--space-4) 0 0;
		font-size: var(--text-footnote);
		color: var(--color-text-secondary);
	}

	/* ---- stats row ---- */
	.stats {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-4);
		margin: 0;
	}
	.kpi {
		flex: 1 1 8.5rem;
		text-align: center;
	}
	.kpi dt {
		display: flex;
		flex-direction: column;
		font-size: var(--text-subheadline);
		font-weight: var(--weight-semibold);
	}
	.kpi dt span {
		font-size: var(--text-caption-1);
		font-weight: var(--weight-regular);
		color: var(--color-text-secondary);
	}
	.kpi dd {
		margin: var(--space-2) 0 0;
		font-size: var(--text-title-3);
		font-weight: var(--weight-semibold);
		font-variant-numeric: tabular-nums;
		color: var(--color-accent);
	}
	.kpi dd.warn {
		color: var(--status-pending-text);
	}
	.kpi dd.bad {
		color: var(--status-down-text);
	}
	.overlay {
		display: flex;
		gap: var(--space-5);
		margin-bottom: var(--space-4);
		font-size: var(--text-footnote);
		color: var(--color-text-secondary);
	}
	.overlay b {
		color: var(--color-text-primary);
		font-variant-numeric: tabular-nums;
	}
	.o::before {
		content: '';
		display: inline-block;
		width: 0.6rem;
		height: 0.6rem;
		border-radius: 2px;
		margin-right: var(--space-2);
		vertical-align: middle;
	}
	.o.min::before {
		background: var(--status-up);
	}
	.o.avg::before {
		background: var(--color-accent);
	}
	.o.max::before {
		background: var(--status-down);
	}

	.cols {
		display: grid;
		grid-template-columns: minmax(0, 2fr) minmax(0, 1fr);
		gap: var(--space-5);
		align-items: start;
	}
	@media (max-width: 1279px) {
		.cols {
			grid-template-columns: minmax(0, 1fr);
		}
	}

	.muted {
		margin: 0;
		color: var(--color-text-tertiary);
		font-size: var(--text-subheadline);
	}

	/* ---- event table ---- */
	.evscroll {
		overflow-x: auto;
	}
	.evtable {
		width: 100%;
		min-width: 32rem;
		border-collapse: collapse;
		font-size: var(--text-subheadline);
	}
	.evtable th {
		text-align: left;
		padding: var(--space-3) var(--space-4);
		font-size: var(--text-footnote);
		font-weight: var(--weight-semibold);
		color: var(--color-text-secondary);
		border-bottom: 1px solid var(--hairline);
	}
	.evtable td {
		padding: var(--space-4);
		border-bottom: 1px solid var(--hairline);
		vertical-align: middle;
	}
	.evtable tbody tr:last-child td {
		border-bottom: 0;
	}
	.evtable .time {
		white-space: nowrap;
		color: var(--color-text-secondary);
		font-variant-numeric: tabular-nums;
	}
	.evtable .msg {
		overflow-wrap: anywhere;
	}
	.lasted {
		display: block;
		margin-top: var(--space-1);
		font-size: var(--text-caption-1);
		color: var(--color-text-secondary);
	}

	.config {
		display: grid;
		grid-template-columns: auto 1fr;
		gap: var(--space-3) var(--space-5);
		margin: 0;
	}
	.config dt {
		color: var(--color-text-tertiary);
		font-size: var(--text-footnote);
	}
	.config dd {
		margin: 0;
		font-size: var(--text-footnote);
		font-weight: var(--weight-medium);
		text-transform: capitalize;
	}
	.tags {
		display: inline-flex;
		flex-wrap: wrap;
		gap: var(--space-2);
	}

	/* The rendered notes are injected with {@html}, so Svelte's scoping never
	   reaches them — every rule below has to be :global under .md. */
	.md {
		font-size: var(--text-subheadline);
		color: var(--color-text-secondary);
		line-height: 1.6;
	}
	.md :global(p) {
		margin: 0 0 var(--space-4);
	}
	.md :global(p:last-child),
	.md :global(ul:last-child),
	.md :global(pre:last-child) {
		margin-bottom: 0;
	}
	.md :global(ul) {
		margin: 0 0 var(--space-4);
		padding-left: var(--space-6);
	}
	.md :global(strong) {
		color: var(--color-text-primary);
		font-weight: var(--weight-semibold);
	}
	.md :global(code) {
		font-family: var(--font-mono);
		font-size: 0.92em;
		padding: 1px var(--space-2);
		border-radius: 4px;
		background: var(--glass-bg-thin);
		border: 1px solid var(--hairline);
	}
	.md :global(pre) {
		margin: 0 0 var(--space-4);
		padding: var(--space-4);
		overflow-x: auto;
		border-radius: var(--radius-squircle-sm);
		background: var(--glass-bg-thin);
		border: 1px solid var(--hairline);
	}
	.md :global(pre code) {
		padding: 0;
		border: none;
		background: none;
	}
	.md :global(a) {
		color: var(--color-accent);
	}
	.tag {
		font-size: var(--text-caption-2);
		padding: 1px var(--space-3);
		border-radius: var(--radius-pill);
		color: var(--tag);
		background: color-mix(in srgb, var(--tag) 14%, transparent);
	}

	.badges {
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: var(--space-5);
	}
	@media (max-width: 900px) {
		.badges {
			grid-template-columns: 1fr;
		}
	}
	.badge-block {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
		padding: var(--space-4);
		border-radius: var(--radius-squircle-sm);
		background: var(--glass-bg-thin);
		border: 1px solid var(--glass-border);
	}
	.badge-head {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-3);
	}
	.badge-label {
		font-size: var(--text-subheadline);
		font-weight: var(--weight-medium);
	}
	.badge-preview {
		height: 20px;
		max-width: 100%;
	}
	.badge-field {
		display: flex;
		align-items: center;
		gap: var(--space-3);
	}
	.badge-key {
		flex-shrink: 0;
		width: 5rem;
		font-size: var(--text-caption-1);
		color: var(--color-text-tertiary);
	}
	.badge-val {
		flex: 1;
		min-width: 0;
		overflow-x: auto;
		white-space: nowrap;
		font-family: var(--font-mono);
		font-size: var(--text-caption-1);
		padding: var(--space-2) var(--space-3);
		border-radius: 6px;
		background: var(--glass-bg-thin);
		border: 1px solid var(--hairline);
	}
</style>
