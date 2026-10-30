<script lang="ts">
	/**
	 * The availability archive: month-by-month uptime over years, as a table.
	 *
	 * A separate route rather than a sixth range on the status page, because it
	 * answers a different question. The page's range selector is "how did the last
	 * N days go" at bar resolution; this is "how has this held up" at month
	 * resolution over years, and rendering that as 120 more bars would be
	 * unreadable and would make every ordinary page view pay for it.
	 *
	 * Deliberately NOT live. There is no WebSocket subscription here: a figure for
	 * a month that closed eighteen months ago does not change, and a table that
	 * re-renders under the reader for no reason is worse than a static one.
	 */
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { api, problemStatus } from '$lib/api';
	import type { PublicHistory } from '$lib/api/types';
	import { t, i18n, type Locale } from '$lib/i18n';
	import { GlassCard, PillButton, SegmentedControl, Skeleton, TextField } from '$lib/design-system';

	const slug = $derived(page.params.slug ?? '');

	let data = $state<PublicHistory | null>(null);
	let loading = $state(true);
	let notFound = $state(false);
	let loadError = $state(false);
	let months = $state(24);

	// The same password gate as the status page. The server scopes its unlock
	// cookie to /api/v1/public/status, so a visitor who already unlocked the page
	// never sees this — but somebody landing on the archive link directly does.
	let passwordRequired = $state(false);
	let passwordValue = $state('');
	let unlocking = $state(false);
	let passwordError = $state(false);

	/**
	 * Identifies the newest in-flight request, so a slower earlier one cannot
	 * overwrite it.
	 *
	 * Switching from 5 years to 1 year fires a second request while the first is
	 * still running, and the 5-year query is by far the slower of the two — so
	 * the late response landed last and the table showed sixty months under a
	 * control reading "1 year". Every write below is gated on still being the
	 * latest.
	 */
	let loadSeq = 0;

	async function load(password?: string) {
		const seq = ++loadSeq;
		try {
			const res = await api.publicStatusHistory(slug, password, months);
			if (seq !== loadSeq) return;
			data = res;
			passwordRequired = false;
			passwordError = false;
			notFound = false;
			loadError = false;
			// The page carries its own locale; honour it here too, or the archive
			// of a German page would render its month names in English.
			if (res.locale) i18n.setLocale(res.locale as Locale);
		} catch (e) {
			if (seq !== loadSeq) return;
			const status = problemStatus(e);
			// All three are cleared first and exactly one is then set. Left
			// standing, passwordError from an earlier wrong password made the next
			// attempt report "Incorrect password" for a server error — sending the
			// visitor to look for a password they already had right.
			passwordRequired = false;
			passwordError = false;
			notFound = false;
			loadError = false;
			if (status === 401) {
				passwordRequired = true;
				passwordError = password !== undefined;
			} else if (status === 404) {
				notFound = true;
			} else {
				loadError = true;
			}
		} finally {
			if (seq === loadSeq) {
				loading = false;
				unlocking = false;
			}
		}
	}

	onMount(() => void load());

	async function submitPassword() {
		if (!passwordValue) return;
		unlocking = true;
		await load(passwordValue);
	}

	const windowOptions = $derived([
		{ value: '12', label: t('archive.window1y') },
		{ value: '24', label: t('archive.window2y') },
		{ value: '60', label: t('archive.window5y') }
	]);

	// True while a window switch refetches an already-rendered table. It dims the
	// table rather than blanking the page: the previous window's numbers are still
	// the right shape, and swapping them for a skeleton makes a two-click
	// comparison flicker.
	let switching = $state(false);

	/**
	 * The page accent, re-validated before it goes anywhere near a style
	 * attribute.
	 *
	 * The status page does the same thing for the same reason: this value is
	 * server-supplied, and interpolating it unchecked would let anything stored
	 * in the column close the declaration and add its own. The column is written
	 * through a validating endpoint, so this is the second line rather than the
	 * first — which is exactly the line that has to hold when the first one is
	 * changed.
	 */
	const accentStyle = $derived.by(() => {
		const a = data?.accent_color;
		if (!a || !/^#[0-9a-f]{6}$/i.test(a)) return undefined;
		return `--color-accent: ${a}`;
	});

	async function setWindow(v: string) {
		months = Number(v);
		switching = true;
		try {
			await load();
		} finally {
			switching = false;
		}
	}

	/**
	 * A month label from the bucket start.
	 *
	 * Formatted from the UTC parts explicitly rather than through a local-time
	 * Date: the bucket is the first of the month at 00:00 UTC, and west of
	 * Greenwich a naive render turns that into the last day of the PREVIOUS month
	 * — so the archive would be off by one for every reader in the Americas.
	 */
	function monthLabel(iso: string): string {
		const d = new Date(iso);
		if (Number.isNaN(d.getTime())) return iso;
		return new Intl.DateTimeFormat(i18n.locale, {
			year: 'numeric',
			month: 'short',
			timeZone: 'UTC'
		}).format(d);
	}

	/**
	 * A 0..1 fraction as a percentage, or a dash when the month has no data.
	 *
	 * TRUNCATED, not rounded. `toFixed` renders 99.999% as "100.00%", and a
	 * historical availability record that reports a month with downtime in it as a
	 * perfect month is the one number this page must not produce. Truncation can
	 * only understate, so 100.00% here means exactly what it says.
	 */
	function pct(v: number): string {
		if (v < 0) return '—';
		return `${(Math.floor(v * 10000) / 100).toFixed(2)}%`;
	}

	function tone(v: number): 'none' | 'up' | 'degraded' | 'down' {
		if (v < 0) return 'none';
		if (v >= 0.9999) return 'up';
		if (v >= 0.95) return 'degraded';
		return 'down';
	}
</script>

<svelte:head>
	<title>{data ? `${t('archive.title')} · ${data.title}` : t('archive.title')}</title>
	{#if data && !data.allow_indexing}<meta name="robots" content="noindex, nofollow" />{/if}
</svelte:head>

<main class="wrap" style={accentStyle}>
	{#if loading}
		<Skeleton lines={8} />
	{:else if passwordRequired}
		<GlassCard title={t('public.passwordTitle')}>
			<p class="muted">{t('public.passwordBody')}</p>
			<form
				class="pwform"
				onsubmit={(e) => {
					e.preventDefault();
					submitPassword();
				}}
			>
				<TextField
					type="password"
					label={t('public.passwordLabel')}
					bind:value={passwordValue}
					autocomplete="current-password"
				/>
				{#if passwordError}<p class="err">{t('public.passwordWrong')}</p>{/if}
				<PillButton type="submit" loading={unlocking}>{t('public.passwordSubmit')}</PillButton>
			</form>
		</GlassCard>
	{:else if notFound}
		<GlassCard title={t('public.notFoundTitle')}>
			<p class="muted">{t('public.notFoundBody')}</p>
		</GlassCard>
	{:else if loadError || !data}
		<GlassCard title={t('public.errorTitle')}>
			<p class="muted">{t('public.errorBody')}</p>
			<PillButton onclick={() => load()}>{t('public.retry')}</PillButton>
		</GlassCard>
	{:else}
		<header class="head">
			<div class="titles">
				{#if data.logo_path}<img class="logo" src={data.logo_path} alt="" />{/if}
				<div>
					<h1>{data.title}</h1>
					<p class="muted">{t('archive.lead')}</p>
				</div>
			</div>
			<div class="controls">
				<SegmentedControl
					segments={windowOptions}
					value={String(months)}
					onchange={setWindow}
					size="sm"
					label={t('archive.windowLabel')}
				/>
				<a class="back" href="/status/{slug}">{t('archive.back')}</a>
			</div>
		</header>

		{#if data.monitors.length === 0}
			<GlassCard title={t('archive.emptyTitle')}>
				<p class="muted">{t('archive.emptyBody')}</p>
			</GlassCard>
		{:else}
			<!-- The table scrolls inside its own container. Sixty months is wider
			     than any phone, and letting the PAGE scroll sideways would drag the
			     header and the footer along with it. -->
			<div class="tablewrap ds-glass" class:busy={switching}>
				<table>
					<thead>
						<tr>
							<th scope="col" class="mon">{t('archive.colMonitor')}</th>
							{#each data.months as m (m)}
								<th scope="col">{monthLabel(m)}</th>
							{/each}
							<th scope="col" class="total">{t('archive.colTotal')}</th>
						</tr>
					</thead>
					<tbody>
						{#each data.monitors as row (row.id)}
							<tr>
								<th scope="row" class="mon">{row.name}</th>
								<!-- Keyed on the bucket's own start, not on the parallel months
								     array by index: were the two ever different lengths, the
								     index lookup would be undefined and Svelte would throw on a
								     duplicate key, blanking the whole page. -->
								{#each row.months as b (b.start)}
									<td
										data-tone={tone(b.uptime)}
										title={`${monthLabel(b.start)} · ${pct(b.uptime)}`}
									>
										{pct(b.uptime)}
									</td>
								{/each}
								<td class="total" data-tone={tone(row.uptime)}>{pct(row.uptime)}</td>
							</tr>
						{/each}
					</tbody>
					<tfoot>
						<tr>
							<th scope="row" class="mon">{t('archive.colOverall')}</th>
							<!-- Driven by `months` and indexing INTO `overall`, so a short
							     overall array renders a dash instead of a key collision. -->
							{#each data.months as m, i (m)}
								<td data-tone={tone(data.overall[i] ?? -1)}>{pct(data.overall[i] ?? -1)}</td>
							{/each}
							<td class="total"></td>
						</tr>
					</tfoot>
				</table>
			</div>
			<p class="foot muted">{t('archive.noDataNote')}</p>
		{/if}
	{/if}
</main>

<style>
	.wrap {
		max-width: 72rem;
		margin: 0 auto;
		padding: var(--space-6) var(--space-5) var(--space-8);
		display: flex;
		flex-direction: column;
		gap: var(--space-5);
	}
	.head {
		display: flex;
		align-items: flex-end;
		justify-content: space-between;
		gap: var(--space-4);
		flex-wrap: wrap;
	}
	.titles {
		display: flex;
		align-items: center;
		gap: var(--space-4);
		min-width: 0;
	}
	.logo {
		height: 2.5rem;
		width: auto;
		border-radius: var(--radius-squircle-sm);
	}
	h1 {
		margin: 0;
		font-size: var(--text-title-2);
		color: var(--color-text-primary);
	}
	.muted {
		margin: 0;
		color: var(--color-text-secondary);
		font-size: var(--text-subheadline);
	}
	.controls {
		display: flex;
		align-items: center;
		gap: var(--space-4);
	}
	.back {
		color: var(--color-accent);
		text-decoration: none;
		font-size: var(--text-subheadline);
		white-space: nowrap;
	}
	.back:hover {
		text-decoration: underline;
	}

	.tablewrap {
		overflow-x: auto;
		border-radius: var(--radius-squircle-lg);
		padding: var(--space-4);
		transition: opacity var(--duration-sm) ease;
	}
	.tablewrap.busy {
		opacity: 0.55;
	}
	@media (prefers-reduced-motion: reduce) {
		.tablewrap {
			transition: none;
		}
	}
	table {
		border-collapse: collapse;
		width: max-content;
		min-width: 100%;
		font-size: var(--text-footnote);
		font-variant-numeric: tabular-nums;
	}
	th,
	td {
		padding: var(--space-2) var(--space-3);
		text-align: right;
		white-space: nowrap;
	}
	thead th {
		font-weight: var(--weight-semibold);
		color: var(--color-text-tertiary);
		border-bottom: 1px solid var(--hairline);
	}
	/* The monitor column is sticky so a name stays readable while the reader
	   scrolls sixty months sideways; without it the row loses its label and the
	   numbers mean nothing. */
	.mon {
		position: sticky;
		left: 0;
		text-align: left;
		background: var(--glass-bg-opaque);
		color: var(--color-text-primary);
		font-weight: var(--weight-semibold);
		z-index: 1;
	}
	tbody tr + tr th,
	tbody tr + tr td {
		border-top: 1px solid var(--hairline);
	}
	tfoot th,
	tfoot td {
		border-top: 1px solid var(--hairline-strong);
		font-weight: var(--weight-semibold);
	}
	.total {
		border-left: 1px solid var(--hairline);
	}
	/* Colour is a second signal. Every cell also carries the number, so the table
	   reads correctly in monochrome and to anyone who cannot separate the hues. */
	td[data-tone='up'] {
		color: var(--status-up);
	}
	td[data-tone='degraded'] {
		color: var(--status-pending);
	}
	td[data-tone='down'] {
		color: var(--status-down);
	}
	td[data-tone='none'] {
		color: var(--color-text-tertiary);
	}
	.foot {
		font-size: var(--text-caption-1);
	}
	.pwform {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
		align-items: flex-start;
	}
	.err {
		margin: 0;
		color: var(--status-down);
		font-size: var(--text-footnote);
	}
</style>
