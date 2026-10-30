<script lang="ts">
	// Public, unauthenticated status page. Lives OUTSIDE the (app) auth group.
	// Instatus/Betterstack-style layout on the dark-graphite glass system:
	// a prominent coloured overall banner, a maintenance section, active-incident
	// timelines, one glass "services" card of grouped monitors — each with a
	// status pill, uptime % and an uptime bar strip — a range selector switching
	// the bars between 24h/7d/30d/90d/1y, past incidents and a footer. Live
	// updates arrive on the public status-page:{slug} WebSocket topic.
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { api, problemStatus } from '$lib/api';
	import { bucketLabelKey, bucketTone } from '$lib/components/statuspage/bars';
	import type {
		PublicPage,
		PublicMonitorView,
		PublicDayBar,
		PublicMaintenance,
		PublicRange
	} from '$lib/api/types';
	import type { IncidentStatus, IncidentSeverity } from '$lib/api/types';
	import Postmortem from '$lib/components/statuspage/Postmortem.svelte';
	import { ws } from '$lib/ws/store.svelte';
	import { t, i18n, type Locale, type MessageKey } from '$lib/i18n';
	import { absoluteTime, relativeTime } from '$lib/util/time';
	import {
		GlassCard,
		HeatmapBars,
		type HeatmapBar,
		StatusBadge,
		SegmentedControl,
		PillButton,
		TextField,
		Skeleton
	} from '$lib/design-system';
	import type { MonitorStatus } from '$lib/design-system';

	const slug = $derived(page.params.slug ?? '');

	let data = $state<PublicPage | null>(null);
	let loading = $state(true);
	let notFound = $state(false);
	// Any failure that is neither 404 nor 401 (5xx, 403, network) — without this
	// the template matched no branch and rendered a blank page.
	let loadError = $state(false);
	// Selected uptime window. Drives the bar strips; a change refetches the page
	// for that range. 90d is the server's default, so first paint matches it.
	let range = $state<PublicRange>('90d');
	// True while a range switch (or a live update) is refreshing an already-loaded
	// page — dims the services card instead of blanking the whole screen.
	let refetching = $state(false);

	// Password gate. A protected page answers 401 until unlocked; rather than the
	// old blank screen we render a prompt, submit the password once, and the
	// server's scoped unlock cookie carries the rest of the page's requests.
	let passwordRequired = $state(false);
	let passwordValue = $state('');
	let unlocking = $state(false);
	let passwordError = $state(false);

	/**
	 * Identifies the newest in-flight request, so a slower earlier one cannot
	 * overwrite it.
	 *
	 * This page has TWO concurrent triggers — the range switcher and the
	 * WebSocket poke, which fires once per monitor transition — so a fleet-wide
	 * outage produces several reloads within a second. Without this a late
	 * 90-day response landed after a freshly chosen 24-hour one and the strip
	 * showed ninety daily bars under a control reading "24h". The archive page
	 * has the same guard; the page it was copied from did not get it.
	 */
	let loadSeq = 0;

	async function load(password?: string) {
		const seq = ++loadSeq;
		if (data) refetching = true;
		try {
			const fresh = await api.publicStatusPage(slug, password, range);
			if (seq !== loadSeq) return;
			data = fresh;
			notFound = false;
			passwordRequired = false;
			passwordError = false;
			loadError = false;
		} catch (e) {
			if (seq !== loadSeq) return;
			const status = problemStatus(e);
			// All four are cleared first and exactly one is then set. Left
			// standing, a notFound from an earlier attempt kept its branch — which
			// is rendered first — so a page that had since been published and
			// password-protected showed "not found" forever and never offered the
			// password field.
			notFound = false;
			passwordRequired = false;
			passwordError = false;
			loadError = false;
			if (status === 404) {
				notFound = true;
			} else if (status === 401) {
				passwordRequired = true;
				// A 401 in response to a submitted password means it was wrong.
				if (password) passwordError = true;
			} else {
				// 5xx, 403 or a network failure: surface a retryable error rather
				// than leaving every branch false and the page blank.
				loadError = true;
			}
		} finally {
			if (seq === loadSeq) {
				loading = false;
				refetching = false;
			}
		}
	}

	function onRangeChange(v: string) {
		range = v as PublicRange;
		load();
	}

	async function unlock(e: SubmitEvent) {
		e.preventDefault();
		if (!passwordValue || unlocking) return;
		unlocking = true;
		passwordError = false;
		await load(passwordValue);
		unlocking = false;
		// Keep the field populated on error so the visitor can correct it; clear
		// it once the page is open so the plaintext does not linger.
		if (data) passwordValue = '';
	}

	onMount(() => {
		load();
		ws.connect();
		const unsub = ws.subscribe(`status-page:${slug}`);
		const off = ws.on((env) => {
			if (env.topic === `status-page:${slug}`) load();
		});
		return () => {
			off();
			unsub();
		};
	});

	// Honor the page's configured language (falls back to the visitor's locale).
	$effect(() => {
		const loc = data?.locale;
		if (loc === 'en' || loc === 'de') i18n.setLocale(loc as Locale);
	});

	// Reflect indexing preference in the document (SPA, so set at runtime).
	$effect(() => {
		if (typeof document === 'undefined') return;
		document.title = data
			? `${data.title} — ${t('public.statusSuffix')}`
			: t('public.statusSuffix');
		let robots = document.querySelector('meta[name="robots"]');
		if (!robots) {
			robots = document.createElement('meta');
			robots.setAttribute('name', 'robots');
			document.head.appendChild(robots);
		}
		robots.setAttribute(
			'content',
			data && data.allow_indexing ? 'index,follow' : 'noindex,nofollow'
		);
	});

	// ---- overall banner --------------------------------------------------------

	// Overall banner colour, expressed as a monitor-status token so the coloured
	// dot + tint reuse the design system's status palette.
	const bannerStatus: Record<string, MonitorStatus> = {
		operational: 'up',
		degraded: 'pending',
		major_outage: 'down',
		maintenance: 'maintenance'
	};

	const allMonitors = $derived(
		data ? [...data.groups.flatMap((g) => g.monitors), ...data.ungrouped] : []
	);

	// Counts that turn the generic overall status into a specific headline
	// ("2 services are down") the way real status pages phrase it.
	const counts = $derived.by(() => {
		let down = 0;
		let degraded = 0;
		for (const m of allMonitors) {
			if (m.status === 'down') down++;
			else if (m.status === 'pending') degraded++;
		}
		return { down, degraded };
	});

	function bannerHeadline(): string {
		const st = data?.overall_status;
		if (st === 'operational') return t('public.operational');
		if (st === 'maintenance') return t('public.underMaintenance');
		if (st === 'major_outage') {
			return counts.down > 0
				? t(counts.down === 1 ? 'public.servicesDownOne' : 'public.servicesDownMany', {
						count: counts.down
					})
				: t('public.majorOutage');
		}
		const affected = counts.down + counts.degraded;
		return affected > 0
			? t(affected === 1 ? 'public.servicesDegradedOne' : 'public.servicesDegradedMany', {
					count: affected
				})
			: t('public.degraded');
	}

	// ---- maintenance -----------------------------------------------------------

	const monitorNameById = $derived(new Map(allMonitors.map((m) => [m.id, m.name])));

	// Monitors covered by a maintenance window that is in effect RIGHT NOW, so the
	// service row can carry a "Maintenance" chip. An active window scoped to no
	// monitor ([] ) covers every service on the page.
	const activeMaintenance = $derived.by(() => {
		const ids = new Set<number>();
		let all = false;
		for (const w of data?.maintenance ?? []) {
			if (!w.active) continue;
			if (w.monitor_ids.length === 0) all = true;
			else for (const id of w.monitor_ids) ids.add(id);
		}
		return { ids, all };
	});

	function underMaintenance(id: number): boolean {
		return activeMaintenance.all || activeMaintenance.ids.has(id);
	}

	// Localized window text: an explicit start–end, an open-ended "from"/"until",
	// or "Ongoing" for a window with no announced bounds.
	function maintWindow(w: PublicMaintenance): string {
		if (w.starts_at && w.ends_at)
			return `${absoluteTime(w.starts_at)} – ${absoluteTime(w.ends_at)}`;
		if (w.starts_at) return t('public.maintFrom', { time: absoluteTime(w.starts_at) });
		if (w.ends_at) return t('public.maintUntil', { time: absoluteTime(w.ends_at) });
		return t('public.maintOngoing');
	}

	function maintServices(w: PublicMaintenance): string {
		if (!w.monitor_ids || w.monitor_ids.length === 0) return t('public.maintAllServices');
		const names = w.monitor_ids
			.map((id) => monitorNameById.get(id))
			.filter((n): n is string => !!n);
		return names.length ? names.join(', ') : t('public.maintAllServices');
	}

	// ---- uptime bars -----------------------------------------------------------

	const RANGE_WINDOW_KEY: Record<PublicRange, MessageKey> = {
		'24h': 'public.window24h',
		'7d': 'public.window7d',
		'30d': 'public.window30d',
		'90d': 'public.window90d',
		'1y': 'public.window1y'
	};

	const rangeSegments = $derived([
		{ value: '24h', label: t('public.range24h') },
		{ value: '7d', label: t('public.range7d') },
		{ value: '30d', label: t('public.range30d') },
		{ value: '90d', label: t('public.range90d') },
		{ value: '1y', label: t('public.range1y') }
	]);

	// The window phrase ("the last 7 days") for the strip's accessible name, read
	// off the range the server actually resolved.
	const windowText = $derived(data ? t(RANGE_WINDOW_KEY[data.range] ?? 'public.window90d') : '');
	// Hourly ranges pack many buckets in; tighten the bar geometry for them.
	const barGap = $derived(data?.bucket === 'hour' ? 1 : 2);
	const barMin = $derived(data?.bucket === 'hour' ? 2 : 3);
	// The trailing bucket is the current, still-open period.
	const newestLabel = $derived(data?.bucket === 'hour' ? t('public.now') : t('public.today'));

	function bucketLabel(startISO: string): string {
		const d = new Date(startISO);
		if (Number.isNaN(d.getTime())) return startISO;
		const tz = i18n.effectiveTimezone;
		const opts: Intl.DateTimeFormatOptions =
			data?.bucket === 'hour'
				? { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', timeZone: tz }
				: data?.bucket === 'month'
					? { month: 'short', year: 'numeric', timeZone: tz }
					: { month: 'short', day: 'numeric', timeZone: tz };
		return new Intl.DateTimeFormat(i18n.localeTag, opts).format(d);
	}

	// bucketTone picks the colour; bucketLabelKey picks the words, which must not
	// inherit the colour collapse (see bars.ts). null means the palette name is
	// already accurate.
	function labelFor(s: PublicDayBar['status']): string | undefined {
		const key = bucketLabelKey(s);
		return key ? t(key) : undefined;
	}

	function toBars(m: PublicMonitorView): HeatmapBar[] {
		return m.heatmap.map((d) => ({
			label: d.start ? bucketLabel(d.start) : d.date,
			status: bucketTone(d.status),
			statusLabel: labelFor(d.status),
			// No percentage for a bucket with nothing in it: the status label
			// already says so, and repeating it read "no data · no data".
			detail: d.uptime >= 0 ? `${(d.uptime * 100).toFixed(2)}%` : undefined
		}));
	}

	function oldestLabel(m: PublicMonitorView): string {
		const first = m.heatmap[0];
		return first?.start ? relativeTime(first.start) : '';
	}

	function uptimePct(m: PublicMonitorView): string {
		return m.uptime >= 0 ? `${(m.uptime * 100).toFixed(2)}%` : '—';
	}

	/**
	 * Whether any component on the page publishes its uptime.
	 *
	 * The archive omits monitors with show_uptime=false — that flag is the
	 * operator's decision about what is public, and an archive is nothing but
	 * figures — so a page that hides all of them would archive to an empty table.
	 */
	const hasPublishedUptime = $derived(
		[...(data?.groups.flatMap((g) => g.monitors) ?? []), ...(data?.ungrouped ?? [])].some(
			(m) => m.show_uptime
		)
	);

	// ---- incidents -------------------------------------------------------------

	/**
	 * Which past incidents have their retrospective expanded.
	 *
	 * A Set held in a `$state` and REPLACED on every change, not mutated: Svelte 5
	 * tracks the reference here, so `openPostmortems.add(k)` alone would update
	 * nothing on screen.
	 *
	 * Keyed on title + start, the same key the `{#each}` uses, and NOT on the
	 * index or the id. An index would move an open panel onto a different incident
	 * the moment one is added or resolved — this list refetches live over the
	 * WebSocket — and the id is not usable here: the mock backend hands every
	 * public incident id 0, so keying on it would open all of them at once in a
	 * mock build and hide that from anyone developing against it.
	 */
	let openPostmortems = $state(new Set<string>());
	const incidentKey = (inc: { title: string; started_at: string }) =>
		`${inc.title}|${inc.started_at}`;
	function togglePostmortem(key: string) {
		const next = new Set(openPostmortems);
		if (!next.delete(key)) next.add(key);
		openPostmortems = next;
	}

	function severityLabel(sev: IncidentSeverity): string {
		return sev === 'minor'
			? t('public.severityMinor')
			: sev === 'major'
				? t('public.severityMajor')
				: t('public.severityCritical');
	}
	function incidentStatusLabel(status: IncidentStatus): string {
		return status === 'investigating'
			? t('public.incInvestigating')
			: status === 'identified'
				? t('public.incIdentified')
				: status === 'monitoring'
					? t('public.incMonitoring')
					: t('public.incResolved');
	}

	// ---- per-page brand accent -------------------------------------------------

	// Relative luminance decides whether text on the accent should be light or
	// dark, so a pale brand colour does not produce white-on-white buttons.
	function onAccent(hex: string): string {
		const n = parseInt(hex.slice(1), 16);
		const f = (c: number) => {
			const s = c / 255;
			return s <= 0.03928 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
		};
		const L = 0.2126 * f((n >> 16) & 255) + 0.7152 * f((n >> 8) & 255) + 0.0722 * f(n & 255);
		return L > 0.5 ? '#111827' : '#ffffff';
	}

	// Per-page accent, applied as a scoped override of the token family so the
	// visitor's theme tokens outside this page are untouched. The hex is
	// re-checked here because it is interpolated into a style attribute.
	const accentStyle = $derived.by(() => {
		const a = data?.accent_color;
		if (!a || !/^#[0-9a-f]{6}$/.test(a)) return undefined;
		return [
			`--color-accent: ${a}`,
			`--color-accent-hover: color-mix(in srgb, ${a} 82%, var(--color-text-primary))`,
			`--color-accent-tint: color-mix(in srgb, ${a} 14%, transparent)`,
			`--color-text-on-accent: ${onAccent(a)}`
		].join('; ');
	});
</script>

<div class="wrap" style={accentStyle}>
	{#if loading}
		<Skeleton height="96px" />
		<Skeleton height="110px" />
		<Skeleton height="280px" />
	{:else if notFound}
		<GlassCard class="notfound">
			<h1>{t('public.notFoundTitle')}</h1>
			<p>{t('public.notFoundBody')}</p>
		</GlassCard>
	{:else if passwordRequired && !data}
		<GlassCard class="password-gate">
			<h1>{t('public.passwordTitle')}</h1>
			<p class="muted">{t('public.passwordBody')}</p>
			<form onsubmit={unlock}>
				<TextField
					label={t('public.passwordLabel')}
					type="password"
					bind:value={passwordValue}
					autocomplete="current-password"
					error={passwordError ? t('public.passwordWrong') : undefined}
					required
				/>
				<PillButton type="submit" variant="primary" loading={unlocking}
					>{t('public.passwordSubmit')}</PillButton
				>
			</form>
		</GlassCard>
	{:else if data}
		<header class="page-head">
			{#if data.logo_path}
				<img
					class="logo"
					src={data.logo_path}
					alt={t('public.logoAlt', { name: data.title })}
					loading="lazy"
				/>
			{/if}
			<h1>{data.title}</h1>
			{#if data.description}<p class="desc">{data.description}</p>{/if}
		</header>

		<!-- Prominent, colour-coded overall status. role=status + aria-live so the
		     headline is announced when a live update flips the banner. -->
		<section
			class="overall ds-glass"
			data-status={bannerStatus[data.overall_status] ?? 'up'}
			role="status"
			aria-live="polite"
		>
			<span class="overall-dot" aria-hidden="true"></span>
			<div class="overall-copy">
				<p class="overall-label">{bannerHeadline()}</p>
				<p class="overall-sub">{t('public.asOf', { time: absoluteTime(data.generated_at) })}</p>
			</div>
		</section>

		{#if data.maintenance.length > 0}
			<section class="block">
				<h2>{t('public.maintenance')}</h2>
				{#each data.maintenance as w (w.id + (w.starts_at ?? ''))}
					<article class="maint ds-glass" data-active={w.active}>
						<div class="maint-head">
							<span class="maint-pill {w.active ? 'is-active' : ''}">
								{w.active ? t('public.maintInProgress') : t('public.maintScheduled')}
							</span>
							<h3 class="maint-title">{w.title}</h3>
						</div>
						{#if w.body}<p class="maint-body">{w.body}</p>{/if}
						<dl class="maint-meta">
							<div>
								<dt>{t('public.maintAffected')}</dt>
								<dd>{maintServices(w)}</dd>
							</div>
							<div>
								<dt>{t('public.maintWhen')}</dt>
								<dd>{maintWindow(w)}</dd>
							</div>
						</dl>
					</article>
				{/each}
			</section>
		{/if}

		{#if data.active_incidents.length > 0}
			<section class="block">
				<h2>{t('public.activeIncidents')}</h2>
				{#each data.active_incidents as inc (inc.title + inc.started_at)}
					<article class="incident ds-glass">
						<div class="incident-head">
							<span class="sev sev-{inc.severity}">{severityLabel(inc.severity)}</span>
							<h3>{inc.title}</h3>
							<span class="inc-status">{incidentStatusLabel(inc.status)}</span>
						</div>
						<ol class="timeline">
							{#each inc.updates as u (u.created_at)}
								<li>
									<span class="tl-status">{incidentStatusLabel(u.status)}</span>
									<span class="tl-body">{u.body}</span>
									<time>{absoluteTime(u.created_at)}</time>
								</li>
							{/each}
						</ol>
					</article>
				{/each}
			</section>
		{/if}

		<section class="block components {refetching ? 'is-busy' : ''}">
			<div class="section-head">
				<h2>{t('public.components')}</h2>
				{#if allMonitors.length > 0}
					<SegmentedControl
						segments={rangeSegments}
						value={range}
						onchange={onRangeChange}
						size="sm"
						label={t('public.rangeLabel')}
					/>
				{/if}
			</div>

			{#if allMonitors.length === 0}
				<p class="muted empty">{t('public.noComponents')}</p>
			{:else}
				<div class="services ds-glass">
					<!-- Keyed on ids, not names: neither monitor nor group names are
					     unique, and a duplicate key makes Svelte throw and blank the page. -->
					{#each data.groups as group (group.id)}
						<div class="group">
							<h3 class="group-name">{group.name}</h3>
							{#each group.monitors as m (m.id)}
								{@render monitorRow(m)}
							{/each}
						</div>
					{/each}
					{#each data.ungrouped as m (m.id)}
						{@render monitorRow(m)}
					{/each}
				</div>
			{/if}
		</section>

		{#if data.recent_incidents.length > 0}
			<section class="block">
				<h2>{t('public.pastIncidents')}</h2>
				{#each data.recent_incidents as inc (inc.title + inc.started_at)}
					<article class="incident resolved ds-glass">
						<div class="incident-head">
							<h3>{inc.title}</h3>
							<span class="inc-status"
								>{t('public.resolved')}{inc.resolved_at
									? ` · ${absoluteTime(inc.resolved_at)}`
									: ''}</span
							>
						</div>
						<!-- The retrospective. Only ever present when the server published
						     it: a draft is stripped from this payload before it is built
						     (Incident.publicCopy), so anything here is public by an
						     explicit act. Collapsed by default — a page listing ten past
						     incidents must not open as ten essays. -->
						{#if inc.postmortem}
							<div class="pmwrap">
								<button
									type="button"
									class="pmtoggle"
									aria-expanded={openPostmortems.has(incidentKey(inc))}
									onclick={() => togglePostmortem(incidentKey(inc))}
								>
									<span
										class="pmcaret"
										class:open={openPostmortems.has(incidentKey(inc))}
										aria-hidden="true">›</span
									>
									{openPostmortems.has(incidentKey(inc))
										? t('public.postmortemHide')
										: t('public.postmortemShow')}
									{#if inc.postmortem_published_at}
										<span class="pmdate"
											>{t('public.postmortemPosted', {
												date: absoluteTime(inc.postmortem_published_at)
											})}</span
										>
									{/if}
								</button>
								{#if openPostmortems.has(incidentKey(inc))}
									<Postmortem source={inc.postmortem} />
								{/if}
							</div>
						{/if}
					</article>
				{/each}
			</section>
		{/if}

		<footer class="page-foot">
			<!-- Only offered when the page actually publishes figures. On a page
			     where every component hides its uptime the archive is an empty
			     table, and a link to nothing is worse than no link. -->
			{#if hasPublishedUptime}
				<a class="archive-link" href="/status/{slug}/history">{t('public.archiveLink')}</a>
			{/if}
			{#if data.footer_text}<span>{data.footer_text}</span>{/if}
			<span class="foot-updated">{t('public.asOf', { time: absoluteTime(data.generated_at) })}</span
			>
		</footer>
	{:else if loadError}
		<GlassCard class="loaderror">
			<h1>{t('public.errorTitle')}</h1>
			<p class="muted">{t('public.errorBody')}</p>
			<div class="retry">
				<PillButton variant="primary" onclick={() => load()}>{t('public.retry')}</PillButton>
			</div>
		</GlassCard>
	{/if}
</div>

{#snippet monitorRow(m: PublicMonitorView)}
	<div class="monitor">
		<div class="mon-top">
			<div class="mon-id">
				<span class="mon-name">{m.name}</span>
				{#if underMaintenance(m.id)}
					<span class="chip-maint">{t('public.maintenance')}</span>
				{/if}
			</div>
			<div class="mon-right">
				{#if m.show_uptime}<span class="uptime">{uptimePct(m)}</span>{/if}
				<StatusBadge status={m.status} size="sm" />
			</div>
		</div>
		<!-- The whole strip is gated, not just the percentage above it.
		     show_uptime=false now means the server sends no figures and no bars at
		     all, so rendering this anyway produced an empty graphic whose
		     accessible name still promised "Uptime for <name>, the last 90 days",
		     an empty left caption and a dangling "Today" on the right. -->
		{#if m.show_uptime}
			<div class="strip-scroll">
				<HeatmapBars
					bars={toBars(m)}
					gap={barGap}
					minBarWidth={barMin}
					label={t('public.uptimeLabel', { name: m.name, window: windowText })}
				/>
			</div>
			<div class="strip-foot">
				<span>{oldestLabel(m)}</span>
				<span>{newestLabel}</span>
			</div>
		{/if}
	</div>
{/snippet}

<style>
	.wrap {
		max-width: 860px;
		margin: 0 auto;
		padding: clamp(1rem, 4vw, 3rem) 1rem 4rem;
		display: flex;
		flex-direction: column;
		gap: var(--space-7);
	}

	/* ---- header ---------------------------------------------------------- */
	.page-head {
		display: flex;
		flex-direction: column;
		align-items: center;
		text-align: center;
		gap: var(--space-2);
	}
	.logo {
		max-height: 56px;
		max-width: 220px;
		margin-bottom: var(--space-2);
	}
	.page-head h1 {
		font-size: var(--text-title-1);
		font-weight: var(--weight-bold);
		letter-spacing: var(--tracking-tight);
	}
	.desc {
		color: var(--color-text-secondary);
		max-width: 46ch;
	}

	/* ---- overall banner -------------------------------------------------- */
	.overall {
		--bnr: var(--status-up);
		display: flex;
		align-items: center;
		gap: var(--space-5);
		padding: var(--space-6) var(--space-7);
		/* accent bar drawn inside so the glass border + radius stay intact */
		box-shadow:
			inset 3px 0 0 var(--bnr),
			var(--elevation-glass-2);
		background: color-mix(in srgb, var(--bnr) 10%, var(--glass-bg-regular));
	}
	.overall[data-status='down'] {
		--bnr: var(--status-down);
	}
	.overall[data-status='pending'] {
		--bnr: var(--status-pending);
	}
	.overall[data-status='maintenance'] {
		--bnr: var(--status-maintenance);
	}
	.overall-dot {
		width: 0.85rem;
		height: 0.85rem;
		border-radius: 50%;
		background: var(--bnr);
		box-shadow: 0 0 0 6px color-mix(in srgb, var(--bnr) 22%, transparent);
		flex-shrink: 0;
	}
	.overall-label {
		font-size: var(--text-title-2);
		font-weight: var(--weight-bold);
		letter-spacing: var(--tracking-tight);
		color: var(--color-text-primary);
	}
	.overall-sub {
		margin-top: 2px;
		font-size: var(--text-footnote);
		color: var(--color-text-secondary);
	}

	/* ---- section blocks -------------------------------------------------- */
	.block h2 {
		font-size: var(--text-title-3);
		font-weight: var(--weight-semibold);
		letter-spacing: var(--tracking-normal);
		margin-bottom: var(--space-4);
	}
	.section-head {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-4);
		flex-wrap: wrap;
		margin-bottom: var(--space-4);
	}
	.section-head h2 {
		margin: 0;
	}

	/* ---- maintenance ----------------------------------------------------- */
	.maint {
		padding: var(--space-5) var(--space-6);
	}
	.maint + .maint {
		margin-top: var(--space-4);
	}
	.maint-head {
		display: flex;
		align-items: center;
		gap: var(--space-4);
		margin-bottom: var(--space-3);
	}
	.maint-title {
		font-size: var(--text-headline);
		font-weight: var(--weight-semibold);
	}
	.maint-pill {
		flex-shrink: 0;
		font-size: var(--text-caption-2);
		font-weight: var(--weight-semibold);
		text-transform: uppercase;
		letter-spacing: var(--tracking-wide);
		padding: 2px var(--space-3);
		border-radius: var(--radius-pill);
		background: var(--status-paused-tint);
		color: var(--status-paused-text);
		border: 1px solid color-mix(in srgb, var(--status-paused) 30%, transparent);
	}
	.maint-pill.is-active {
		background: var(--status-maintenance-tint);
		color: var(--status-maintenance-text);
		border-color: color-mix(in srgb, var(--status-maintenance) 32%, transparent);
	}
	.maint-body {
		color: var(--color-text-secondary);
		font-size: var(--text-subheadline);
		margin-bottom: var(--space-4);
	}
	.maint-meta {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-3) var(--space-9);
		margin: 0;
	}
	.maint-meta div {
		display: flex;
		flex-direction: column;
		gap: 2px;
		min-width: 0;
	}
	.maint-meta dt {
		font-size: var(--text-caption-1);
		text-transform: uppercase;
		letter-spacing: var(--tracking-wide);
		color: var(--color-text-tertiary);
	}
	.maint-meta dd {
		margin: 0;
		font-size: var(--text-footnote);
		color: var(--color-text-primary);
	}

	/* ---- postmortem ------------------------------------------------------ */
	.pmwrap {
		margin-top: var(--space-3);
		padding-top: var(--space-3);
		border-top: 1px solid var(--hairline);
	}
	.pmtoggle {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		width: 100%;
		padding: 0;
		border: 0;
		background: none;
		cursor: pointer;
		font: inherit;
		font-size: var(--text-subheadline);
		color: var(--color-accent);
		text-align: left;
	}
	.pmcaret {
		display: inline-block;
		transition: transform var(--duration-sm) ease;
	}
	.pmcaret.open {
		transform: rotate(90deg);
	}
	.pmdate {
		margin-left: auto;
		font-size: var(--text-caption-1);
		color: var(--color-text-tertiary);
	}
	@media (prefers-reduced-motion: reduce) {
		.pmcaret {
			transition: none;
		}
	}

	/* ---- incidents ------------------------------------------------------- */
	.incident {
		padding: var(--space-5) var(--space-6);
	}
	.incident + .incident {
		margin-top: var(--space-4);
	}
	.incident-head {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		margin-bottom: var(--space-4);
	}
	.incident-head h3 {
		font-size: var(--text-headline);
		font-weight: var(--weight-semibold);
		flex: 1;
	}
	.incident.resolved .incident-head {
		margin-bottom: 0;
	}
	.sev {
		font-size: var(--text-caption-2);
		text-transform: uppercase;
		letter-spacing: var(--tracking-wide);
		padding: 2px var(--space-3);
		border-radius: var(--radius-pill);
		font-weight: var(--weight-bold);
		border: 1px solid transparent;
		flex-shrink: 0;
	}
	.sev-minor {
		background: var(--status-pending-tint);
		color: var(--status-pending-text);
		border-color: color-mix(in srgb, var(--status-pending) 30%, transparent);
	}
	.sev-major {
		background: color-mix(in srgb, #e8863a 18%, transparent);
		color: #f2b072;
		border-color: color-mix(in srgb, #e8863a 32%, transparent);
	}
	.sev-critical {
		background: var(--status-down-tint);
		color: var(--status-down-text);
		border-color: color-mix(in srgb, var(--status-down) 30%, transparent);
	}
	.inc-status {
		font-size: var(--text-footnote);
		color: var(--color-text-secondary);
	}
	.timeline {
		list-style: none;
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
		border-left: 2px solid var(--hairline);
		padding-left: var(--space-5);
	}
	.timeline li {
		display: flex;
		flex-direction: column;
		gap: 2px;
	}
	.tl-status {
		font-size: var(--text-caption-1);
		text-transform: uppercase;
		letter-spacing: var(--tracking-wide);
		font-weight: var(--weight-bold);
		color: var(--color-accent);
	}
	.tl-body {
		font-size: var(--text-subheadline);
		color: var(--color-text-primary);
	}
	.timeline time {
		font-size: var(--text-caption-1);
		color: var(--color-text-tertiary);
	}

	/* ---- components / services card -------------------------------------- */
	.components.is-busy {
		opacity: 0.55;
		pointer-events: none;
		transition: opacity var(--duration-md) ease;
	}
	.services {
		padding: var(--space-2) var(--space-6);
	}
	.group-name {
		font-size: var(--text-caption-1);
		text-transform: uppercase;
		letter-spacing: var(--tracking-wide);
		color: var(--color-text-tertiary);
		padding: var(--space-5) 0 var(--space-2);
	}
	.monitor {
		padding: var(--space-4) 0;
		border-top: 1px solid var(--hairline);
	}
	/* No leading hairline directly under the card top or a group header. */
	.services > .monitor:first-child,
	.group:first-child .group-name {
		border-top: 0;
	}
	.group .monitor:first-of-type {
		border-top: 0;
	}
	.mon-top {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-4);
	}
	.mon-id {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		min-width: 0;
	}
	.mon-name {
		font-weight: var(--weight-medium);
		color: var(--color-text-primary);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.chip-maint {
		flex-shrink: 0;
		font-size: var(--text-caption-2);
		font-weight: var(--weight-semibold);
		text-transform: uppercase;
		letter-spacing: var(--tracking-wide);
		padding: 2px var(--space-3);
		border-radius: var(--radius-pill);
		background: var(--status-maintenance-tint);
		color: var(--status-maintenance-text);
		border: 1px solid color-mix(in srgb, var(--status-maintenance) 30%, transparent);
		white-space: nowrap;
	}
	.mon-right {
		display: flex;
		align-items: center;
		gap: var(--space-4);
		flex-shrink: 0;
	}
	.uptime {
		font-variant-numeric: tabular-nums;
		color: var(--color-text-secondary);
		font-size: var(--text-footnote);
	}
	.strip-scroll {
		margin-top: var(--space-3);
		overflow-x: auto;
		scrollbar-width: thin;
	}
	.strip-foot {
		display: flex;
		justify-content: space-between;
		margin-top: var(--space-2);
		font-size: var(--text-caption-1);
		color: var(--color-text-tertiary);
	}

	/* ---- shared / footer ------------------------------------------------- */
	.muted {
		color: var(--color-text-secondary);
		font-size: var(--text-subheadline);
	}
	.empty {
		padding: var(--space-6) 0;
		text-align: center;
	}
	.archive-link {
		color: var(--color-accent);
		text-decoration: none;
	}
	.archive-link:hover {
		text-decoration: underline;
	}
	.page-foot {
		display: flex;
		flex-direction: column;
		align-items: center;
		gap: var(--space-1);
		text-align: center;
		color: var(--color-text-tertiary);
		font-size: var(--text-caption-1);
		padding-top: var(--space-6);
		border-top: 1px solid var(--hairline);
	}

	:global(.password-gate),
	:global(.loaderror) {
		text-align: center;
	}
	:global(.password-gate) h1,
	:global(.loaderror) h1 {
		font-size: var(--text-title-2);
		font-weight: var(--weight-bold);
		margin-bottom: var(--space-2);
	}
	:global(.password-gate) form {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
		margin-top: var(--space-5);
		text-align: left;
		max-width: 320px;
		margin-inline: auto;
	}
	.retry {
		margin-top: var(--space-5);
	}

	@media (max-width: 560px) {
		.overall {
			padding: var(--space-5) var(--space-6);
		}
		.overall-label {
			font-size: var(--text-title-3);
		}
		.section-head {
			align-items: flex-start;
			flex-direction: column;
		}
	}
</style>
