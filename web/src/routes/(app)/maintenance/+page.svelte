<script lang="ts">
	import { onMount } from 'svelte';
	import {
		api,
		problemMessage,
		type MaintenanceRecurrence,
		type MaintenanceStrategy,
		type MaintenanceWindow,
		type Monitor
	} from '$lib/api';
	import { session } from '$lib/stores/session.svelte';
	import RelativeTime from '$lib/components/RelativeTime.svelte';
	import { absoluteTime } from '$lib/util/time';
	import {
		WEEKDAY_ORDER,
		describeRecurrence,
		isValidRecurrence,
		nextOccurrence,
		parseClock,
		weekdayShortNames
	} from '$lib/util/recurrence';
	import { COMMON_TIMEZONES, formatDateTime, i18n, t } from '$lib/i18n';
	import {
		Checkbox,
		EmptyState,
		GlassPanel,
		Modal,
		PillButton,
		SegmentedControl,
		Select,
		Skeleton,
		TextArea,
		TextField,
		toast
	} from '$lib/design-system';

	const MAX_DURATION_MINUTES = 7 * 24 * 60;

	let items = $state<MaintenanceWindow[]>([]);
	let allMonitors = $state<Monitor[]>([]);
	let loaded = $state(false);

	let editorOpen = $state(false);
	let editing = $state<MaintenanceWindow | undefined>(undefined);
	let fTitle = $state('');
	let fDesc = $state('');
	let fStrategy = $state<MaintenanceStrategy>('single');
	let fStart = $state('');
	let fEnd = $state('');
	let fWeekdays = $state<number[]>([]);
	let fRecStart = $state('02:00');
	let fRecDuration = $state('60');
	let fTimezone = $state('UTC');
	let fMonitors = $state<string[]>([]);
	let saving = $state(false);
	let confirmId = $state<string | undefined>(undefined);

	const strategySegments = $derived([
		{ value: 'single', label: t('maintenance.strategySingle') },
		{ value: 'recurring', label: t('maintenance.strategyRecurring') }
	]);

	const weekdayNames = $derived(weekdayShortNames());

	// Keep whatever zone the window already carries selectable even when it is
	// not one of the common ones, exactly as the settings picker does.
	const timezoneOptions = $derived(
		[...new Set([...COMMON_TIMEZONES, fTimezone])].sort().map((z) => ({ value: z, label: z }))
	);

	/** The recurrence as currently edited, whether or not it is valid yet. */
	const draftRecurrence = $derived<MaintenanceRecurrence>({
		weekdays: fWeekdays,
		start: fRecStart,
		durationMinutes: Number(fRecDuration)
	});
	const recurrenceValid = $derived(isValidRecurrence(draftRecurrence));
	const recurrenceSummary = $derived(
		recurrenceValid ? describeRecurrence(draftRecurrence, fTimezone) : ''
	);
	const nextRun = $derived(recurrenceValid ? nextOccurrence(draftRecurrence, fTimezone) : null);

	onMount(async () => {
		[items, allMonitors] = await Promise.all([api.listMaintenance(), api.listMonitors()]);
		loaded = true;
	});

	const toLocalInput = (iso: string) => {
		const d = new Date(iso);
		const off = d.getTimezoneOffset();
		return new Date(d.getTime() - off * 60000).toISOString().slice(0, 16);
	};

	function openCreate() {
		editing = undefined;
		fTitle = '';
		fDesc = '';
		fStrategy = 'single';
		fStart = toLocalInput(new Date().toISOString());
		fEnd = toLocalInput(new Date(Date.now() + 36e5).toISOString());
		fWeekdays = [];
		fRecStart = '02:00';
		fRecDuration = '60';
		// The zone the operator already reads timestamps in is the one they mean
		// when they type a start time, so start there rather than at UTC.
		fTimezone = i18n.effectiveTimezone;
		fMonitors = [];
		editorOpen = true;
	}
	function openEdit(w: MaintenanceWindow) {
		editing = w;
		fTitle = w.title;
		fDesc = w.description ?? '';
		fStrategy = w.strategy;
		// A recurring window may have no bounds at all; fall back to a sensible
		// pair so switching it to one-off does not open with empty inputs.
		fStart = toLocalInput(w.startsAt || new Date().toISOString());
		fEnd = toLocalInput(w.endsAt || new Date(Date.now() + 36e5).toISOString());
		fWeekdays = [...(w.recurrence?.weekdays ?? [])];
		fRecStart = w.recurrence?.start ?? '02:00';
		fRecDuration = String(w.recurrence?.durationMinutes ?? 60);
		fTimezone = w.timezone || i18n.effectiveTimezone;
		fMonitors = [...w.monitorIds];
		editorOpen = true;
	}
	function toggleMon(id: string) {
		fMonitors = fMonitors.includes(id) ? fMonitors.filter((x) => x !== id) : [...fMonitors, id];
	}
	function toggleWeekday(day: number) {
		fWeekdays = fWeekdays.includes(day) ? fWeekdays.filter((d) => d !== day) : [...fWeekdays, day];
	}

	/**
	 * Client-side mirror of the server's recurrence validation. It exists to
	 * name the offending field before a round trip, not to replace the check:
	 * the server rejects the same payloads with its own 400.
	 */
	function recurrenceError(): string | undefined {
		if (fWeekdays.length === 0) return t('maintenance.weekdaysRequired');
		if (!parseClock(fRecStart)) return t('maintenance.startTimeInvalid');
		const minutes = Number(fRecDuration);
		if (!Number.isInteger(minutes) || minutes <= 0 || minutes > MAX_DURATION_MINUTES) {
			return t('maintenance.durationInvalid');
		}
		return undefined;
	}

	async function save() {
		if (!fTitle.trim()) {
			toast.error(t('maintenance.titleRequired'));
			return;
		}
		const recurring = fStrategy === 'recurring';
		if (recurring) {
			const problem = recurrenceError();
			if (problem) {
				toast.error(problem);
				return;
			}
		}
		saving = true;
		try {
			const payload = {
				title: fTitle,
				description: fDesc || undefined,
				monitorIds: fMonitors,
				strategy: fStrategy,
				timezone: fTimezone,
				// A recurring series runs unbounded: starts_at/ends_at are the
				// window itself only for a one-off, and sending the editor's
				// leftover pair would bound the series without saying so.
				startsAt: recurring ? '' : new Date(fStart).toISOString(),
				endsAt: recurring ? '' : new Date(fEnd).toISOString(),
				// Copied out of the reactive draft so the request body cannot be
				// mutated by later edits to the (still open) form.
				recurrence: recurring
					? {
							weekdays: [...fWeekdays],
							start: fRecStart,
							durationMinutes: Number(fRecDuration)
						}
					: undefined
			};
			if (editing) {
				const u = await api.updateMaintenance(editing.id, payload);
				items = items.map((i) => (i.id === u.id ? u : i));
			} else {
				items = [...items, await api.createMaintenance(payload)];
			}
			editorOpen = false;
			toast.success(t('maintenance.saved'));
		} catch (e) {
			toast.error(problemMessage(e) ?? 'Could not save the maintenance window.');
		} finally {
			saving = false;
		}
	}
	async function remove(id: string) {
		try {
			await api.deleteMaintenance(id);
			items = items.filter((i) => i.id !== id);
			confirmId = undefined;
			toast.success(t('maintenance.deleted'));
		} catch (e) {
			toast.error(problemMessage(e) ?? 'Could not delete the window.');
		}
	}
	const monName = (id: string) => allMonitors.find((m) => m.id === id)?.name ?? 'monitor';
</script>

<svelte:head><title>{t('maintenance.pageTitle')}</title></svelte:head>

<div class="pagehead">
	<p class="lead">{t('maintenance.lead')}</p>
	{#if session.canEdit}
		<PillButton size="sm" onclick={openCreate}>{t('maintenance.newWindow')}</PillButton>
	{/if}
</div>

{#if !loaded}
	<Skeleton variant="rect" height="8rem" />
{:else if items.length === 0}
	<GlassPanel radius="lg" class="mt-wrap">
		<EmptyState title={t('maintenance.emptyTitle')} description={t('maintenance.emptyDescription')}>
			{#snippet action()}<PillButton onclick={openCreate}>{t('maintenance.newWindow')}</PillButton
				>{/snippet}
		</EmptyState>
	</GlassPanel>
{:else}
	<div class="list">
		{#each items as w (w.id)}
			<GlassPanel radius="md" class="mt-row">
				<div class="info">
					<div class="titleline">
						<strong>{w.title}</strong>
						{#if w.active}<span class="badge live">{t('common.active')}</span>{/if}
					</div>
					{#if w.description}<span class="desc">{w.description}</span>{/if}
					{#if w.strategy === 'recurring' && w.recurrence}
						{@const next = nextOccurrence(w.recurrence, w.timezone)}
						<span class="sub">
							{describeRecurrence(w.recurrence, w.timezone)}
							· {w.monitorIds.length === 1
								? t('maintenance.monitorCount', { n: w.monitorIds.length })
								: t('maintenance.monitorCountPlural', { n: w.monitorIds.length })}
						</span>
						{#if next}
							<span class="sub"
								>{t('maintenance.nextOccurrence', { when: formatDateTime(next) })}</span
							>
						{/if}
					{:else}
						<span class="sub" title={absoluteTime(w.startsAt)}>
							<RelativeTime value={w.startsAt} /> → <RelativeTime value={w.endsAt} />
							· {w.monitorIds.length === 1
								? t('maintenance.monitorCount', { n: w.monitorIds.length })
								: t('maintenance.monitorCountPlural', { n: w.monitorIds.length })}
						</span>
					{/if}
					{#if w.monitorIds.length}
						<span class="mons">{w.monitorIds.map(monName).join(', ')}</span>
					{/if}
				</div>
				{#if session.canEdit}
					<div class="rowactions">
						<PillButton variant="ghost" size="sm" onclick={() => openEdit(w)}
							>{t('common.edit')}</PillButton
						>
						<PillButton variant="ghost" size="sm" onclick={() => (confirmId = w.id)}
							>{t('common.delete')}</PillButton
						>
					</div>
				{/if}
			</GlassPanel>
		{/each}
	</div>
{/if}

<Modal
	bind:open={editorOpen}
	title={editing ? t('maintenance.editWindow') : t('maintenance.newWindow')}
>
	<div class="form">
		<TextField
			label={t('maintenance.title')}
			bind:value={fTitle}
			placeholder={t('maintenance.titlePlaceholder')}
			required
		/>
		<TextArea
			label={t('maintenance.description')}
			bind:value={fDesc}
			rows={2}
			placeholder={t('maintenance.descriptionPlaceholder')}
		/>
		<div>
			<span class="pl">{t('maintenance.schedule')}</span>
			<SegmentedControl
				segments={strategySegments}
				value={fStrategy}
				onchange={(v) => (fStrategy = v as MaintenanceStrategy)}
			/>
		</div>
		{#if fStrategy === 'single'}
			<div class="two">
				<TextField label={t('maintenance.starts')} type="datetime-local" bind:value={fStart} />
				<TextField label={t('maintenance.ends')} type="datetime-local" bind:value={fEnd} />
			</div>
		{:else}
			<div>
				<span class="pl">{t('maintenance.weekdays')}</span>
				<div class="weekdays">
					{#each WEEKDAY_ORDER as day (day)}
						<Checkbox
							label={weekdayNames[day]}
							checked={fWeekdays.includes(day)}
							onchange={() => toggleWeekday(day)}
						/>
					{/each}
				</div>
			</div>
			<div class="two">
				<TextField label={t('maintenance.startTime')} type="time" bind:value={fRecStart} />
				<TextField
					label={t('maintenance.duration')}
					type="number"
					min="1"
					max={MAX_DURATION_MINUTES}
					hint={t('maintenance.durationMinutes')}
					bind:value={fRecDuration}
				/>
			</div>
			<Select label={t('maintenance.timezone')} bind:value={fTimezone} options={timezoneOptions} />
			<p class="hint">{t('maintenance.timezoneHint')}</p>
			{#if recurrenceSummary}
				<GlassPanel radius="sm" class="mt-preview">
					<span class="summary">{recurrenceSummary}</span>
					{#if nextRun}
						<span class="next"
							>{t('maintenance.nextOccurrence', { when: formatDateTime(nextRun) })}</span
						>
					{/if}
				</GlassPanel>
			{/if}
		{/if}
		<div>
			<span class="pl">{t('maintenance.affectedMonitors')}</span>
			<div class="monlist">
				{#each allMonitors as m (m.id)}
					<Checkbox
						label={m.name}
						checked={fMonitors.includes(m.id)}
						onchange={() => toggleMon(m.id)}
					/>
				{/each}
			</div>
		</div>
	</div>
	{#snippet footer()}
		<PillButton variant="ghost" onclick={() => (editorOpen = false)}
			>{t('common.cancel')}</PillButton
		>
		<PillButton loading={saving} onclick={save}>{t('common.save')}</PillButton>
	{/snippet}
</Modal>

<Modal
	open={confirmId != null}
	title={t('maintenance.deleteTitle')}
	onclose={() => (confirmId = undefined)}
>
	<p style="margin:0">{t('maintenance.deleteBody')}</p>
	{#snippet footer()}
		<PillButton variant="ghost" onclick={() => (confirmId = undefined)}
			>{t('common.cancel')}</PillButton
		>
		<PillButton variant="destructive" onclick={() => confirmId && remove(confirmId)}
			>{t('common.delete')}</PillButton
		>
	{/snippet}
</Modal>

<style>
	.pagehead {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-4);
		margin-bottom: var(--space-5);
	}
	.lead {
		margin: 0;
		color: var(--color-text-secondary);
		font-size: var(--text-subheadline);
	}
	:global(.mt-wrap) {
		padding: var(--space-5);
	}
	.list {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}
	:global(.mt-row) {
		display: flex;
		align-items: flex-start;
		justify-content: space-between;
		gap: var(--space-4);
		padding: var(--space-4) var(--space-5);
	}
	.info {
		display: flex;
		flex-direction: column;
		gap: 3px;
		min-width: 0;
	}
	.titleline {
		display: flex;
		align-items: center;
		gap: var(--space-3);
	}
	.badge.live {
		font-size: var(--text-caption-2);
		padding: 1px var(--space-3);
		border-radius: var(--radius-pill);
		background: var(--status-maintenance-tint);
		color: var(--status-maintenance-text);
	}
	.desc {
		font-size: var(--text-footnote);
		color: var(--color-text-secondary);
	}
	.sub {
		font-size: var(--text-caption-1);
		color: var(--color-text-tertiary);
	}
	.mons {
		font-size: var(--text-caption-1);
		color: var(--color-text-tertiary);
	}
	.rowactions {
		display: flex;
		gap: var(--space-1);
		flex-shrink: 0;
	}
	.form {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}
	.two {
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: var(--space-4);
	}
	.pl {
		display: block;
		font-size: var(--text-footnote);
		font-weight: var(--weight-medium);
		color: var(--color-text-secondary);
		margin-bottom: var(--space-3);
	}
	.weekdays {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-3) var(--space-4);
	}
	.hint {
		margin: calc(-1 * var(--space-2)) 0 0;
		font-size: var(--text-caption-1);
		color: var(--color-text-tertiary);
	}
	:global(.mt-preview) {
		display: flex;
		flex-direction: column;
		gap: 2px;
		padding: var(--space-3) var(--space-4);
	}
	.summary {
		font-size: var(--text-footnote);
		color: var(--color-text-secondary);
	}
	.next {
		font-size: var(--text-caption-1);
		color: var(--color-text-tertiary);
	}
	.monlist {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
		max-height: 12rem;
		overflow-y: auto;
	}
	@media (max-width: 640px) {
		.two {
			grid-template-columns: 1fr;
		}
		/* A phone has no room for the actions beside the text: they would squeeze
		   the title into a one-word column. Drop them onto their own line. */
		:global(.mt-row) {
			flex-direction: column;
			align-items: stretch;
			gap: var(--space-3);
		}
	}
</style>
