<script lang="ts">
	// Create/edit form with per-type config fields driven by the monitor-type
	// registry (registry/monitor-types). Selecting a type swaps the config
	// schema; `showIf` rules toggle dependent fields. Also assigns tags and
	// notification channels.
	import { untrack } from 'svelte';
	import { goto } from '$app/navigation';
	import {
		api,
		configFromForm,
		configToForm,
		isFieldVisible,
		monitorTypeSpecs,
		problemMessage,
		type Monitor,
		type MonitorGroup,
		type Notification,
		type Tag
	} from '$lib/api';
	import { monitors } from '$lib/stores/monitors.svelte';
	import { t } from '$lib/i18n';
	import ConfigFields from './ConfigFields.svelte';
	import {
		Checkbox,
		GlassCard,
		PillButton,
		PillToggle,
		Select,
		TextArea,
		TextField,
		toast
	} from '$lib/design-system';

	let {
		existing,
		template
	}: {
		/** Present only when editing; also the flag that makes submit a PUT. */
		existing?: Monitor;
		/**
		 * Clone source. Seeds the same fields as `existing` but leaves this a
		 * CREATE, which is the whole point: the copy must get its own id and its
		 * own push token. Nothing from the source is spread into the payload —
		 * only what the form actually collects is sent — so the unique columns
		 * cannot ride along even by accident. (push_token carries a partial UNIQUE
		 * index in 0001_init; monitorRequest has no field for it at all, and the
		 * server mints one per monitor.)
		 */
		template?: Monitor;
	} = $props();

	let tags = $state<Tag[]>([]);
	let channels = $state<Notification[]>([]);
	// Guards applyDefaultChannels against a second channel load re-ticking boxes
	// the operator has since unticked.
	let defaultsApplied = $state(false);
	let groups = $state<MonitorGroup[]>([]);
	// The full monitor list backs the parent picker: the exclusions below need
	// every monitor's own parent, not just the candidates' names.
	let allMonitors = $state<Monitor[]>([]);
	$effect(() => {
		api.listTags().then((t) => (tags = t));
		api.listNotifications().then((n) => {
			channels = n;
			applyDefaultChannels(n);
		});
		api.listMonitorGroups().then((g) => (groups = g));
		api.listMonitors().then((m) => (allMonitors = m));
	});

	/**
	 * Pre-select the channels flagged as defaults, for a genuinely NEW monitor.
	 *
	 * The server applies the same rule when `notification_ids` is absent, but
	 * this form always sends the field, so the server rule would never fire here.
	 * Doing it in the form is also the better half of the deal: the boxes are
	 * ticked visibly and can be unticked, whereas a server-side attach would
	 * silently add channels the operator never saw — and unticking them would be
	 * undone on the next save.
	 *
	 * Not for a clone: a copy inherits the source monitor's channels, and adding
	 * the defaults on top would quietly widen who gets paged. Not for an edit
	 * either — `seed` is set there, and re-adding a default the operator removed
	 * would make removing one impossible.
	 *
	 * Only active channels, matching the server: a disabled one cannot deliver.
	 * `enabled` is this model's name for the wire's `active` (see
	 * notificationFromWire).
	 */
	function applyDefaultChannels(all: Notification[]) {
		if (seed || defaultsApplied || notificationIds.length > 0) return;
		defaultsApplied = true;
		notificationIds = all.filter((c) => c.isDefault && c.enabled).map((c) => c.id);
	}

	// ---- form state (seeded once from `existing`; the edit route only mounts
	// this form after the monitor has loaded, so a one-time snapshot is right) ----
	const seed = untrack(() => existing ?? template);
	const isClone = untrack(() => existing == null && template != null);
	// A clone opens under an obviously-derived name so it can never be mistaken
	// for the original in the list, and so saving without touching the field
	// cannot silently produce two identically named monitors.
	let name = $state(
		isClone && seed ? t('monitorForm.copyOfName', { name: seed.name }) : (seed?.name ?? '')
	);
	let type = $state(seed?.type ?? 'http');
	let interval = $state(String(seed?.interval ?? 60));
	let timeout = $state(String(seed?.timeout ?? 30));
	let maxRetries = $state(String(seed?.maxRetries ?? 3));
	let recoveryChecks = $state(String(seed?.recoveryChecks ?? 1));
	let retryInterval = $state(String(seed?.retryInterval ?? seed?.interval ?? 60));
	let resendInterval = $state(String(seed?.resendInterval ?? 0));
	let upsideDown = $state(seed?.upsideDown ?? false);
	let description = $state(seed?.description ?? '');
	let externalUrl = $state(seed?.externalUrl ?? '');
	let tagIds = $state<string[]>(seed?.tagIds ?? []);
	let notificationIds = $state<string[]>(seed?.notificationIds ?? []);
	// '' is the "none" choice for both pickers; Select works in strings and the
	// mappers turn an empty selection back into a null foreign key.
	let parentId = $state(seed?.parentId ?? '');
	let groupId = $state(seed?.groupId ?? '');
	let newGroupName = $state('');
	let creatingGroup = $state(false);
	// Form state, not wire state: arrays and objects in the saved config are
	// unpacked into the text their widgets can hold, and configFromForm packs
	// them back on submit.
	const seedSpec = monitorTypeSpecs.find((s) => s.type === (seed?.type ?? 'http'));
	let config = $state<Record<string, unknown>>(
		configToForm(seedSpec?.fields ?? [], seed?.config ?? {})
	);
	let saving = $state(false);
	let errors = $state<Record<string, string>>({});

	const spec = $derived(monitorTypeSpecs.find((s) => s.type === type) ?? monitorTypeSpecs[0]);

	/**
	 * Parent candidates. The SERVER is the authority on cycles — it walks the
	 * stored chain on every write and answers 400 — but a picker that offers a
	 * choice guaranteed to be rejected is a bad picker, so the two obviously
	 * cyclic classes are filtered here as well: the monitor itself, and anything
	 * that already descends from it (A→B→A and longer).
	 *
	 * The hop cap mirrors maxParentChain in internal/api/monitors.go. It is a
	 * safety stop, not a product limit: if stored data somehow already contains a
	 * loop, this walk still terminates instead of hanging the editor.
	 */
	const parentOptions = $derived.by(() => {
		const selfId = existing?.id;
		const byId = new Map(allMonitors.map((m) => [m.id, m]));
		const descendsFromSelf = (m: Monitor): boolean => {
			let cur = m.parentId;
			for (let hops = 0; cur != null && hops < 64; hops++) {
				if (cur === selfId) return true;
				cur = byId.get(cur)?.parentId;
			}
			return false;
		};
		return [
			{ value: '', label: t('monitorForm.noParent') },
			...allMonitors
				.filter((m) => m.id !== selfId && !descendsFromSelf(m))
				.map((m) => ({ value: m.id, label: m.name }))
		];
	});

	const groupOptions = $derived([
		{ value: '', label: t('monitorForm.noGroup') },
		...groups.map((g) => ({ value: g.id, label: g.name }))
	]);

	/** Creates a folder inline and selects it, so an empty list is not a dead end. */
	async function createGroup() {
		const name = newGroupName.trim();
		if (!name) return;
		creatingGroup = true;
		try {
			const g = await api.createMonitorGroup({ name, sortOrder: groups.length });
			groups = [...groups, g];
			groupId = g.id;
			newGroupName = '';
		} catch (err) {
			toast.error(problemMessage(err) ?? t('monitorForm.groupCreateError'));
		} finally {
			creatingGroup = false;
		}
	}

	function toggleTag(idv: string) {
		tagIds = tagIds.includes(idv) ? tagIds.filter((x) => x !== idv) : [...tagIds, idv];
	}
	function toggleChannel(idv: string) {
		notificationIds = notificationIds.includes(idv)
			? notificationIds.filter((x) => x !== idv)
			: [...notificationIds, idv];
	}

	function validate(): boolean {
		const e: Record<string, string> = {};
		if (!name.trim()) e.name = t('monitorForm.nameRequired');
		for (const f of spec.fields) {
			if (!isFieldVisible(f, config)) continue;
			if (f.required && !config[f.key])
				e[`cfg_${f.key}`] = t('monitorForm.fieldRequired', { field: t(f.label) });
		}
		errors = e;
		return Object.keys(e).length === 0;
	}

	async function submit(ev: SubmitEvent) {
		ev.preventDefault();
		if (!validate()) {
			toast.error(t('monitorForm.fixFields'));
			return;
		}
		saving = true;
		try {
			// The monitor write is full-replace: spreading `existing` carries the
			// fields this form does not edit through the save instead of letting the
			// mapper's fallbacks rewrite them. `url` is not sent at all — the server
			// derives it from config.
			const payload: Partial<Monitor> = {
				...existing,
				name,
				type,
				interval: Number(interval),
				timeout: Number(timeout),
				maxRetries: Number(maxRetries),
				recoveryChecks: Number(recoveryChecks) || 1,
				retryInterval: Number(retryInterval),
				resendInterval: Number(resendInterval),
				upsideDown,
				description,
				externalUrl: externalUrl || undefined,
				// Both foreign keys are cleared with undefined, which the mappers
				// send as an explicit null — a full-replace write has no other way
				// to say "no parent" or "no folder".
				parentId: parentId || undefined,
				groupId: groupId || undefined,
				tagIds,
				notificationIds,
				// The only place form state becomes a wire config: applies each
				// field's `shape`, drops empty strings (a `"port": ""` fails to
				// decode into an int) and drops keys left behind by an earlier
				// type selection.
				config: configFromForm(spec.fields, $state.snapshot(config))
			};
			if (existing) {
				monitors.replace(await api.updateMonitor(existing.id, payload));
				toast.success(t('monitorForm.saved'));
				await goto(`/monitors/${existing.id}`);
			} else {
				const created = await api.createMonitor(payload);
				monitors.replace(created);
				toast.success(t('monitorForm.created'));
				await goto(`/monitors/${created.id}`);
			}
		} catch (err) {
			// Surface the server's own explanation rather than a generic failure:
			// a rejected parent is the operator's input to fix, and only the
			// server can detect a cycle formed by two concurrent edits. The
			// problem detail names the field it rejected (decision C), so a
			// mention of parent_id is what pins the message to that control —
			// nothing else in this payload can produce that word.
			const detail = problemMessage(err);
			if (detail?.includes('parent_id')) errors = { ...errors, parentId: detail };
			toast.error(detail ?? t('monitorForm.saveError'));
		} finally {
			saving = false;
		}
	}

	const typeOptions = monitorTypeSpecs.map((s) => ({
		value: s.type,
		label: `${t(s.label)} (${s.family})`
	}));

	const intervalPresets = [30, 60, 120, 300, 900, 3600];
	function intervalLabel(s: number): string {
		if (s < 60) return t('monitorForm.intervalSeconds', { n: s });
		if (s === 3600) return t('monitorForm.intervalHour');
		if (s % 60 === 0) return t('monitorForm.intervalMinutes', { n: s / 60 });
		// Off-preset, non-whole-minute (e.g. 90s) reads clearest in seconds.
		return t('monitorForm.intervalSeconds', { n: s });
	}
	// An imported monitor can carry an off-preset interval — Uptime-Kuma allows any
	// number of seconds (20, 45, …). Inject the current value as its own option so
	// the Select shows and keeps it instead of a blank "Select…".
	const intervalOptions = $derived.by(() => {
		const cur = Number(interval);
		const values =
			Number.isInteger(cur) && cur > 0 && !intervalPresets.includes(cur)
				? [...intervalPresets, cur].sort((a, b) => a - b)
				: intervalPresets;
		return values.map((s) => ({ value: String(s), label: intervalLabel(s) }));
	});
</script>

<form onsubmit={submit} class="form">
	<GlassCard title={t('monitorForm.basics')}>
		<div class="grid">
			<TextField
				label={t('monitorForm.name')}
				bind:value={name}
				placeholder={t('monitorForm.namePlaceholder')}
				error={errors.name}
				required
			/>
			<Select label={t('monitorForm.type')} bind:value={type} options={typeOptions} />
		</div>
		<p class="spec-desc">{t(spec.description)}</p>
	</GlassCard>

	<GlassCard title={t('monitorForm.configuration', { type: t(spec.label) })}>
		<div class="grid">
			<ConfigFields fields={spec.fields} bind:config {errors} />
		</div>
	</GlassCard>

	<GlassCard title={t('monitorForm.scheduling')}>
		<div class="grid">
			<Select
				label={t('monitorForm.checkInterval')}
				bind:value={interval}
				options={intervalOptions}
			/>
			<TextField label={t('monitorForm.timeoutSec')} type="number" bind:value={timeout} />
			<TextField label={t('monitorForm.maxRetries')} type="number" bind:value={maxRetries} />
			<TextField
				label={t('monitorForm.recoveryChecks')}
				type="number"
				min="1"
				max="100"
				bind:value={recoveryChecks}
				hint={t('monitorForm.recoveryChecksHint')}
			/>
			<TextField
				label={t('monitorForm.retryIntervalSec')}
				type="number"
				min="1"
				bind:value={retryInterval}
				hint={t('monitorForm.retryIntervalHint')}
			/>
			<TextField
				label={t('monitorForm.resendIntervalMin')}
				type="number"
				min="0"
				bind:value={resendInterval}
				hint={t('monitorForm.resendIntervalHint')}
			/>
			<div class="fullrow">
				<PillToggle label={t('monitorForm.upsideDownMode')} bind:checked={upsideDown} />
			</div>
		</div>
	</GlassCard>

	<GlassCard title={t('monitorForm.organisation')}>
		<div class="grid">
			<div>
				<Select
					label={t('monitorForm.parent')}
					bind:value={parentId}
					options={parentOptions}
					placeholder={t('monitorForm.noParent')}
				/>
				{#if errors.parentId}
					<p class="field-error">{errors.parentId}</p>
				{:else}
					<p class="field-hint">{t('monitorForm.parentHint')}</p>
				{/if}
			</div>
			<div>
				<Select
					label={t('monitorForm.group')}
					bind:value={groupId}
					options={groupOptions}
					placeholder={t('monitorForm.noGroup')}
				/>
				<p class="field-hint">
					{groups.length === 0 ? t('monitorForm.noGroupsYet') : t('monitorForm.groupHint')}
				</p>
				<div class="newgroup">
					<TextField
						label={t('monitorForm.newGroup')}
						bind:value={newGroupName}
						placeholder={t('monitorForm.newGroupPlaceholder')}
					/>
					<PillButton
						variant="secondary"
						size="sm"
						type="button"
						loading={creatingGroup}
						disabled={!newGroupName.trim()}
						onclick={createGroup}
					>
						{t('monitorForm.addGroup')}
					</PillButton>
				</div>
			</div>
		</div>
	</GlassCard>

	<div class="cols">
		<GlassCard title={t('monitorForm.tags')}>
			{#if tags.length === 0}
				<p class="muted">{t('monitorForm.noTags')}</p>
			{:else}
				<div class="chips">
					{#each tags as tag (tag.id)}
						<button
							type="button"
							class="chip ds-focusable"
							class:on={tagIds.includes(tag.id)}
							style="--tag: {tag.color}"
							aria-pressed={tagIds.includes(tag.id)}
							onclick={() => toggleTag(tag.id)}
						>
							{tag.name}
						</button>
					{/each}
				</div>
			{/if}
		</GlassCard>

		<GlassCard title={t('monitorForm.notifications')}>
			{#if channels.length === 0}
				<p class="muted">{t('monitorForm.noChannels')}</p>
			{:else}
				<div class="checks">
					{#each channels as c (c.id)}
						<Checkbox
							label="{c.name} ({c.type})"
							checked={notificationIds.includes(c.id)}
							onchange={() => toggleChannel(c.id)}
						/>
					{/each}
				</div>
			{/if}
		</GlassCard>
	</div>

	<GlassCard title={t('monitorForm.notes')}>
		<TextArea
			label={t('monitorForm.descriptionMarkdown')}
			bind:value={description}
			placeholder={t('monitorForm.descriptionPlaceholder')}
			rows={3}
		/>
		<div style="margin-top: var(--space-4)">
			<TextField
				label={t('monitorForm.externalLink')}
				bind:value={externalUrl}
				placeholder="https://runbook.example.com"
			/>
		</div>
	</GlassCard>

	<div class="footer">
		<PillButton variant="ghost" type="button" onclick={() => history.back()}
			>{t('common.cancel')}</PillButton
		>
		<PillButton type="submit" loading={saving}>
			{existing ? t('common.saveChanges') : t('monitorForm.createMonitor')}
		</PillButton>
	</div>
</form>

<style>
	.form {
		display: flex;
		flex-direction: column;
		gap: var(--space-5);
		max-width: 52rem;
	}
	.grid {
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: var(--space-4) var(--space-5);
	}
	.fullrow {
		grid-column: 1 / -1;
	}
	:global(.form .grid > *) {
		min-width: 0;
	}
	.spec-desc {
		margin: var(--space-4) 0 0;
		font-size: var(--text-footnote);
		color: var(--color-text-tertiary);
	}
	.cols {
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: var(--space-5);
		align-items: start;
	}
	.chips {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-3);
	}
	.chip {
		padding: var(--space-2) var(--space-4);
		border-radius: var(--radius-pill);
		border: 1px solid color-mix(in srgb, var(--tag) 30%, transparent);
		background: transparent;
		color: var(--color-text-secondary);
		font-family: inherit;
		font-size: var(--text-footnote);
		cursor: pointer;
	}
	.chip.on {
		color: var(--tag);
		background: color-mix(in srgb, var(--tag) 16%, transparent);
		font-weight: var(--weight-semibold);
	}
	.checks {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}
	.muted {
		margin: 0;
		color: var(--color-text-tertiary);
		font-size: var(--text-subheadline);
	}
	/* Select carries no error/hint slot of its own, so these match TextField's. */
	.field-hint,
	.field-error {
		margin: var(--space-2) 0 0;
		font-size: var(--text-caption-1);
	}
	.field-hint {
		color: var(--color-text-tertiary);
	}
	.field-error {
		color: var(--color-danger);
		font-weight: var(--weight-medium);
	}
	.newgroup {
		display: flex;
		align-items: flex-end;
		gap: var(--space-3);
		margin-top: var(--space-4);
	}
	.newgroup :global(.field) {
		flex: 1;
	}
	.footer {
		display: flex;
		justify-content: flex-end;
		gap: var(--space-3);
	}
	@media (max-width: 720px) {
		.grid,
		.cols {
			grid-template-columns: 1fr;
		}
	}
</style>
