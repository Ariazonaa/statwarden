<script lang="ts">
	// The persistent monitor list of the monitor area, laid out like Uptime
	// Kuma's left column: a toolbar (selection mode, search, filters, more), then
	// one row per monitor with its uptime pill and per-check bar, folders as
	// collapsible groups with a rolled-up row of their own.
	//
	// Everything the old dashboard card grid could do lives here now: search and
	// filters, sorting (including the manual drag order), favourites pinned on
	// top, folders, bulk selection with every bulk action, the folder manager
	// and the recycle bin. Only the presentation changed.
	import { onMount } from 'svelte';
	import {
		api,
		monitorTypeSpecs,
		problemMessage,
		problemStatus,
		type BulkMonitorAction,
		type Monitor,
		type MonitorGroup,
		type Notification,
		type Tag
	} from '$lib/api';
	import { monitors } from '$lib/stores/monitors.svelte';
	import { monitorList, type ListSort } from '$lib/stores/monitorList.svelte';
	import { prefs } from '$lib/stores/prefs.svelte';
	import { session } from '$lib/stores/session.svelte';
	import { t, formatNumber } from '$lib/i18n';
	import {
		Checkbox,
		EmptyState,
		HeartbeatBar,
		Modal,
		PillButton,
		Select,
		Skeleton,
		StatusPill,
		TextField,
		monitorStatuses,
		toast
	} from '$lib/design-system';
	import MonitorRow from './MonitorRow.svelte';
	import { meanUptime, mergeBeats, worstStatus } from './aggregate';

	let tags = $state<Tag[]>([]);
	let groups = $state<MonitorGroup[]>([]);
	let channels = $state<Notification[]>([]);

	onMount(() => {
		const stop = monitors.activate();
		api.listTags().then((tg) => (tags = tg));
		api.listMonitorGroups().then((g) => (groups = g));
		api.listNotifications().then((n) => (channels = n));
		monitors.load();
		return stop;
	});

	const total = $derived(monitors.items.length);

	// ---- toolbar ----
	let filtersOpen = $state(false);
	let moreOpen = $state(false);

	function onDocPointer(e: PointerEvent) {
		if (moreOpen && !(e.target as HTMLElement).closest('.ml-more')) moreOpen = false;
	}

	const statusOptions = $derived([
		{ value: 'all', label: t('monitorList.allStatuses') },
		...monitorStatuses.map((s) => ({ value: s, label: t(`status.${s}`) }))
	]);
	const tagOptions = $derived([
		{ value: 'all', label: t('dashboard.allTags') },
		...tags.map((tg) => ({ value: tg.id, label: tg.name }))
	]);
	const typeOptions = $derived([
		{ value: 'all', label: t('dashboard.allTypes') },
		...monitorTypeSpecs.map((spec) => ({ value: spec.type, label: t(spec.label) }))
	]);
	const sortOptions = $derived<{ value: ListSort; label: string }[]>([
		{ value: 'status', label: t('dashboard.sortStatus') },
		{ value: 'name', label: t('dashboard.sortName') },
		{ value: 'latency', label: t('dashboard.sortLatency') },
		{ value: 'uptime', label: t('dashboard.sortUptime') },
		{ value: 'created', label: t('dashboard.sortNewest') },
		{ value: 'manual', label: t('dashboard.sortManual') }
	]);

	const statusRank: Record<string, number> = {
		down: 0,
		pending: 1,
		maintenance: 2,
		up: 3,
		paused: 4
	};

	const filtered = $derived.by(() => {
		const q = monitorList.search.trim().toLowerCase();
		let list = monitors.items.filter((m) => {
			if (q && !m.name.toLowerCase().includes(q) && !(m.url ?? '').toLowerCase().includes(q))
				return false;
			if (monitorList.status !== 'all' && m.status !== monitorList.status) return false;
			if (monitorList.type !== 'all' && m.type !== monitorList.type) return false;
			if (monitorList.tag !== 'all' && !m.tagIds.includes(monitorList.tag)) return false;
			return true;
		});
		if (monitorList.sort === 'manual') {
			list = prefs.applyOrder(list);
		} else {
			list = [...list].sort((a, b) => {
				switch (monitorList.sort) {
					case 'name':
						return a.name.localeCompare(b.name);
					case 'latency':
						return (b.latencyMs ?? -1) - (a.latencyMs ?? -1);
					case 'uptime':
						return (a.uptime24h ?? 100) - (b.uptime24h ?? 100);
					case 'created':
						return b.createdAt.localeCompare(a.createdAt);
					default:
						// Status, then name — Kuma's order, and stable across reloads.
						return statusRank[a.status] - statusRank[b.status] || a.name.localeCompare(b.name);
				}
			});
		}
		return list;
	});

	const pinned = $derived(filtered.filter((m) => prefs.isFavorite(m.id)));
	const rest = $derived(filtered.filter((m) => !prefs.isFavorite(m.id)));

	// ---- folders ----
	// Grouping is opt-out and only offered once a folder exists, so a fresh
	// install never sees a single "No folder" header over its whole list.
	const grouping = $derived(monitorList.groupByFolder && groups.length > 0);

	/**
	 * The rest of the list split into folder sections; unfiled monitors follow
	 * the folders WITHOUT a header of their own, as Kuma lists ungrouped
	 * monitors beside its groups.
	 *
	 * A monitor whose `groupId` matches no known folder counts as unfiled rather
	 * than being dropped: the folder list and the monitor list are two separate
	 * fetches, so a folder deleted between them would otherwise make monitors
	 * silently disappear. Nothing may vanish behind grouping.
	 *
	 * Empty sections are omitted — a folder none of whose monitors survived the
	 * filters is noise here — but the folder manager still lists every folder.
	 */
	const sections = $derived.by(() => {
		const known = new Map(groups.map((g) => [g.id, g]));
		const out: { key: string; label: string; items: Monitor[] }[] = [];
		for (const g of [...groups].sort(
			(a, b) => a.sortOrder - b.sortOrder || a.id.localeCompare(b.id)
		)) {
			const items = rest.filter((m) => m.groupId === g.id);
			if (items.length > 0) out.push({ key: g.id, label: g.name, items });
		}
		const unfiled = rest.filter((m) => m.groupId == null || !known.has(m.groupId));
		return { folders: out, unfiled };
	});

	let collapsed = $state<Record<string, boolean>>({});
	// A search expands every folder: a match hidden inside a collapsed group
	// looks exactly like no match at all.
	const isCollapsed = (key: string) => !!collapsed[key] && monitorList.search.trim() === '';

	function folderUptime(items: Monitor[]): string {
		const u = meanUptime(items);
		return u === undefined
			? '–'
			: `${formatNumber(u, { maximumFractionDigits: u === 100 ? 0 : 2 })} %`;
	}

	// ---- bulk selection ----
	let selectMode = $state(false);
	let selected = $state<string[]>([]);
	let bulkBusy = $state(false);

	const visibleIds = $derived(new Set(filtered.map((m) => m.id)));
	/**
	 * Only what is on screen can be acted on. Intersecting here rather than
	 * pruning `selected` in an effect means the count the operator reads and the
	 * ids the request carries are the same value — a selection hidden by a filter
	 * can never be swept into a bulk delete.
	 */
	const selectedIds = $derived(selected.filter((id) => visibleIds.has(id)));
	const selectedMonitors = $derived(
		selectedIds.map((id) => monitors.get(id)).filter((m): m is Monitor => m != null)
	);
	const allVisibleSelected = $derived(
		filtered.length > 0 && selectedIds.length === filtered.length
	);

	function toggleSelect(id: string, on: boolean) {
		selected = on ? [...new Set([...selected, id])] : selected.filter((x) => x !== id);
	}
	function selectAll() {
		selected = filtered.map((m) => m.id);
	}
	function clearSelection() {
		selected = [];
	}
	function setSelectMode(on: boolean) {
		selectMode = on;
		if (!on) clearSelection();
	}

	let confirmBulkDelete = $state(false);
	let tagDialog = $state(false);
	let tagPicks = $state<string[]>([]);
	let moveDialog = $state(false);
	let movePick = $state('');
	let notifyDialog = $state(false);
	let notifyPicks = $state<string[]>([]);
	let scheduleDialog = $state(false);
	// Kept as strings because the number inputs bind strings; an empty optional
	// field means "leave this monitor's value untouched", which is why they are
	// not seeded to a number and only parsed when non-empty on apply.
	let schedInterval = $state('60');
	let schedTimeout = $state('');
	let schedRetries = $state('');
	let schedRetryInterval = $state('');

	/**
	 * Runs one bulk action and reports honestly. The endpoint answers 200 with
	 * per-id outcomes on partial failure, so a resolved promise is NOT success —
	 * `failed` has to be read, or an operator would be told five monitors were
	 * paused when three of them were not.
	 */
	async function runBulk(action: BulkMonitorAction, extra: Parameters<typeof api.bulkMonitors>[0]) {
		if (selectedIds.length === 0) return;
		bulkBusy = true;
		try {
			const res = await api.bulkMonitors(extra);
			// Every action here changes something the row renders (status, tags,
			// folder) or removes the monitor, so the list is refetched rather than
			// patched per action.
			await monitors.load(true);
			selected = res.results.filter((r) => !r.ok).map((r) => r.monitorId);
			if (res.failed > 0) {
				toast.error(t('dashboard.bulkPartial', { ok: res.succeeded, failed: res.failed }));
			} else {
				toast.success(t(`dashboard.bulkDone.${action}`, { n: res.succeeded }));
				if (selectMode) clearSelection();
			}
		} catch (err) {
			toast.error(problemMessage(err) ?? t('dashboard.bulkError'));
		} finally {
			bulkBusy = false;
			confirmBulkDelete = false;
			tagDialog = false;
			moveDialog = false;
			notifyDialog = false;
			scheduleDialog = false;
		}
	}

	const bulkPause = () => runBulk('pause', { action: 'pause', monitorIds: selectedIds });
	const bulkResume = () => runBulk('resume', { action: 'resume', monitorIds: selectedIds });
	const bulkDelete = () => runBulk('delete', { action: 'delete', monitorIds: selectedIds });
	const bulkTag = () =>
		runBulk('tag', { action: 'tag', monitorIds: selectedIds, tagIds: tagPicks });
	// `null` clears the folder; the mapper sends it as an explicit null, which is
	// the only way a single-assignment column can be emptied.
	const bulkMove = () =>
		runBulk('group', { action: 'group', monitorIds: selectedIds, groupId: movePick || null });
	// Additive, like bulkTag: the selected channels are ADDED to every monitor.
	const bulkNotify = () =>
		runBulk('notify', {
			action: 'notify',
			monitorIds: selectedIds,
			notificationIds: notifyPicks
		});
	// An empty optional field is left off the request entirely, so that monitor's
	// stored timeout/retries survive — only the interval is always sent.
	const bulkSchedule = () =>
		runBulk('schedule', {
			action: 'schedule',
			monitorIds: selectedIds,
			schedule: {
				interval: schedIntervalNum,
				timeout: optNum(schedTimeout),
				maxRetries: optNum(schedRetries),
				retryInterval: optNum(schedRetryInterval)
			}
		});

	// Parses an optional numeric field: blank means "leave unchanged" (undefined),
	// so it is dropped from the request rather than sent as 0.
	function optNum(v: string): number | undefined {
		const s = v.trim();
		if (s === '') return undefined;
		const n = Number(s);
		return Number.isFinite(n) ? n : undefined;
	}

	const schedIntervalNum = $derived(Number(schedInterval));
	// Interval is the one required schedule field; the server floors it at 1.
	const schedValid = $derived(Number.isFinite(schedIntervalNum) && schedIntervalNum >= 1);

	function openTagDialog() {
		tagPicks = [];
		tagDialog = true;
	}
	function openMoveDialog() {
		movePick = '';
		moveDialog = true;
	}
	function toggleTagPick(id: string, on: boolean) {
		tagPicks = on ? [...new Set([...tagPicks, id])] : tagPicks.filter((x) => x !== id);
	}
	function openNotifyDialog() {
		notifyPicks = [];
		notifyDialog = true;
	}
	function toggleNotifyPick(id: string, on: boolean) {
		notifyPicks = on ? [...new Set([...notifyPicks, id])] : notifyPicks.filter((x) => x !== id);
	}
	function openScheduleDialog() {
		schedInterval = '60';
		schedTimeout = '';
		schedRetries = '';
		schedRetryInterval = '';
		scheduleDialog = true;
	}

	const moveOptions = $derived([
		{ value: '', label: t('dashboard.folderNone') },
		...groups.map((g) => ({ value: g.id, label: g.name }))
	]);

	// ---- recycle bin ----
	//
	// Deleting a monitor is a soft delete: it moves here, out of every other
	// view, and can be brought back with its whole heartbeat history. That is the
	// point — the bulk action deletes a whole selection behind one confirmation,
	// and before the bin there was nothing to undo it with.
	let trashDialog = $state(false);
	let trashItems = $state<Monitor[]>([]);
	let trashLoading = $state(false);
	/**
	 * Set to the load failure, so the dialog can say so.
	 *
	 * Without it a failed fetch left trashItems empty and the dialog rendered
	 * "The bin is empty" — the single most misleading thing it could say about a
	 * bin whose contents it could not read.
	 */
	let trashError = $state<string | undefined>(undefined);
	/** The ids of the rows with a request in flight; a set, so two rows can be busy. */
	let trashBusy = $state<string[]>([]);

	function markBusy(id: string, on: boolean) {
		trashBusy = on ? [...trashBusy, id] : trashBusy.filter((x) => x !== id);
	}
	// Purging destroys the history for good, so it asks first — and asks per
	// monitor, not once for the whole bin.
	let purgeConfirmId = $state<string | undefined>(undefined);

	async function openTrash() {
		moreOpen = false;
		trashDialog = true;
		purgeConfirmId = undefined;
		trashError = undefined;
		// Cleared BEFORE the fetch, not after it: reopening the dialog otherwise
		// showed the previous visit's rows, with live buttons, while it loaded.
		trashItems = [];
		trashLoading = true;
		try {
			trashItems = await api.listDeletedMonitors();
		} catch (err) {
			trashError = problemMessage(err) ?? t('trash.loadError');
			toast.error(trashError);
		} finally {
			trashLoading = false;
		}
	}

	async function restoreFromTrash(m: Monitor) {
		markBusy(m.id, true);
		try {
			await api.restoreMonitor(m.id);
			trashItems = trashItems.filter((x) => x.id !== m.id);
			// Refetched rather than patched: the monitor comes back PAUSED, and its
			// row has to show that rather than the state it was deleted in.
			await monitors.load(true);
			toast.success(t('trash.restored', { name: m.name }));
		} catch (err) {
			toast.error(problemMessage(err) ?? t('trash.restoreError'));
		} finally {
			markBusy(m.id, false);
		}
	}

	async function purgeFromTrash(m: Monitor) {
		markBusy(m.id, true);
		try {
			await api.purgeMonitor(m.id);
			trashItems = trashItems.filter((x) => x.id !== m.id);
			purgeConfirmId = undefined;
			toast.success(t('trash.purged', { name: m.name }));
		} catch (err) {
			// 403 is the one failure with a specific cause worth naming: purging is
			// admin-only, because it is the only step here that cannot be undone.
			toast.error(
				problemStatus(err) === 403
					? t('trash.purgeForbidden')
					: (problemMessage(err) ?? t('trash.purgeError'))
			);
		} finally {
			markBusy(m.id, false);
		}
	}

	// ---- folder management ----
	let folderDialog = $state(false);
	let folderDrafts = $state<Record<string, string>>({});
	let newFolder = $state('');
	let folderBusy = $state(false);
	let folderConfirmId = $state<string | undefined>(undefined);

	function openFolders() {
		moreOpen = false;
		folderDrafts = Object.fromEntries(groups.map((g) => [g.id, g.name]));
		newFolder = '';
		folderConfirmId = undefined;
		folderDialog = true;
	}

	async function createFolder() {
		const name = newFolder.trim();
		if (!name) return;
		folderBusy = true;
		try {
			const g = await api.createMonitorGroup({ name, sortOrder: groups.length });
			groups = [...groups, g];
			folderDrafts = { ...folderDrafts, [g.id]: g.name };
			newFolder = '';
			toast.success(t('dashboard.folderCreated'));
		} catch (err) {
			toast.error(problemMessage(err) ?? t('dashboard.folderError'));
		} finally {
			folderBusy = false;
		}
	}

	async function renameFolder(g: MonitorGroup) {
		const name = (folderDrafts[g.id] ?? '').trim();
		if (!name || name === g.name) return;
		folderBusy = true;
		try {
			const u = await api.updateMonitorGroup(g.id, { name, sortOrder: g.sortOrder });
			groups = groups.map((x) => (x.id === u.id ? u : x));
			toast.success(t('dashboard.folderRenamed'));
		} catch (err) {
			toast.error(problemMessage(err) ?? t('dashboard.folderError'));
		} finally {
			folderBusy = false;
		}
	}

	async function deleteFolder(id: string) {
		folderBusy = true;
		try {
			await api.deleteMonitorGroup(id);
			groups = groups.filter((g) => g.id !== id);
			// The monitors survive with their folder cleared (ON DELETE SET NULL),
			// so the list has to be refetched or they would keep rendering under a
			// folder that no longer exists.
			await monitors.load(true);
			folderConfirmId = undefined;
			toast.success(t('dashboard.folderDeleted'));
		} catch (err) {
			toast.error(problemMessage(err) ?? t('dashboard.folderError'));
		} finally {
			folderBusy = false;
		}
	}

	// ---- drag & drop (manual sort) ----
	let dragId = $state<string | undefined>(undefined);
	// Dragging is disabled while grouping or selecting: manual order is one flat
	// sequence, so a drag across two folder sections would record a reordering
	// the grouped view cannot show — a control that silently does nothing.
	const canDrag = $derived(
		monitorList.sort === 'manual' && session.canEdit && !grouping && !selectMode
	);

	function onDrop(targetId: string) {
		if (!dragId || dragId === targetId) return;
		// Reorder the FULL list, not the filtered view: prefs.setOrder replaces the
		// stored order wholesale, so building it from `filtered` would drop every
		// monitor the search hides to the end — permanently, with no message.
		const ids = prefs.applyOrder(monitors.items).map((m) => m.id);
		const from = ids.indexOf(dragId);
		const to = ids.indexOf(targetId);
		if (from < 0 || to < 0) return;
		ids.splice(to, 0, ids.splice(from, 1)[0]);
		prefs.setOrder(ids);
		dragId = undefined;
	}
</script>

<svelte:document onpointerdown={onDocPointer} />

{#snippet rows(items: Monitor[], indent: boolean)}
	{#each items as m (m.id)}
		<div
			draggable={canDrag}
			role={canDrag ? 'listitem' : undefined}
			class:dragging={dragId === m.id}
			ondragstart={() => (dragId = m.id)}
			ondragover={(e) => canDrag && e.preventDefault()}
			ondrop={() => onDrop(m.id)}
			ondragend={() => (dragId = undefined)}
		>
			<MonitorRow
				monitor={m}
				{tags}
				{indent}
				draggable={canDrag}
				selectable={selectMode}
				selected={selectedIds.includes(m.id)}
				onselect={(on) => toggleSelect(m.id, on)}
			/>
		</div>
	{/each}
{/snippet}

<section class="ml" aria-label={t('monitorList.label')}>
	<div class="ml-toolbar">
		{#if session.canEdit}
			<span class="ml-selmode">
				<Checkbox
					checked={selectMode}
					label={t('monitorList.selectMode')}
					labelHidden
					onchange={setSelectMode}
				/>
			</span>
		{/if}
		<label class="ml-search">
			<svg viewBox="0 0 16 16" aria-hidden="true"
				><circle
					cx="6.75"
					cy="6.75"
					r="4.75"
					fill="none"
					stroke="currentColor"
					stroke-width="1.8"
				/><path
					d="M10.5 10.5 14 14"
					stroke="currentColor"
					stroke-width="1.8"
					stroke-linecap="round"
				/></svg
			>
			<input
				type="search"
				bind:value={monitorList.search}
				placeholder={t('dashboard.searchPlaceholder')}
				aria-label={t('dashboard.searchPlaceholder')}
			/>
		</label>
		<button
			type="button"
			class="ml-tool ds-focusable"
			class:on={filtersOpen || monitorList.activeFilters > 0}
			aria-expanded={filtersOpen}
			aria-controls="ml-filters"
			onclick={() => (filtersOpen = !filtersOpen)}
		>
			<svg viewBox="0 0 24 24" aria-hidden="true"
				><path
					d="M4 6h16M7 12h10M10 18h4"
					fill="none"
					stroke="currentColor"
					stroke-width="2"
					stroke-linecap="round"
				/></svg
			>
			{monitorList.activeFilters > 0
				? t('monitorList.filtersActive', { n: monitorList.activeFilters })
				: t('monitorList.filters')}
		</button>
		<div class="ml-more">
			<button
				type="button"
				class="ml-tool icon ds-focusable"
				aria-haspopup="menu"
				aria-expanded={moreOpen}
				aria-label={t('monitorList.more')}
				onclick={() => (moreOpen = !moreOpen)}
			>
				<svg viewBox="0 0 24 24" aria-hidden="true"
					><circle cx="5" cy="12" r="1.6" fill="currentColor" /><circle
						cx="12"
						cy="12"
						r="1.6"
						fill="currentColor"
					/><circle cx="19" cy="12" r="1.6" fill="currentColor" /></svg
				>
			</button>
			{#if moreOpen}
				<div class="ml-menu" role="menu">
					{#if groups.length > 0}
						<button
							type="button"
							class="mi"
							role="menuitemcheckbox"
							aria-checked={monitorList.groupByFolder}
							onclick={() => {
								monitorList.groupByFolder = !monitorList.groupByFolder;
								moreOpen = false;
							}}
						>
							{monitorList.groupByFolder ? t('dashboard.flatList') : t('dashboard.groupByFolder')}
						</button>
					{/if}
					<button
						type="button"
						class="mi"
						role="menuitem"
						onclick={() => {
							prefs.toggleDensity();
							moreOpen = false;
						}}
					>
						{prefs.density === 'compact'
							? t('dashboard.densityComfortable')
							: t('dashboard.densityCompact')}
					</button>
					{#if session.canEdit}
						<button type="button" class="mi" role="menuitem" onclick={openFolders}>
							{t('dashboard.manageFolders')}
						</button>
						<button type="button" class="mi" role="menuitem" onclick={openTrash}>
							{t('trash.open')}
						</button>
					{/if}
				</div>
			{/if}
		</div>
	</div>

	{#if filtersOpen}
		<div class="ml-filters" id="ml-filters">
			<Select
				bind:value={monitorList.status}
				options={statusOptions}
				label={t('monitorList.filterByStatus')}
			/>
			<Select
				bind:value={monitorList.tag}
				options={tagOptions}
				label={t('dashboard.filterByTag')}
			/>
			<Select
				bind:value={monitorList.type}
				options={typeOptions}
				label={t('dashboard.filterByType')}
			/>
			<Select bind:value={monitorList.sort} options={sortOptions} label={t('dashboard.sort')} />
			{#if monitorList.activeFilters > 0}
				<PillButton variant="ghost" size="sm" onclick={() => monitorList.clearFilters()}>
					{t('monitorList.clearFilters')}
				</PillButton>
			{/if}
		</div>
	{/if}

	{#if selectMode}
		<!-- The bar is the selection's status AND its controls. aria-live announces
		     the count on every change, so a keyboard user ticking checkboxes hears
		     the running total without moving focus up here to read it. -->
		<div class="ml-bulk" role="group" aria-label={t('dashboard.bulkBarLabel')}>
			<div class="ml-bulk-head">
				<p class="count" aria-live="polite" aria-atomic="true">
					{t('dashboard.selectedCount', { n: selectedIds.length })}
				</p>
				<PillButton
					variant="ghost"
					size="sm"
					disabled={bulkBusy || allVisibleSelected || filtered.length === 0}
					onclick={selectAll}
				>
					{t('dashboard.selectAll')}
				</PillButton>
				<PillButton
					variant="ghost"
					size="sm"
					disabled={bulkBusy || selectedIds.length === 0}
					onclick={clearSelection}
				>
					{t('dashboard.clearSelection')}
				</PillButton>
			</div>
			<div class="ml-bulk-actions">
				<PillButton
					variant="secondary"
					size="sm"
					disabled={bulkBusy || selectedIds.length === 0}
					onclick={bulkPause}
				>
					{t('dashboard.bulkPause')}
				</PillButton>
				<PillButton
					variant="secondary"
					size="sm"
					disabled={bulkBusy || selectedIds.length === 0}
					onclick={bulkResume}
				>
					{t('dashboard.bulkResume')}
				</PillButton>
				<PillButton
					variant="secondary"
					size="sm"
					disabled={bulkBusy || selectedIds.length === 0}
					onclick={openTagDialog}
				>
					{t('dashboard.bulkTag')}
				</PillButton>
				<PillButton
					variant="secondary"
					size="sm"
					disabled={bulkBusy || selectedIds.length === 0}
					onclick={openNotifyDialog}
				>
					{t('dashboard.bulkNotify')}
				</PillButton>
				<PillButton
					variant="secondary"
					size="sm"
					disabled={bulkBusy || selectedIds.length === 0}
					onclick={openMoveDialog}
				>
					{t('dashboard.bulkMove')}
				</PillButton>
				<PillButton
					variant="secondary"
					size="sm"
					disabled={bulkBusy || selectedIds.length === 0}
					onclick={openScheduleDialog}
				>
					{t('dashboard.bulkSchedule')}
				</PillButton>
				<PillButton
					variant="destructive"
					size="sm"
					disabled={bulkBusy || selectedIds.length === 0}
					onclick={() => (confirmBulkDelete = true)}
				>
					{t('dashboard.bulkDelete')}
				</PillButton>
			</div>
		</div>
	{/if}

	<div class="ml-body">
		{#if monitors.error && !monitors.loaded}
			<!-- First fetch failed: `loaded` stays false, so without this branch the
			     list would sit on skeletons forever. -->
			<EmptyState title={monitors.error ?? ''}>
				{#snippet action()}
					<PillButton onclick={() => monitors.load(true)}>{t('common.retry')}</PillButton>
				{/snippet}
			</EmptyState>
		{:else if !monitors.loaded}
			<div class="ml-skel">
				{#each Array(6) as _, i (i)}
					<Skeleton variant="rect" height="2.5rem" />
				{/each}
			</div>
		{:else if total === 0}
			<EmptyState title={t('dashboard.emptyTitle')} description={t('dashboard.emptyDescription')} />
		{:else if filtered.length === 0}
			<EmptyState
				title={t('dashboard.noMatchesTitle')}
				description={t('dashboard.noMatchesDescription')}
			/>
		{:else}
			{#if pinned.length > 0}
				<h2 class="ml-head">{t('dashboard.pinned')}</h2>
				{@render rows(pinned, false)}
				{#if rest.length > 0}<h2 class="ml-head">{t('dashboard.allMonitors')}</h2>{/if}
			{/if}

			{#if grouping}
				{#each sections.folders as sec (sec.key)}
					{@const open = !isCollapsed(sec.key)}
					<button
						type="button"
						class="ml-folder ds-focusable"
						aria-expanded={open}
						onclick={() => (collapsed = { ...collapsed, [sec.key]: open })}
					>
						<svg class="chev" class:shut={!open} viewBox="0 0 24 24" aria-hidden="true"
							><path
								d="m6 9 6 6 6-6"
								fill="none"
								stroke="currentColor"
								stroke-width="2.2"
								stroke-linecap="round"
							/></svg
						>
						<span class="ml-folder-info">
							<StatusPill status={worstStatus(sec.items)} label={folderUptime(sec.items)} />
							<span class="ml-folder-name">{sec.label}</span>
							<span class="ml-folder-count">({sec.items.length})</span>
						</span>
						<HeartbeatBar beats={mergeBeats(sec.items, 30)} slots={30} size="sm" class="bar" />
					</button>
					{#if open}
						{@render rows(sec.items, true)}
					{/if}
				{/each}
				{@render rows(sections.unfiled, false)}
			{:else}
				{@render rows(rest, false)}
			{/if}
		{/if}
	</div>
</section>

<!-- Bulk delete names what is about to happen: the monitors are listed, not
     just counted, because "delete 7 monitors" is not something an operator can
     check against their intent. -->
<Modal
	open={confirmBulkDelete}
	title={t('dashboard.bulkDeleteTitle', { n: selectedIds.length })}
	onclose={() => (confirmBulkDelete = false)}
>
	<p class="dlgtext">{t('dashboard.bulkDeleteBody', { n: selectedIds.length })}</p>
	<ul class="namelist">
		{#each selectedMonitors.slice(0, 12) as m (m.id)}
			<li>{m.name}</li>
		{/each}
	</ul>
	{#if selectedMonitors.length > 12}
		<p class="dlgtext">{t('dashboard.bulkDeleteMore', { n: selectedMonitors.length - 12 })}</p>
	{/if}
	{#snippet footer()}
		<PillButton variant="ghost" onclick={() => (confirmBulkDelete = false)}>
			{t('common.cancel')}
		</PillButton>
		<PillButton variant="destructive" loading={bulkBusy} onclick={bulkDelete}>
			{t('dashboard.bulkDeleteConfirm', { n: selectedIds.length })}
		</PillButton>
	{/snippet}
</Modal>

<Modal
	open={tagDialog}
	title={t('dashboard.bulkTagTitle', { n: selectedIds.length })}
	onclose={() => (tagDialog = false)}
>
	{#if tags.length === 0}
		<p class="dlgtext">{t('dashboard.bulkTagNone')}</p>
	{:else}
		<p class="dlgtext">{t('dashboard.bulkTagBody')}</p>
		<div class="picks">
			{#each tags as tg (tg.id)}
				<Checkbox
					checked={tagPicks.includes(tg.id)}
					label={tg.name}
					onchange={(on) => toggleTagPick(tg.id, on)}
				/>
			{/each}
		</div>
	{/if}
	{#snippet footer()}
		<PillButton variant="ghost" onclick={() => (tagDialog = false)}>{t('common.cancel')}</PillButton
		>
		<PillButton loading={bulkBusy} disabled={tagPicks.length === 0} onclick={bulkTag}>
			{t('dashboard.bulkTagApply')}
		</PillButton>
	{/snippet}
</Modal>

<Modal
	open={moveDialog}
	title={t('dashboard.bulkMoveTitle', { n: selectedIds.length })}
	onclose={() => (moveDialog = false)}
>
	<Select label={t('dashboard.folder')} bind:value={movePick} options={moveOptions} />
	<p class="dlgtext">{t('dashboard.bulkMoveHint')}</p>
	{#snippet footer()}
		<PillButton variant="ghost" onclick={() => (moveDialog = false)}
			>{t('common.cancel')}</PillButton
		>
		<PillButton loading={bulkBusy} onclick={bulkMove}>{t('dashboard.bulkMoveApply')}</PillButton>
	{/snippet}
</Modal>

<Modal
	open={notifyDialog}
	title={t('dashboard.bulkNotifyTitle', { n: selectedIds.length })}
	onclose={() => (notifyDialog = false)}
>
	{#if channels.length === 0}
		<p class="dlgtext">{t('dashboard.bulkNotifyNone')}</p>
	{:else}
		<p class="dlgtext">{t('dashboard.bulkNotifyBody')}</p>
		<div class="picks">
			{#each channels as ch (ch.id)}
				<Checkbox
					checked={notifyPicks.includes(ch.id)}
					label={ch.name}
					onchange={(on) => toggleNotifyPick(ch.id, on)}
				/>
			{/each}
		</div>
	{/if}
	{#snippet footer()}
		<PillButton variant="ghost" onclick={() => (notifyDialog = false)}>
			{t('common.cancel')}
		</PillButton>
		<PillButton loading={bulkBusy} disabled={notifyPicks.length === 0} onclick={bulkNotify}>
			{t('dashboard.bulkNotifyApply')}
		</PillButton>
	{/snippet}
</Modal>

<Modal
	open={scheduleDialog}
	title={t('dashboard.bulkScheduleTitle', { n: selectedIds.length })}
	onclose={() => (scheduleDialog = false)}
>
	<p class="dlgtext">{t('dashboard.bulkScheduleBody')}</p>
	<div class="schedfields">
		<TextField
			label={t('dashboard.bulkScheduleInterval')}
			type="number"
			min="1"
			inputmode="numeric"
			bind:value={schedInterval}
		/>
		<TextField
			label={t('dashboard.bulkScheduleTimeout')}
			type="number"
			min="1"
			inputmode="numeric"
			placeholder={t('dashboard.bulkScheduleKeep')}
			bind:value={schedTimeout}
		/>
		<TextField
			label={t('dashboard.bulkScheduleRetries')}
			type="number"
			min="0"
			inputmode="numeric"
			placeholder={t('dashboard.bulkScheduleKeep')}
			bind:value={schedRetries}
		/>
		<TextField
			label={t('dashboard.bulkScheduleRetryInterval')}
			type="number"
			min="1"
			inputmode="numeric"
			placeholder={t('dashboard.bulkScheduleKeep')}
			bind:value={schedRetryInterval}
		/>
	</div>
	{#snippet footer()}
		<PillButton variant="ghost" onclick={() => (scheduleDialog = false)}>
			{t('common.cancel')}
		</PillButton>
		<PillButton loading={bulkBusy} disabled={!schedValid} onclick={bulkSchedule}>
			{t('dashboard.bulkScheduleApply')}
		</PillButton>
	{/snippet}
</Modal>

<Modal
	open={folderDialog}
	title={t('dashboard.foldersTitle')}
	onclose={() => (folderDialog = false)}
>
	<p class="dlgtext">{t('dashboard.foldersLead')}</p>
	{#if groups.length > 0}
		<ul class="folders">
			{#each groups as g (g.id)}
				<li class="folderrow">
					<TextField
						label={t('dashboard.folderNameFor', { name: g.name })}
						labelHidden
						bind:value={folderDrafts[g.id]}
					/>
					<PillButton
						variant="secondary"
						size="sm"
						disabled={folderBusy ||
							!(folderDrafts[g.id] ?? '').trim() ||
							(folderDrafts[g.id] ?? '').trim() === g.name}
						onclick={() => renameFolder(g)}
					>
						{t('dashboard.folderRename')}
					</PillButton>
					{#if folderConfirmId === g.id}
						<!-- Inline rather than a second modal: nesting dialogs traps focus
						     in the wrong one. Deleting a folder keeps its monitors. -->
						<PillButton
							variant="destructive"
							size="sm"
							loading={folderBusy}
							onclick={() => deleteFolder(g.id)}
						>
							{t('dashboard.folderDeleteConfirm')}
						</PillButton>
						<PillButton variant="ghost" size="sm" onclick={() => (folderConfirmId = undefined)}>
							{t('common.cancel')}
						</PillButton>
					{:else}
						<PillButton
							variant="ghost"
							size="sm"
							disabled={folderBusy}
							onclick={() => (folderConfirmId = g.id)}
						>
							{t('common.delete')}
						</PillButton>
					{/if}
				</li>
			{/each}
		</ul>
	{:else}
		<p class="dlgtext">{t('dashboard.foldersEmpty')}</p>
	{/if}

	<div class="folderrow newfolder">
		<TextField
			label={t('dashboard.newFolder')}
			bind:value={newFolder}
			placeholder={t('dashboard.newFolderPlaceholder')}
		/>
		<PillButton
			variant="secondary"
			size="sm"
			loading={folderBusy}
			disabled={!newFolder.trim()}
			onclick={createFolder}
		>
			{t('dashboard.folderCreate')}
		</PillButton>
	</div>

	{#snippet footer()}
		<PillButton onclick={() => (folderDialog = false)}>{t('common.done')}</PillButton>
	{/snippet}
</Modal>

<Modal open={trashDialog} title={t('trash.title')} onclose={() => (trashDialog = false)}>
	<p class="dlgtext">{t('trash.lead')}</p>
	{#if trashLoading}
		<Skeleton lines={3} />
	{:else if trashError}
		<!-- "The bin is empty" would be a lie here, and the one lie this dialog
		     must never tell: it is the only place a deleted monitor can be got
		     back from. -->
		<EmptyState title={trashError}>
			{#snippet action()}
				<PillButton onclick={openTrash}>{t('common.retry')}</PillButton>
			{/snippet}
		</EmptyState>
	{:else if trashItems.length === 0}
		<p class="dlgtext">{t('trash.empty')}</p>
	{:else}
		<ul class="trashlist">
			{#each trashItems as m (m.id)}
				<li class="trashrow">
					<span class="tname" title={m.name}>{m.name}</span>
					<span class="ttype">{m.type}</span>
					{#if purgeConfirmId === m.id}
						<!-- The confirmation replaces the row's buttons rather than opening a
						     second dialog on top of this one: a nested modal over a list is
						     where people lose track of WHICH row they are confirming. -->
						<span class="tconfirm">{t('trash.purgeConfirm')}</span>
						<PillButton
							variant="destructive"
							size="sm"
							loading={trashBusy.includes(m.id)}
							onclick={() => purgeFromTrash(m)}
						>
							{t('trash.purgeYes')}
						</PillButton>
						<PillButton variant="ghost" size="sm" onclick={() => (purgeConfirmId = undefined)}>
							{t('common.cancel')}
						</PillButton>
					{:else}
						<PillButton
							variant="secondary"
							size="sm"
							loading={trashBusy.includes(m.id)}
							onclick={() => restoreFromTrash(m)}
						>
							{t('trash.restore')}
						</PillButton>
						{#if session.isAdmin}
							<PillButton variant="ghost" size="sm" onclick={() => (purgeConfirmId = m.id)}>
								{t('trash.purge')}
							</PillButton>
						{/if}
					{/if}
				</li>
			{/each}
		</ul>
	{/if}
	{#snippet footer()}
		<PillButton onclick={() => (trashDialog = false)}>{t('common.done')}</PillButton>
	{/snippet}
</Modal>

<style>
	.ml {
		display: flex;
		flex-direction: column;
		min-height: 0;
		border: 1px solid var(--hairline);
		border-radius: var(--radius-squircle-md);
		background: var(--glass-bg-thin);
		overflow: hidden;
	}

	.ml-toolbar {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-4) var(--space-5);
		border-bottom: 1px solid var(--hairline);
	}
	.ml-selmode {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		min-width: 2rem;
		min-height: 2.75rem;
	}
	.ml-search {
		flex: 1 1 auto;
		min-width: 0;
		display: flex;
		align-items: center;
		gap: var(--space-3);
		height: 2.5rem;
		padding: 0 var(--space-5);
		border-radius: var(--radius-pill);
		border: 1px solid var(--hairline);
		background: var(--glass-bg-thick);
		color: var(--color-text-secondary);
	}
	.ml-search:focus-within {
		border-color: var(--hairline-strong);
		box-shadow: var(--focus-ring);
	}
	.ml-search svg {
		width: 0.95rem;
		height: 0.95rem;
		flex-shrink: 0;
	}
	.ml-search input {
		flex: 1;
		min-width: 0;
		border: 0;
		outline: none;
		background: transparent;
		color: var(--color-text-primary);
		font: inherit;
		font-size: var(--text-subheadline);
	}
	.ml-tool {
		display: inline-flex;
		align-items: center;
		gap: var(--space-2);
		flex-shrink: 0;
		height: 2.5rem;
		padding: 0 var(--space-4);
		border-radius: var(--radius-pill);
		border: 1px solid var(--hairline);
		background: transparent;
		color: var(--color-text-secondary);
		font: inherit;
		font-size: var(--text-footnote);
		font-weight: var(--weight-medium);
		cursor: pointer;
		white-space: nowrap;
	}
	.ml-tool.icon {
		width: 2.5rem;
		padding: 0;
		justify-content: center;
	}
	.ml-tool:hover,
	.ml-tool.on {
		color: var(--color-text-primary);
		border-color: var(--hairline-strong);
	}
	.ml-tool svg {
		width: 1rem;
		height: 1rem;
	}

	.ml-more {
		position: relative;
	}
	.ml-menu {
		position: absolute;
		right: 0;
		top: calc(100% + var(--space-2));
		z-index: 40;
		min-width: 12rem;
		padding: var(--space-2);
		display: flex;
		flex-direction: column;
		border-radius: var(--radius-squircle-sm);
		border: 1px solid var(--hairline-strong);
		background: var(--glass-bg-thick);
		box-shadow: 0 18px 48px rgb(0 0 0 / 0.6);
	}
	.mi {
		padding: var(--space-3);
		border: 0;
		border-radius: var(--radius-squircle-sm);
		background: transparent;
		color: var(--color-text-primary);
		font: inherit;
		font-size: var(--text-subheadline);
		text-align: left;
		cursor: pointer;
	}
	.mi:hover {
		background: var(--hairline);
	}

	.ml-filters {
		display: grid;
		grid-template-columns: repeat(2, minmax(0, 1fr));
		gap: var(--space-3);
		padding: var(--space-4) var(--space-5);
		border-bottom: 1px solid var(--hairline);
	}

	.ml-bulk {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
		padding: var(--space-4) var(--space-5);
		border-bottom: 1px solid var(--hairline);
		background: var(--glass-bg-regular);
	}
	.ml-bulk-head,
	.ml-bulk-actions {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: var(--space-2);
	}
	.ml-bulk .count {
		margin: 0 auto 0 0;
		font-size: var(--text-footnote);
		font-weight: var(--weight-semibold);
		font-variant-numeric: tabular-nums;
	}

	.ml-body {
		overflow-y: auto;
		min-height: 0;
	}
	.ml-skel {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
		padding: var(--space-5);
	}
	.ml-head {
		margin: 0;
		padding: var(--space-4) var(--space-5) var(--space-2);
		font-size: var(--text-caption-1);
		font-weight: var(--weight-semibold);
		text-transform: uppercase;
		letter-spacing: var(--tracking-wide);
		color: var(--color-text-secondary);
		border-bottom: 1px solid var(--hairline);
	}

	.ml-folder {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: var(--space-3) var(--space-4);
		width: 100%;
		min-height: 3.5rem;
		padding: var(--space-4) var(--space-5);
		border: 0;
		border-bottom: 1px solid var(--hairline);
		background: transparent;
		color: var(--color-text-primary);
		font: inherit;
		text-align: left;
		cursor: pointer;
	}
	.ml-folder:hover {
		background: var(--glass-bg-regular);
	}
	.chev {
		width: 0.9rem;
		height: 0.9rem;
		flex-shrink: 0;
		color: var(--color-text-secondary);
		transition: transform var(--duration-sm) ease;
	}
	.chev.shut {
		transform: rotate(-90deg);
	}
	.ml-folder-info {
		flex: 1 1 9rem;
		min-width: 0;
		display: flex;
		align-items: center;
		gap: var(--space-3);
	}
	.ml-folder-name {
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		font-size: var(--text-subheadline);
		font-weight: var(--weight-semibold);
	}
	.ml-folder-count {
		flex-shrink: 0;
		font-size: var(--text-caption-1);
		color: var(--color-text-secondary);
		font-variant-numeric: tabular-nums;
	}
	.ml-folder :global(.bar) {
		flex: 0 1 50%;
		min-width: 7.5rem;
	}
	@media (max-width: 767px) {
		.ml-folder :global(.bar) {
			flex-basis: 100%;
		}
	}

	.dragging {
		opacity: 0.5;
	}

	/* ---- dialogs ---- */
	.dlgtext {
		margin: 0 0 var(--space-3);
		color: var(--color-text-secondary);
		font-size: var(--text-subheadline);
	}
	.namelist {
		margin: 0 0 var(--space-3);
		padding-left: var(--space-6);
		max-height: 12rem;
		overflow-y: auto;
		font-size: var(--text-subheadline);
	}
	.picks {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}
	.schedfields {
		display: grid;
		grid-template-columns: repeat(2, 1fr);
		gap: var(--space-3) var(--space-4);
	}
	@media (max-width: 480px) {
		.schedfields,
		.ml-filters {
			grid-template-columns: 1fr;
		}
	}
	.folders {
		list-style: none;
		margin: 0 0 var(--space-4);
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}
	.folderrow {
		display: flex;
		align-items: flex-end;
		gap: var(--space-2);
	}
	.folderrow :global(.field) {
		flex: 1;
		min-width: 0;
	}
	.newfolder {
		border-top: 1px solid var(--hairline);
		padding-top: var(--space-4);
	}
	.trashlist {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.trashrow {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-3) 0;
		border-bottom: 1px solid var(--hairline);
	}
	.trashrow:last-child {
		border-bottom: 0;
	}
	.tname {
		flex: 1 1 auto;
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		color: var(--color-text-primary);
	}
	.ttype {
		flex: 0 0 auto;
		font-size: var(--text-caption-1);
		color: var(--color-text-secondary);
	}
	.tconfirm {
		flex: 0 0 auto;
		font-size: var(--text-caption-1);
		color: var(--status-down);
	}
</style>
