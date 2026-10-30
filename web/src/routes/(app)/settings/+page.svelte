<script lang="ts">
	import { onMount } from 'svelte';
	import {
		api,
		problemMessage,
		problemStatus,
		type ApiKey,
		type ConfigBundle,
		type ImportResult,
		type Session,
		type AppVersion,
		type Settings,
		type StepUpConfirmation,
		type WebhookFeed
	} from '$lib/api';
	import { configExportUrl, slaReportUrl, type ReportRange } from '$lib/api/client';
	import { session } from '$lib/stores/session.svelte';
	import { monitors } from '$lib/stores/monitors.svelte';
	import RelativeTime from '$lib/components/RelativeTime.svelte';
	import StepUpDialog from '$lib/components/StepUpDialog.svelte';
	import WebhookSettings from '$lib/components/WebhookSettings.svelte';
	import SettingsShell, { type SettingsSection } from '$lib/components/shell/SettingsShell.svelte';
	import {
		t,
		i18n,
		browserTimezone,
		COMMON_TIMEZONES,
		LOCALES,
		formatDateTime,
		formatNumber
	} from '$lib/i18n';
	import {
		Checkbox,
		GlassCard,
		Modal,
		PillButton,
		Select,
		Skeleton,
		TextField,
		toast
	} from '$lib/design-system';

	let settings = $state<Settings | undefined>(undefined);
	let sessions = $state<Session[]>([]);
	let keys = $state<ApiKey[]>([]);
	let savingSettings = $state(false);
	let savingTtl = $state(false);

	// Language + timezone (persisted in the i18n store, per-browser).
	let localePref = $state(i18n.locale);
	$effect(() => i18n.setLocale(localePref));

	const languageOptions = LOCALES.map((l) => ({ value: l.code, label: l.label }));

	// "auto" follows the browser; otherwise a specific IANA zone.
	let tzPref = $state(i18n.timezone ?? 'auto');
	$effect(() => i18n.setTimezone(tzPref === 'auto' ? null : tzPref));

	const timezoneOptions = $derived([
		{ value: 'auto', label: t('settings.timezoneAuto', { zone: browserTimezone() }) },
		// Keep any already-selected zone available even if not in the common list.
		...[...new Set([...COMMON_TIMEZONES, ...(i18n.timezone ? [i18n.timezone] : [])])]
			.sort()
			.map((z) => ({ value: z, label: z }))
	]);

	// Live preview reacts to locale + timezone selection.
	let previewNow = $state(Date.now());
	const datePreview = $derived(formatDateTime(previewNow));
	const numberPreview = $derived(formatNumber(1234567.89));

	// new api key
	let keyModal = $state(false);
	let keyName = $state('');
	let newToken = $state<string | undefined>(undefined);
	let creatingKey = $state(false);

	// Scopes the server actually accepts (see validateScopes: read/write/admin).
	// Default read+write; the old ['monitors:read','monitors:write'] were rejected
	// with a 400, which broke key creation outright.
	const apiKeyScopes = ['read', 'write', 'admin'] as const;
	let keyScopes = $state<string[]>(['read', 'write']);
	// Defaults to 90 days rather than "never". The API keeps "never" as ITS
	// default so no existing integration changes behaviour, but a key minted by
	// hand in this dialog is the one a human can reason about, and an expiry
	// somebody has to opt out of gets rotated far more often than one they have
	// to opt into.
	let keyExpiryDays = $state('90');
	const keyExpiryOptions = $derived([
		{ value: '30', label: t('settings.keyExpiryDays', { n: 30 }) },
		{ value: '90', label: t('settings.keyExpiryDays', { n: 90 }) },
		{ value: '365', label: t('settings.keyExpiryDays', { n: 365 }) },
		{ value: '0', label: t('settings.keyExpiryNever') }
	]);
	function toggleScope(scope: string, on: boolean) {
		keyScopes = on ? [...keyScopes, scope] : keyScopes.filter((s) => s !== scope);
	}

	// Build metadata for the "About" section. Optional: the page is complete
	// without it, so a failure leaves the section saying nothing rather than
	// breaking the rest.
	let version = $state<AppVersion | undefined>(undefined);

	/**
	 * The menu, in Kuma's order. Admin-only sections are left out for everyone
	 * else, exactly as their cards were before.
	 */
	const sections = $derived.by<SettingsSection[]>(() => {
		const out: SettingsSection[] = [
			{
				id: 'general',
				label: t('settings.general'),
				icon: 'M4 6h9m4 0h3M4 12h3m4 0h9M4 18h11m4 0h1M15 4v4M9 10v4M17 16v4'
			},
			{
				id: 'sessions',
				label: t('settings.sessions'),
				icon: 'M3 5h18v11H3zM8 20h8M12 16v4'
			},
			{
				id: 'api-keys',
				label: t('settings.apiKeys'),
				icon: 'M14.5 4a5 5 0 1 1-4.6 7H8v3H5v3H3v-5l6.9-6.9A5 5 0 0 1 14.5 4Zm1.5 4h.01'
			}
		];
		if (session.isAdmin) {
			out.push(
				{
					id: 'webhooks',
					label: t('settingsShell.webhooks'),
					icon: 'M3 12h4l3-8 4 16 3-8h4'
				},
				{
					id: 'backup',
					label: t('settings.dataTransfer'),
					icon: 'M12 3v12m0 0-4-4m4 4 4-4M4 17v4h16v-4'
				}
			);
		}
		out.push(
			{
				id: 'reports',
				label: t('settings.reports'),
				icon: 'M6 3h9l3 3v15H6zM9 9h3M9 13h6M9 17h6'
			},
			{
				id: 'about',
				label: t('settingsShell.about'),
				icon: 'M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18Zm0-10v6m0-9.5h.01'
			}
		);
		return out;
	});

	onMount(async () => {
		api
			.version()
			.then((v) => (version = v))
			.catch(() => (version = undefined));
		[settings, sessions, keys] = await Promise.all([
			api.getSettings(),
			api.listSessions(),
			api.listApiKeys()
		]);
	});

	// Short retention is expressed in days; longer horizons read better in years.
	// The server allows up to MaxHeartbeatRetentionDays = 3650 (~10y), so year
	// options run 1..10. A year is 365 days, so 365 lives in the year group as
	// "1 Jahr" rather than as a duplicate "365 Tage" day entry. There is no i18n
	// key for years, so the label falls back to a plain literal (as several
	// toasts already do); the singular/plural split keeps it grammatical.
	const retentionOptions = $derived([
		...[30, 60, 90, 180].map((d) => ({
			value: String(d),
			label: t('settings.retentionDays', { n: d })
		})),
		...[1, 2, 3, 5, 10].map((y) => ({
			value: String(y * 365),
			label: y === 1 ? t('settings.retentionYear') : t('settings.retentionYears', { n: y })
		}))
	]);

	// Days, not seconds: the server clamps to 1..365 and an operator reasons in
	// "a week" / "a month", never in 604800.
	const sessionTtlOptions = $derived(
		[1, 7, 14, 30, 90, 365].map((d) => ({
			value: String(d),
			label: d === 1 ? t('settings.sessionTtlDay') : t('settings.sessionTtlDays', { n: d })
		}))
	);

	/**
	 * Applies a partial save without discarding what the other cards are still
	 * holding.
	 *
	 * This screen has three independent save buttons over ONE `settings` object,
	 * and every one of them used to do `settings = await api.updateSettings(...)`.
	 * The endpoint answers with the whole record, so saving any card overwrote
	 * every field of every other card with what the server had — silently. Type a
	 * new instance name, then change the session lifetime (which saves on change),
	 * and the name you typed is simply gone, with a green "Saved" toast over it.
	 *
	 * So only the keys this call actually SENT are taken from the response. The
	 * rest keep whatever is in the form, which is either unchanged or a pending
	 * edit — and in both cases it is not this save's business.
	 */
	function applySaved(saved: Settings, sent: (keyof Settings)[]) {
		if (!settings) {
			settings = saved;
			return;
		}
		const next: Settings = { ...settings };
		for (const key of sent) {
			// The narrowing is per-key and the two objects are the same type; the
			// cast is only here because TS cannot see that across a loop variable.
			(next[key] as Settings[typeof key]) = saved[key];
		}
		settings = next;
	}

	// Saved on change rather than via the General card's button, which is a
	// different card and would read as unrelated to this control.
	async function saveSessionTtl(days: number) {
		if (!settings) return;
		const previous = settings.sessionTtlDays;
		settings.sessionTtlDays = days;
		savingTtl = true;
		try {
			applySaved(await api.updateSettings({ sessionTtlDays: days }), ['sessionTtlDays']);
			toast.success(t('settings.saved'));
		} catch (e) {
			if (settings) settings.sessionTtlDays = previous;
			toast.error(problemMessage(e) ?? 'Could not save settings.');
		} finally {
			savingTtl = false;
		}
	}

	async function saveSettings() {
		if (!settings) return;
		savingSettings = true;
		try {
			applySaved(
				await api.updateSettings({
					instanceName: settings.instanceName,
					baseUrl: settings.baseUrl,
					retentionDays: Number(settings.retentionDays),
					defaultInterval: settings.defaultInterval
				}),
				['instanceName', 'baseUrl', 'retentionDays', 'defaultInterval']
			);
			toast.success(t('settings.saved'));
		} catch (e) {
			toast.error(problemMessage(e) ?? 'Could not save settings.');
		} finally {
			savingSettings = false;
		}
	}

	// ---- webhooks ------------------------------------------------------------
	//
	// Each URL is saved on its own rather than folded into saveSettings, because
	// it is free text whose server-side rejection an operator cannot predict from
	// the control: retention and session lifetime are pick-lists that cannot
	// produce an out-of-range value, while this has to be an absolute http/https
	// URL or empty. A shared save would report "could not save settings" for a bad
	// URL and leave the other fields' fate ambiguous.
	const webhookUrlField = {
		audit: 'auditWebhookUrl',
		outbound: 'outboundWebhookUrl'
	} as const satisfies Record<WebhookFeed, keyof Settings>;
	const webhookSecretFlag = {
		audit: 'auditWebhookSecretSet',
		outbound: 'outboundWebhookSecretSet'
	} as const satisfies Record<WebhookFeed, keyof Settings>;

	async function saveWebhookUrl(feed: WebhookFeed, url: string) {
		const field = webhookUrlField[feed];
		// Checked here as well as on the server, so a typo is caught before a round
		// trip; the server check is the one that counts, since the settings table is
		// also writable directly in psql.
		if (url !== '' && !/^https?:\/\/[^\s/]+/i.test(url)) {
			toast.error(t('settings.auditWebhookInvalid'));
			throw new Error('invalid url');
		}
		try {
			applySaved(await api.updateSettings({ [field]: url }), [field]);
			toast.success(t('settings.saved'));
		} catch (e) {
			toast.error(problemMessage(e) ?? t('settings.auditWebhookInvalid'));
			throw e;
		}
	}

	async function saveWebhookSecret(feed: WebhookFeed, secret: string) {
		try {
			applySaved(await api.setWebhookSecret(feed, secret), [webhookSecretFlag[feed]]);
		} catch (e) {
			toast.error(problemMessage(e) ?? t('settings.webhookSecretInvalid'));
			throw e;
		}
	}

	async function revokeSession(id: string) {
		await api.revokeSession(id);
		sessions = sessions.filter((s) => s.id !== id);
		toast.success(t('settings.sessionRevoked'));
	}

	// An API key outlives every session and no password change revokes it, so
	// the server re-authenticates before minting one: a borrowed session must
	// not be convertible into standing access.
	let keyConfirmOpen = $state(false);

	function askForKeyConfirmation() {
		if (!keyName.trim()) {
			toast.error(t('settings.nameKey'));
			return;
		}
		keyConfirmOpen = true;
	}

	async function createKey(confirm: StepUpConfirmation) {
		creatingKey = true;
		try {
			const k = await api.createApiKey({
				name: keyName,
				scopes: keyScopes,
				// '0' is the "never" option, and the client omits the field
				// entirely for a falsy value — the server rejects a literal 0.
				expiresInDays: Number(keyExpiryDays) || undefined,
				confirm
			});
			newToken = k.token;
			keys = [...keys, { ...k, token: undefined }];
			keyName = '';
			keyScopes = ['read', 'write'];
			keyExpiryDays = '90';
		} catch (e) {
			// 403 is the re-authentication failing; the dialog shows it inline
			// rather than closing and replacing it with a toast.
			if (problemStatus(e) === 403) throw e;
			toast.error(problemMessage(e) ?? 'Could not create the API key.');
		} finally {
			creatingKey = false;
		}
	}
	async function revokeKey(id: string) {
		await api.revokeApiKey(id);
		keys = keys.filter((k) => k.id !== id);
		toast.success(t('settings.keyRevoked'));
	}
	function closeKeyModal() {
		keyModal = false;
		newToken = undefined;
	}

	// ---- Import & export (admin) ----------------------------------------------
	// Config export and the SLA reports are same-origin file downloads, so they
	// render as <a href download> against the URL helpers — there is nothing to
	// fetch. Config/Kuma import read the picked file, parse it in the browser and
	// POST the parsed value; a config restore is confirmed first because it writes
	// to the whole instance.
	let configInput = $state<HTMLInputElement>();
	let kumaInput = $state<HTMLInputElement>();
	let importing = $state(false);
	let pendingBundle = $state<ConfigBundle | undefined>(undefined);
	let confirmImport = $state(false);
	let skipped = $state<string[]>([]);
	let skippedOpen = $state(false);

	// Parses the picked file as JSON and clears the input so re-picking the same
	// file fires `change` again. Returns undefined (and toasts) on a non-JSON file.
	async function readJson(input: HTMLInputElement): Promise<unknown | undefined> {
		const file = input.files?.[0];
		input.value = '';
		if (!file) return undefined;
		try {
			return JSON.parse(await file.text());
		} catch {
			toast.error(t('settings.importBadFile'));
			return undefined;
		}
	}

	function reportResult(r: ImportResult) {
		// A config / Uptime Kuma import bulk-inserts monitors server-side, and the
		// WebSocket hub does not replay those rows, so the cached dashboard store
		// would keep showing the old list until a manual reload. Force a refresh
		// here — reached by both import flows — before surfacing the result.
		void monitors.reload();
		toast.success(
			t('settings.importDone', {
				monitors: r.monitors,
				notifications: r.notifications,
				tags: r.tags,
				pages: r.statusPages
			})
		);
		if (r.warnings.length > 0) {
			skipped = r.warnings;
			skippedOpen = true;
		}
	}

	async function onConfigChosen() {
		if (!configInput) return;
		const parsed = await readJson(configInput);
		if (parsed === undefined) return;
		// The bundle is the verbatim export payload — already snake_case — so it is
		// held as-is for the confirmed POST, not remapped.
		pendingBundle = parsed as ConfigBundle;
		confirmImport = true;
	}

	function cancelImport() {
		if (importing) return;
		confirmImport = false;
		pendingBundle = undefined;
	}

	async function runConfigImport() {
		if (!pendingBundle) return;
		importing = true;
		try {
			const r = await api.importConfig(pendingBundle);
			confirmImport = false;
			pendingBundle = undefined;
			reportResult(r);
		} catch (e) {
			toast.error(problemMessage(e) ?? t('settings.importError'));
		} finally {
			importing = false;
		}
	}

	async function onKumaChosen() {
		if (!kumaInput) return;
		// Sent as raw bytes: modern Uptime-Kuma has no JSON export, so this is
		// normally the binary kuma.db SQLite file. The server auto-detects a
		// SQLite database vs a legacy JSON export, so both work through this path.
		const file = kumaInput.files?.[0];
		kumaInput.value = '';
		if (!file) return;
		importing = true;
		try {
			reportResult(await api.importUptimeKuma(await file.arrayBuffer()));
		} catch (e) {
			toast.error(problemMessage(e) ?? t('settings.importError'));
		} finally {
			importing = false;
		}
	}

	// ---- SLA reports ----------------------------------------------------------
	let reportRange = $state<ReportRange>('30d');
	const reportRanges = $derived([
		{ value: '24h', label: t('settings.reportRange24h') },
		{ value: '7d', label: t('settings.reportRange7d') },
		{ value: '30d', label: t('settings.reportRange30d') },
		{ value: '1y', label: t('settings.reportRange1y') }
	]);
</script>

<svelte:head><title>{t('settings.pageTitle')}</title></svelte:head>

<SettingsShell {sections}>
	{#snippet children(current: string)}
		{#if current === 'general'}
			<GlassCard>
				{#if !settings}
					<Skeleton lines={3} />
				{:else}
					<div class="grid">
						<TextField label={t('settings.instanceName')} bind:value={settings.instanceName} />
						<TextField label={t('settings.baseUrl')} bind:value={settings.baseUrl} />
						<div class="field">
							<Select
								label={t('settings.retention')}
								value={String(settings.retentionDays)}
								options={retentionOptions}
								onchange={(v) => settings && (settings.retentionDays = Number(v))}
							/>
							<p class="hint">{t('settings.retentionHelp')}</p>
						</div>
					</div>
					<div class="save">
						<PillButton loading={savingSettings} onclick={saveSettings}
							>{t('settings.saveChanges')}</PillButton
						>
					</div>
				{/if}
			</GlassCard>

			<GlassCard title={t('settings.languageRegion')} subtitle={t('settings.languageRegionSub')}>
				<div class="grid">
					<Select
						label={t('settings.language')}
						bind:value={localePref}
						options={languageOptions}
					/>
					<Select label={t('settings.timezone')} bind:value={tzPref} options={timezoneOptions} />
				</div>
				<p class="preview">
					<span class="pk">{t('settings.datePreview')}</span>
					<span class="pv">{datePreview} · {numberPreview}</span>
				</p>
			</GlassCard>
		{:else if current === 'sessions'}
			<GlassCard subtitle={t('settings.sessionsSub')}>
				{#if settings}
					<div class="ttl">
						<Select
							label={t('settings.sessionTtl')}
							value={String(settings.sessionTtlDays)}
							options={sessionTtlOptions}
							disabled={savingTtl}
							onchange={(v) => saveSessionTtl(Number(v))}
						/>
						<p class="hint">{t('settings.sessionTtlHelp')}</p>
					</div>
				{/if}
				<ul class="rows">
					{#each sessions as s (s.id)}
						<li>
							<div>
								<strong>{s.userAgent}</strong>
								{#if s.current}<span class="cur">{t('settings.thisDevice')}</span>{/if}
								<span class="sub"
									>{s.ip} · {t('settings.activePrefix')} <RelativeTime value={s.lastSeenAt} /></span
								>
							</div>
							{#if !s.current}
								<PillButton variant="ghost" size="sm" onclick={() => revokeSession(s.id)}
									>{t('common.revoke')}</PillButton
								>
							{/if}
						</li>
					{/each}
				</ul>
			</GlassCard>
		{:else if current === 'api-keys'}
			<GlassCard subtitle={t('settings.apiKeysSub')}>
				{#snippet actions()}
					<PillButton size="sm" onclick={() => (keyModal = true)}>{t('settings.newKey')}</PillButton
					>
				{/snippet}
				{#if keys.length === 0}
					<p class="muted">{t('settings.noKeys')}</p>
				{:else}
					<ul class="rows">
						{#each keys as k (k.id)}
							<li>
								<div>
									<strong>{k.name}</strong>
									<span class="sub">
										<code>{k.prefix}…</code> · {k.scopes.join(', ')} · {t('settings.lastUsed')}
										<RelativeTime value={k.lastUsedAt} />
										{#if k.expiresAt}
											·
											{#if new Date(k.expiresAt) <= new Date()}
												<span class="expired">{t('settings.keyExpired')}</span>
											{:else}
												{t('settings.keyExpires')}
												<RelativeTime value={k.expiresAt} />
											{/if}
										{/if}
									</span>
								</div>
								<PillButton variant="ghost" size="sm" onclick={() => revokeKey(k.id)}
									>{t('common.revoke')}</PillButton
								>
							</li>
						{/each}
					</ul>
				{/if}
			</GlassCard>
		{:else if current === 'webhooks' && session.isAdmin}
			{#if !settings}
				<GlassCard title={t('settings.statusWebhook')}><Skeleton lines={2} /></GlassCard>
				<GlassCard title={t('settings.auditForwarding')}><Skeleton lines={2} /></GlassCard>
			{:else}
				<WebhookSettings
					title={t('settings.statusWebhook')}
					subtitle={t('settings.statusWebhookSub')}
					urlLabel={t('settings.statusWebhookUrl')}
					urlHint={t('settings.statusWebhookHint')}
					urlPlaceholder="https://n8n.example/webhook/statwarden"
					bind:url={settings.outboundWebhookUrl}
					secretSet={settings.outboundWebhookSecretSet}
					onsaveurl={(u) => saveWebhookUrl('outbound', u)}
					onsetsecret={(s) => saveWebhookSecret('outbound', s)}
				/>
				<WebhookSettings
					title={t('settings.auditForwarding')}
					urlLabel={t('settings.auditWebhookUrl')}
					urlHint={t('settings.auditWebhookHint')}
					urlPlaceholder="https://collector.example/statwarden"
					bind:url={settings.auditWebhookUrl}
					secretSet={settings.auditWebhookSecretSet}
					onsaveurl={(u) => saveWebhookUrl('audit', u)}
					onsetsecret={(s) => saveWebhookSecret('audit', s)}
				/>
			{/if}
		{:else if current === 'backup' && session.isAdmin}
			<GlassCard subtitle={t('settings.dataTransferSub')}>
				<div class="xfer">
					<div class="xfer-row">
						<div class="xfer-txt">
							<strong>{t('settings.exportConfig')}</strong>
							<p class="hint">{t('settings.exportConfigSub')}</p>
						</div>
						<a class="dl" href={configExportUrl()} download>{t('settings.exportDownload')}</a>
					</div>
					<div class="xfer-row">
						<div class="xfer-txt">
							<strong>{t('settings.importConfig')}</strong>
							<p class="hint">{t('settings.importConfigSub')}</p>
						</div>
						<PillButton
							variant="secondary"
							size="sm"
							disabled={importing}
							onclick={() => configInput?.click()}>{t('settings.importChoose')}</PillButton
						>
					</div>
					<div class="xfer-row">
						<div class="xfer-txt">
							<strong>{t('settings.importKuma')}</strong>
							<p class="hint">{t('settings.importKumaSub')}</p>
						</div>
						<PillButton
							variant="secondary"
							size="sm"
							disabled={importing}
							onclick={() => kumaInput?.click()}>{t('settings.importKumaChoose')}</PillButton
						>
					</div>
				</div>
				<input
					bind:this={configInput}
					type="file"
					accept="application/json,.json"
					hidden
					onchange={onConfigChosen}
				/>
				<input
					bind:this={kumaInput}
					type="file"
					accept=".db,.sqlite,.sqlite3,application/json,.json"
					hidden
					onchange={onKumaChosen}
				/>
			</GlassCard>
		{:else if current === 'reports'}
			<GlassCard subtitle={t('settings.reportsSub')}>
				<div class="reports">
					<div class="report-range">
						<Select
							label={t('settings.reportRange')}
							value={reportRange}
							options={reportRanges}
							onchange={(v) => (reportRange = v as ReportRange)}
						/>
					</div>
					<div class="report-actions">
						<a class="dl" href={slaReportUrl('csv', { range: reportRange })} download
							>{t('settings.reportCsv')}</a
						>
						<a class="dl" href={slaReportUrl('pdf', { range: reportRange })} download
							>{t('settings.reportPdf')}</a
						>
					</div>
				</div>
			</GlassCard>
		{:else if current === 'about'}
			<div class="about">
				<span class="about-mark" aria-hidden="true">
					<svg viewBox="0 0 64 64"
						><path
							d="M32 7 50 14v15c0 12-7.7 21.6-18 26-10.3-4.4-18-14-18-26V14L32 7Zm-12 25h7l4-8 6 16 4-8h3"
							fill="none"
							stroke="currentColor"
							stroke-width="4.5"
							stroke-linecap="round"
							stroke-linejoin="round"
						/></svg
					>
				</span>
				<strong class="about-name">Statwarden</strong>
				{#if version}
					<dl class="about-meta">
						<dt>{t('settingsShell.version')}</dt>
						<dd>{version.version}</dd>
						{#if version.commit}
							<dt>{t('settingsShell.commit')}</dt>
							<dd><code>{version.commit}</code></dd>
						{/if}
						{#if version.buildDate}
							<dt>{t('settingsShell.buildDate')}</dt>
							<dd>{version.buildDate}</dd>
						{/if}
						<dt>{t('settingsShell.goVersion')}</dt>
						<dd>{version.goVersion}</dd>
					</dl>
				{:else}
					<Skeleton lines={2} />
				{/if}
				<a class="dl" href="/api/v1/docs">{t('settingsShell.apiDocs')}</a>
			</div>
		{/if}
	{/snippet}
</SettingsShell>

<Modal
	open={confirmImport}
	title={t('settings.importConfig')}
	onclose={cancelImport}
	dismissable={!importing}
>
	<p class="tokintro">{t('settings.importConfigSub')}</p>
	{#snippet footer()}
		<PillButton variant="ghost" disabled={importing} onclick={cancelImport}
			>{t('common.cancel')}</PillButton
		>
		<PillButton loading={importing} onclick={runConfigImport}
			>{t('settings.importConfig')}</PillButton
		>
	{/snippet}
</Modal>

<Modal
	open={skippedOpen}
	title={t('settings.importSkippedTitle')}
	onclose={() => (skippedOpen = false)}
>
	<p class="tokintro">{t('settings.importSkipped', { n: skipped.length })}</p>
	<ul class="warn">
		{#each skipped as w, i (i)}
			<li>{w}</li>
		{/each}
	</ul>
	{#snippet footer()}
		<PillButton onclick={() => (skippedOpen = false)}>{t('common.done')}</PillButton>
	{/snippet}
</Modal>

<Modal open={keyModal} title={t('settings.newApiKey')} onclose={closeKeyModal}>
	{#if newToken}
		<p class="tokintro">{t('settings.tokenIntro')}</p>
		<code class="token">{newToken}</code>
	{:else}
		<TextField
			label={t('settings.keyName')}
			bind:value={keyName}
			placeholder={t('settings.keyNamePlaceholder')}
			required
		/>
		<div class="scopes">
			{#each apiKeyScopes as scope (scope)}
				<Checkbox
					label={scope}
					checked={keyScopes.includes(scope)}
					onchange={(on) => toggleScope(scope, on)}
				/>
			{/each}
		</div>
		<Select label={t('settings.keyExpiry')} options={keyExpiryOptions} bind:value={keyExpiryDays} />
	{/if}
	{#snippet footer()}
		{#if newToken}
			<PillButton onclick={closeKeyModal}>{t('common.done')}</PillButton>
		{:else}
			<PillButton variant="ghost" onclick={closeKeyModal}>{t('common.cancel')}</PillButton>
			<PillButton loading={creatingKey} onclick={askForKeyConfirmation}>
				{t('settings.createKey')}
			</PillButton>
		{/if}
	{/snippet}
</Modal>

<StepUpDialog
	bind:open={keyConfirmOpen}
	title={t('settings.createKey')}
	intro={t('settings.createKeyConfirmBody')}
	confirmLabel={t('settings.createKey')}
	onconfirm={createKey}
/>

<style>
	/* Kuma's About page: the mark, the name and the build, centred. */
	.about {
		display: flex;
		flex-direction: column;
		align-items: center;
		gap: var(--space-5);
		padding: var(--space-8) 0;
		text-align: center;
	}
	.about-mark {
		display: inline-grid;
		place-items: center;
		width: 6rem;
		height: 6rem;
		border-radius: var(--radius-squircle-md);
		background: var(--color-accent-tint);
		border: 1px solid var(--hairline-strong);
	}
	.about-mark svg {
		width: 3.75rem;
		height: 3.75rem;
	}
	.about-name {
		font-size: var(--text-title-1);
		font-weight: var(--weight-bold);
		letter-spacing: var(--tracking-tight);
	}
	.about-meta {
		display: grid;
		grid-template-columns: auto auto;
		gap: var(--space-3) var(--space-6);
		margin: 0;
		font-size: var(--text-subheadline);
		text-align: left;
	}
	.about-meta dt {
		color: var(--color-text-secondary);
	}
	.about-meta dd {
		margin: 0;
		font-weight: var(--weight-medium);
	}
	.expired {
		color: var(--status-down-text);
		font-weight: 600;
	}
	.grid {
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: var(--space-4);
	}
	.save {
		margin-top: var(--space-5);
		display: flex;
		justify-content: flex-end;
	}
	.ttl {
		display: grid;
		gap: var(--space-2);
		max-width: 22rem;
		padding-bottom: var(--space-4);
		margin-bottom: var(--space-2);
		border-bottom: 1px solid var(--hairline);
	}
	.hint {
		margin: 0;
		font-size: var(--text-caption-1);
		color: var(--color-text-tertiary);
		line-height: 1.45;
	}
	.rows {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}
	.rows li {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-4);
		padding: var(--space-3) 0;
		border-bottom: 1px solid var(--hairline);
	}
	.rows li:last-child {
		border-bottom: 0;
	}
	.cur {
		margin-left: var(--space-3);
		font-size: var(--text-caption-2);
		padding: 1px var(--space-3);
		border-radius: var(--radius-pill);
		background: var(--status-up-tint);
		color: var(--status-up-text);
	}
	.sub {
		display: block;
		font-size: var(--text-caption-1);
		color: var(--color-text-tertiary);
		margin-top: 2px;
	}
	.muted {
		margin: 0;
		color: var(--color-text-tertiary);
		font-size: var(--text-subheadline);
	}
	.preview {
		display: flex;
		align-items: baseline;
		gap: var(--space-3);
		flex-wrap: wrap;
		margin: var(--space-4) 0 0;
	}
	.preview .pk {
		font-size: var(--text-caption-1);
		color: var(--color-text-tertiary);
	}
	.preview .pv {
		font-size: var(--text-subheadline);
		font-weight: var(--weight-medium);
		font-variant-numeric: tabular-nums;
	}
	code {
		font-family: var(--font-mono);
	}
	.tokintro {
		margin: 0 0 var(--space-4);
		font-size: var(--text-subheadline);
		color: var(--color-text-secondary);
	}
	.scopes {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-4);
		margin-top: var(--space-4);
	}
	.token {
		display: block;
		padding: var(--space-4);
		border-radius: var(--radius-squircle-sm);
		background: var(--glass-bg-thin);
		border: 1px solid var(--hairline);
		font-size: var(--text-footnote);
		word-break: break-all;
	}
	.xfer {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}
	.xfer-row {
		display: flex;
		align-items: flex-start;
		justify-content: space-between;
		gap: var(--space-4);
		padding-bottom: var(--space-4);
		border-bottom: 1px solid var(--hairline);
	}
	.xfer-row:last-child {
		padding-bottom: 0;
		border-bottom: 0;
	}
	.xfer-txt strong {
		display: block;
		font-size: var(--text-subheadline);
	}
	.xfer-txt .hint {
		margin-top: 2px;
		max-width: 34rem;
	}
	.reports {
		display: flex;
		align-items: flex-end;
		gap: var(--space-4);
		flex-wrap: wrap;
	}
	.report-range {
		flex: 1 1 14rem;
		max-width: 20rem;
	}
	.report-actions {
		display: flex;
		gap: var(--space-3);
		padding-bottom: 2px;
	}
	.warn {
		margin: var(--space-3) 0 0;
		padding-left: var(--space-5);
		font-size: var(--text-footnote);
		color: var(--color-text-secondary);
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	/* Download links are real <a href download>, styled to read as secondary
	   pill buttons (PillButton renders a <button>, which cannot carry href). */
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
	.dl:focus-visible {
		outline: none;
		box-shadow: var(--focus-ring);
	}
	@media (prefers-reduced-motion: reduce) {
		.dl,
		.dl:hover,
		.dl:active {
			transition: none;
			transform: none;
		}
	}
	@media (max-width: 640px) {
		.grid {
			grid-template-columns: 1fr;
		}
		.xfer-row {
			flex-direction: column;
			align-items: stretch;
		}
	}
</style>
