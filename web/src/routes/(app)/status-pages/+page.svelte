<script lang="ts">
	// Status-page management: create/edit pages (branding, theme, custom domain,
	// password, indexing, monitor groups + per-monitor uptime visibility),
	// manage incidents (create + post updates + resolve) and reorder the
	// published layout.
	import { onMount } from 'svelte';
	import {
		api,
		problemStatus,
		problemMessage,
		type StatusPage,
		type StatusPageTheme,
		type StatusPageMonitorRef,
		type StatusPageLayoutOrder,
		type Incident,
		type IncidentStatus,
		type IncidentSeverity,
		type Monitor
	} from '$lib/api';
	import LayoutSorter from '$lib/components/statuspage/LayoutSorter.svelte';
	import Postmortem from '$lib/components/statuspage/Postmortem.svelte';
	import { postmortemTemplate } from '$lib/components/statuspage/postmortem';
	import type { Container } from '$lib/components/statuspage/layout';
	import { session } from '$lib/stores/session.svelte';
	import { t } from '$lib/i18n';
	import {
		Checkbox,
		ColorField,
		EmptyState,
		GlassPanel,
		ImageDropzone,
		Modal,
		PillButton,
		Select,
		Skeleton,
		Tabs,
		TextArea,
		TextField,
		toast
	} from '$lib/design-system';

	let pages = $state<StatusPage[]>([]);
	let allMonitors = $state<Monitor[]>([]);
	let loaded = $state(false);

	// Editor state.
	let editorOpen = $state(false);
	let editing = $state<StatusPage | undefined>(undefined);
	let f = $state(blankForm());
	let saving = $state(false);
	let confirmId = $state<string | undefined>(undefined);

	interface MonRow {
		monitorId: string;
		selected: boolean;
		groupName: string;
		showUptime: boolean;
	}

	function blankForm() {
		return {
			slug: '',
			title: '',
			description: '',
			footerText: '',
			locale: '',
			/**
			 * Not shown anywhere in this editor, and carried purely so a save
			 * cannot change it.
			 *
			 * The PUT is full-replace and the server coerces an unknown or absent
			 * theme to "auto" — so omitting it meant every save reset the page's
			 * theme, including saves that only fixed a typo in the footer. "auto"
			 * for a new page is the same value the server would have chosen.
			 */
			theme: 'auto' as StatusPageTheme,
			// Display state for the dropzone only — logo_path is server-owned and is
			// never sent back in buildPayload().
			logoPath: '',
			accentColor: '',
			customDomain: '',
			password: '',
			clearPassword: false,
			hasPassword: false,
			published: true,
			allowIndexing: true,
			rows: [] as MonRow[]
		};
	}

	onMount(async () => {
		try {
			[pages, allMonitors] = await Promise.all([api.listStatusPages(), api.listMonitors()]);
		} catch (e) {
			// Without this the failed load never flips `loaded`, so the skeleton
			// spins forever instead of surfacing the error.
			toast.error(problemMessage(e) ?? t('statusPages.saveError'));
		} finally {
			loaded = true;
		}
	});

	// Row order carries the page's display order: buildPayload() emits each
	// selected row's index as its sortOrder, so the rows must already be in the
	// stored order or a plain Save would flatten a drag-and-drop reorder. Placed
	// monitors come first in their persisted sortOrder, the rest keep the
	// monitor-list order behind them.
	function rowsFor(page?: StatusPage): MonRow[] {
		const placed = new Map((page?.monitors ?? []).map((r, i) => [r.monitorId, r.sortOrder ?? i]));
		return allMonitors
			.map((m) => {
				const ref = page?.monitors.find((r) => r.monitorId === m.id);
				return {
					monitorId: m.id,
					selected: !!ref || (page?.monitorIds.includes(m.id) ?? false),
					groupName: ref?.groupName ?? '',
					showUptime: ref?.showUptime ?? true
				};
			})
			.sort((a, b) => {
				const ao = placed.get(a.monitorId);
				const bo = placed.get(b.monitorId);
				if (ao === undefined) return bo === undefined ? 0 : 1;
				return bo === undefined ? -1 : ao - bo;
			});
	}

	function openCreate() {
		editing = undefined;
		f = blankForm();
		f.rows = rowsFor();
		editorOpen = true;
	}
	function openEdit(p: StatusPage) {
		editing = p;
		f = {
			slug: p.slug,
			title: p.title,
			description: p.description,
			footerText: p.footerText,
			locale: p.locale ?? '',
			theme: p.theme,
			logoPath: p.logoPath ?? '',
			accentColor: p.accentColor ?? '',
			customDomain: p.customDomain ?? '',
			password: '',
			clearPassword: false,
			hasPassword: p.hasPassword,
			published: p.published,
			allowIndexing: p.allowIndexing,
			rows: rowsFor(p)
		};
		editorOpen = true;
	}

	function buildPayload() {
		const selected = f.rows.filter((r) => r.selected);
		/**
		 * Placements this editor cannot show, carried through untouched.
		 *
		 * `rowsFor` maps over `allMonitors`, which excludes monitors in the recycle
		 * bin — so a binned monitor's placement has no row and no checkbox. This
		 * PUT is full-replace, so without carrying it forward, renaming the page
		 * would silently delete a placement that restoring the monitor is supposed
		 * to bring back. Before deleting became reversible the schema's cascade had
		 * already removed the row, and dropping it here was correct; it is not any
		 * more.
		 *
		 * Untouched means untouched: same group, same uptime visibility. The
		 * operator did not see it, so this edit must not decide anything about it.
		 */
		const known = new Set(allMonitors.map((m) => m.id));
		const carried = (editing?.monitors ?? []).filter((r) => !known.has(r.monitorId));
		// Where each VISIBLE placement sat on the stored page, so a carried one can
		// be merged back into that sequence rather than pushed to the end. A row
		// that was not on the page yet has no stored position and goes last.
		const placedOrder = new Map(
			(editing?.monitors ?? []).map((r, i) => [r.monitorId, r.sortOrder ?? i])
		);

		// Groups keep the page's stored order; names introduced in this edit are
		// appended. Both sequences are contiguous 0..n-1, which the store's
		// `if order == 0 { order = i }` coercion leaves alone.
		//
		// A carried placement's group name counts as named too, or the group it
		// sits in would not be recreated and the placement would land ungrouped.
		const named = [
			...new Set(
				[
					...selected.map((r) => r.groupName.trim()),
					...carried.map((r) => r.groupName ?? '')
				].filter(Boolean)
			)
		];
		const stored = [...(editing?.groups ?? [])]
			.sort((a, b) => (a.sortOrder ?? 0) - (b.sortOrder ?? 0))
			.map((g) => g.name)
			.filter((name) => named.includes(name));
		const groupNames = [...new Set([...stored, ...named])];
		// Carried placements keep their POSITION, not just their existence.
		//
		// Appending them after the visible rows renumbered them to the end of the
		// page, so restoring a monitor that had been third put it last — and any
		// unrelated save moved it again. `rowsFor` already sorts the visible rows
		// by their stored sortOrder, so both sequences are in page order and can
		// be merged on it: a carried placement slots back in front of every
		// visible row that used to follow it.
		const merged = [
			...selected.map((r) => ({
				monitorId: r.monitorId,
				groupName: r.groupName.trim() || undefined,
				showUptime: r.showUptime,
				at: placedOrder.get(r.monitorId) ?? Number.MAX_SAFE_INTEGER
			})),
			...carried.map((r, i) => ({
				monitorId: r.monitorId,
				groupName: r.groupName,
				showUptime: r.showUptime,
				at: r.sortOrder ?? i
			}))
		];
		// A stable sort, so two placements that share a stored order (or are both
		// new) keep the order they were listed in.
		const monitors: StatusPageMonitorRef[] = merged
			.map((m, i) => ({ m, i }))
			.sort((a, b) => a.m.at - b.m.at || a.i - b.i)
			.map(({ m }, i) => ({
				monitorId: m.monitorId,
				groupName: m.groupName,
				showUptime: m.showUptime,
				sortOrder: i
			}));
		return {
			slug: f.slug.trim(),
			title: f.title.trim(),
			description: f.description,
			footerText: f.footerText,
			// Carried, not edited. See blankForm.
			theme: f.theme,
			accentColor: f.accentColor || undefined,
			locale: f.locale || undefined,
			customDomain: f.customDomain.trim() || undefined,
			published: f.published,
			allowIndexing: f.allowIndexing,
			password: f.password || undefined,
			clearPassword: f.clearPassword || undefined,
			monitorIds: monitors.map((m) => m.monitorId),
			groups: groupNames.map((name, i) => ({ name, sortOrder: i })),
			monitors
		};
	}

	async function save() {
		if (!f.slug.trim() || !f.title.trim()) {
			toast.error(t('statusPages.slugTitleRequired'));
			return;
		}
		saving = true;
		try {
			const payload = buildPayload();
			if (editing) {
				const u = await api.updateStatusPage(editing.id, payload);
				pages = pages.map((p) => (p.id === u.id ? u : p));
			} else {
				pages = [...pages, await api.createStatusPage(payload)];
			}
			editorOpen = false;
			toast.success(t('statusPages.saved'));
		} catch (e) {
			// The server's own explanation, like every other handler in this file
			// already does. This one was the exception, and it is the path where
			// it matters most: the rules it can reject on are ones an operator
			// cannot guess from the form — the slug alphabet
			// (^[a-z0-9_-]{1,64}$), an accent colour that is not a hex value, a
			// duplicate or empty group name. "Could not save" leaves them
			// retyping the same thing.
			toast.error(problemMessage(e) ?? t('statusPages.saveError'));
		} finally {
			saving = false;
		}
	}

	// --- logo upload ---
	// The endpoint is /status-pages/{id}/logo, so a page must already be saved
	// before it can carry a logo; the dropzone is disabled until then.
	let logoBusy = $state(false);

	async function uploadLogo(file: File) {
		if (!editing) {
			toast.info(t('statusPages.logoSaveFirst'));
			return;
		}
		logoBusy = true;
		try {
			const u = await api.uploadStatusPageLogo(editing.id, file);
			f.logoPath = u.logoPath ?? '';
			pages = pages.map((p) => (p.id === u.id ? u : p));
			toast.success(t('statusPages.logoUploaded'));
		} catch (e) {
			const status = problemStatus(e);
			toast.error(
				status === 413
					? t('statusPages.logoTooLarge')
					: status === 415
						? t('statusPages.logoBadType')
						: t('statusPages.logoError')
			);
		} finally {
			logoBusy = false;
		}
	}

	async function removeLogo() {
		if (!editing) {
			f.logoPath = '';
			return;
		}
		const id = editing.id;
		try {
			await api.deleteStatusPageLogo(id);
			f.logoPath = '';
			pages = pages.map((p) => (p.id === id ? { ...p, logoPath: undefined } : p));
			toast.success(t('statusPages.logoRemoved'));
		} catch (e) {
			toast.error(problemMessage(e) ?? t('statusPages.logoError'));
		}
	}

	async function remove(id: string) {
		try {
			await api.deleteStatusPage(id);
			pages = pages.filter((p) => p.id !== id);
			confirmId = undefined;
			toast.success(t('statusPages.deleted'));
		} catch (e) {
			toast.error(problemMessage(e) ?? t('statusPages.saveError'));
		}
	}

	// --- incident management ---
	let manageOpen = $state(false);
	let managePage = $state<StatusPage | undefined>(undefined);
	let manageTab = $state('incidents');
	let incidents = $state<Incident[]>([]);

	// New-incident form.
	let incTitle = $state('');
	let incBody = $state('');
	let incSeverity = $state<IncidentSeverity>('minor');
	let incStatus = $state<IncidentStatus>('investigating');
	let incSaving = $state(false);
	// Per-incident update draft.
	let updateBody = $state<Record<string, string>>({});
	let updateStatus = $state<Record<string, IncidentStatus>>({});

	// --- layout ordering ---
	let layout = $state<Container[]>([]);
	/** Last server-confirmed order, and the only correct rollback target: `layout`
	 *  is bound into LayoutSorter, so by the time a commit fires the child has
	 *  already written the new order into it. */
	let layoutServer: Container[] = [];
	let layoutBusy = $state(false);

	function buildLayout(p: StatusPage): Container[] {
		// A placement whose monitor is not in `allMonitors` is one whose monitor is
		// in the recycle bin: the soft delete leaves status_page_monitors intact
		// (restoring must put it back), but listMonitors excludes it. Falling back
		// to the raw id would put a bare number in the editor with no explanation.
		const nameOf = (mid: string) =>
			allMonitors.find((m) => m.id === mid)?.name ?? t('statusPages.deletedMonitor');
		const sorted = [...p.monitors].sort((a, b) => (a.sortOrder ?? 0) - (b.sortOrder ?? 0));
		const groups = [...p.groups].sort((a, b) => (a.sortOrder ?? 0) - (b.sortOrder ?? 0));
		// Partition by group id, never by name: names are not unique, so a name match
		// duplicates a row into every same-named group and the duplicated monitor ids
		// then make each drag a no-op. A ref pointing at a group the page no longer
		// carries falls through to ungrouped, which is what the public page does too.
		const known = new Set(groups.map((g) => g.id));
		const groupOf = (m: StatusPageMonitorRef) =>
			m.groupId && known.has(m.groupId) ? m.groupId : undefined;
		const inGroup = (gid?: string) =>
			sorted
				.filter((m) => groupOf(m) === gid)
				.map((m) => ({ id: m.monitorId, name: nameOf(m.monitorId) }));
		// Ungrouped first. The public page renders it LAST, so the editor is not
		// strictly WYSIWYG — but LayoutSorter pins containers[0] as the immovable
		// block by index in four places (drop targets, the grip, the announcement
		// count), so reordering here silently inverts the drag and keyboard logic.
		// Fixing it properly means keying those on `id === null` instead; tracked
		// rather than done as a drive-by.
		return [
			{ id: null, name: t('statusPages.ungrouped'), monitors: inGroup(undefined) },
			...groups.map((g) => ({ id: g.id!, name: g.name, monitors: inGroup(g.id) }))
		];
	}

	function setLayout(p: StatusPage) {
		layout = buildLayout(p);
		layoutServer = $state.snapshot(layout) as Container[];
	}

	async function commitLayout(order: StatusPageLayoutOrder) {
		if (!managePage) return;
		const rollback = layoutServer;
		layoutBusy = true;
		try {
			const updated = await api.reorderStatusPageLayout(managePage.id, order);
			managePage = updated;
			pages = pages.map((p) => (p.id === updated.id ? updated : p));
			setLayout(updated);
			toast.success(t('statusPages.layoutSaved'));
		} catch (e) {
			layout = rollback;
			if (problemStatus(e) === 409) {
				toast.error(t('statusPages.layoutStale'));
				try {
					const fresh = await api.getStatusPage(managePage.id);
					managePage = fresh;
					setLayout(fresh);
				} catch {
					// Keep the rollback; the next openManage refetches anyway.
				}
			} else {
				toast.error(t('statusPages.layoutError'));
			}
		} finally {
			layoutBusy = false;
		}
	}

	async function openManage(p: StatusPage) {
		managePage = p;
		manageTab = 'incidents';
		manageOpen = true;
		layout = [];
		// The list response may predate the last save, and group rows — hence
		// group ids — are recreated on every save, so the layout tab has to work
		// from a freshly fetched page or its very first reorder 409s.
		const [inc, fresh] = await Promise.all([api.listIncidents(p.id), api.getStatusPage(p.id)]);
		// Drop drafts that match what the server had when they were seeded, i.e.
		// ones the operator opened and never typed into.
		//
		// openPostmortem seeds `pmDrafts[id] ??= inc.postmortem`, which also seeds
		// the EMPTY string, and draftOf() prefers the draft unconditionally. So
		// opening the editor on an incident with no write-up and closing it again
		// left an empty draft that then shadowed a retrospective a colleague
		// published in the meantime — and "Unpublish" sent that empty body and
		// erased it. An untouched draft has nothing to protect.
		for (const prev of incidents) {
			// NEVER the editor that is currently open. Deleting its entry leaves
			// the textarea bound to `undefined` — which renders empty next to a
			// "Published" badge, so the operator retypes into what looks like a
			// blank field and the save replaces the stored retrospective. It also
			// throws on the next remount (Svelte refuses an undefined initial
			// value for a $bindable with a fallback), which tears down the dialog
			// on a Preview toggle or a tab switch.
			if (prev.id === pmOpenFor) continue;
			if (pmDrafts[prev.id] === prev.postmortem) delete pmDrafts[prev.id];
		}
		incidents = inc;
		managePage = fresh;
		setLayout(fresh);
	}

	async function createIncident() {
		if (!managePage || !incTitle.trim()) {
			toast.error(t('statusPages.incidentTitleRequired'));
			return;
		}
		incSaving = true;
		try {
			const inc = await api.createIncident(managePage.id, {
				title: incTitle,
				body: incBody,
				severity: incSeverity,
				status: incStatus
			});
			incidents = [inc, ...incidents];
			incTitle = '';
			incBody = '';
			toast.success(t('statusPages.incidentCreated'));
		} catch (e) {
			toast.error(problemMessage(e) ?? t('statusPages.saveError'));
		} finally {
			incSaving = false;
		}
	}

	async function postUpdate(inc: Incident) {
		const body = updateBody[inc.id]?.trim();
		const status = updateStatus[inc.id] ?? inc.status;
		if (!body) {
			toast.error(t('statusPages.updateRequired'));
			return;
		}
		try {
			const updated = await api.addIncidentUpdate(inc.statusPageId, inc.id, { status, body });
			incidents = incidents.map((i) => (i.id === updated.id ? updated : i));
			updateBody[inc.id] = '';
			toast.success(
				status === 'resolved' ? t('statusPages.incidentResolved') : t('statusPages.updatePosted')
			);
		} catch (e) {
			toast.error(problemMessage(e) ?? t('statusPages.saveError'));
		}
	}

	// --- postmortem ---
	// One incident at a time is VISIBLE for editing, keyed by id: the modal lists
	// every incident, and a per-incident textarea bound to a shared draft would
	// let one write-up bleed into the next card.
	let pmOpenFor = $state<string | undefined>(undefined);
	let pmBusy = $state(false);
	let pmPreview = $state(false);
	/**
	 * The unsaved text, one entry per incident id.
	 *
	 * A single shared draft string was the source of three separate ways to lose
	 * half an hour of writing, and a confirm() on each exit was never going to
	 * cover them all: the dialog also closes on Escape, on a scrim click and on
	 * the close button, none of which a handler can veto. Keeping the text per
	 * incident means nothing has to be discarded in the first place — collapse
	 * the editor, open another one, close the dialog and come back, and the words
	 * are still there. It also makes the save unambiguous, where the shared
	 * string could write one incident's draft onto another and publish it.
	 *
	 * Entries live until the page is left or the incident is deleted. They are a
	 * few KiB of text at most, and outliving their usefulness is the point.
	 */
	let pmDrafts = $state<Record<string, string>>({});

	/** The draft for an incident, falling back to what the server has stored. */
	function draftOf(inc: Incident): string {
		return pmDrafts[inc.id] ?? inc.postmortem;
	}

	function openPostmortem(inc: Incident) {
		if (pmOpenFor === inc.id) {
			// Collapsing the editor keeps the draft; this is a show/hide, not a
			// discard.
			pmOpenFor = undefined;
			return;
		}
		pmOpenFor = inc.id;
		pmPreview = false;
		// Seeded from what is stored, NOT from the template: reopening an editor
		// must show what was written, and offering the scaffold as a button keeps
		// "insert the template" an explicit act that can be declined.
		pmDrafts[inc.id] ??= inc.postmortem;
	}

	function insertTemplate(inc: Incident) {
		if (draftOf(inc).trim() && !confirm(t('statusPages.postmortemTemplateOverwrite'))) return;
		pmDrafts[inc.id] = postmortemTemplate(
			{
				title: inc.title,
				startedAt: inc.startedAt,
				resolvedAt: inc.resolvedAt,
				updates: inc.updates,
				// A monitor in the recycle bin is not in allMonitors, and the id is
				// not a name: "Affected: mon_41" in a document published to the
				// public tells a reader nothing. The affected component is exactly
				// what an incident that outlived its monitor still needs to state,
				// so it falls back to a label rather than to an internal id.
				monitorNames: inc.monitorIds.map(
					(id) => allMonitors.find((m) => m.id === id)?.name ?? t('statusPages.monitorRemoved')
				)
			},
			t
		);
	}

	/**
	 * Saves the retrospective. `published` is passed through verbatim rather than
	 * inferred from the draft, because unpublishing is a real operation: the text
	 * stays, only its visibility changes.
	 */
	async function savePostmortem(inc: Incident, published: boolean) {
		pmBusy = true;
		try {
			const updated = await api.setIncidentPostmortem(inc.statusPageId, inc.id, {
				body: draftOf(inc),
				published
			});
			incidents = incidents.map((i) => (i.id === updated.id ? updated : i));
			// Re-seeded from the response so the editor shows what the server
			// actually stored (it trims).
			pmDrafts[updated.id] = updated.postmortem;
			toast.success(
				published
					? t('statusPages.postmortemWasPublished')
					: inc.postmortemPublishedAt
						? t('statusPages.postmortemWasUnpublished')
						: t('statusPages.postmortemSaved')
			);
		} catch (e) {
			// 409 is the one failure with a specific remedy — resolve the incident
			// first — so it gets its own message instead of the generic save error.
			toast.error(
				problemStatus(e) === 409
					? t('statusPages.postmortemNeedsResolved')
					: (problemMessage(e) ?? t('statusPages.saveError'))
			);
		} finally {
			pmBusy = false;
		}
	}

	async function deleteIncident(inc: Incident) {
		try {
			await api.deleteIncident(inc.statusPageId, inc.id);
			incidents = incidents.filter((i) => i.id !== inc.id);
			// Its editor is gone with it, so the draft goes too: nothing will ever
			// be able to save that text again, and keeping it would only offer it
			// back for an incident that no longer exists.
			if (pmOpenFor === inc.id) pmOpenFor = undefined;
			delete pmDrafts[inc.id];
			toast.success(t('statusPages.incidentDeleted'));
		} catch (e) {
			toast.error(problemMessage(e) ?? t('statusPages.saveError'));
		}
	}

	const languageOptions = $derived([
		{ value: '', label: t('statusPages.languageDefault') },
		{ value: 'en', label: 'English' },
		{ value: 'de', label: 'Deutsch' }
	]);
	const severityOptions = $derived([
		{ value: 'minor', label: t('statusPages.severityMinor') },
		{ value: 'major', label: t('statusPages.severityMajor') },
		{ value: 'critical', label: t('statusPages.severityCritical') }
	]);
	const statusOptions = $derived([
		{ value: 'investigating', label: t('statusPages.statusInvestigating') },
		{ value: 'identified', label: t('statusPages.statusIdentified') },
		{ value: 'monitoring', label: t('statusPages.statusMonitoring') },
		{ value: 'resolved', label: t('statusPages.statusResolved') }
	]);
	const manageTabs = $derived([
		{ id: 'incidents', label: t('statusPages.tabIncidents') },
		{ id: 'layout', label: t('statusPages.tabLayout') }
	]);

	function severityLabel(sev: IncidentSeverity): string {
		return sev === 'minor'
			? t('statusPages.severityMinor')
			: sev === 'major'
				? t('statusPages.severityMajor')
				: t('statusPages.severityCritical');
	}
	function incidentStatusLabel(status: IncidentStatus): string {
		return status === 'investigating'
			? t('statusPages.statusInvestigating')
			: status === 'identified'
				? t('statusPages.statusIdentified')
				: status === 'monitoring'
					? t('statusPages.statusMonitoring')
					: t('statusPages.statusResolved');
	}
</script>

<svelte:head><title>{t('statusPages.pageTitle')}</title></svelte:head>

<div class="pagehead">
	<p class="lead">{t('statusPages.lead')}</p>
	{#if session.canEdit}
		<PillButton size="sm" onclick={openCreate}>{t('statusPages.newPage')}</PillButton>
	{/if}
</div>

{#if !loaded}
	<Skeleton variant="rect" height="8rem" />
{:else if pages.length === 0}
	<GlassPanel radius="lg" class="sp-wrap">
		<EmptyState title={t('statusPages.emptyTitle')} description={t('statusPages.emptyDescription')}>
			{#snippet action()}<PillButton onclick={openCreate}>{t('statusPages.newPage')}</PillButton
				>{/snippet}
		</EmptyState>
	</GlassPanel>
{:else}
	<div class="list">
		{#each pages as p (p.id)}
			<GlassPanel radius="md" class="sp-row">
				<div class="info">
					<div class="titleline">
						<strong>{p.title}</strong>
						{#if !p.published}<span class="badge">{t('common.draft')}</span>{/if}
						{#if p.hasPassword}<span class="badge">{t('common.protected')}</span>{/if}
					</div>
					<span class="sub">/status/{p.slug}{p.customDomain ? ` · ${p.customDomain}` : ''}</span>
					<span class="sub">
						{p.monitorIds.length === 1
							? t('maintenance.monitorCount', { n: p.monitorIds.length })
							: t('maintenance.monitorCountPlural', { n: p.monitorIds.length })} ·
						{t('statusPages.viewsLabel', { n: p.viewCount })}
					</span>
				</div>
				<div class="rowactions">
					<a class="viewlink" href="/status/{p.slug}" target="_blank" rel="noreferrer"
						>{t('common.view')}</a
					>
					{#if session.canEdit}
						<PillButton variant="ghost" size="sm" onclick={() => openManage(p)}
							>{t('common.manage')}</PillButton
						>
						<PillButton variant="ghost" size="sm" onclick={() => openEdit(p)}
							>{t('common.edit')}</PillButton
						>
						<PillButton variant="ghost" size="sm" onclick={() => (confirmId = p.id)}
							>{t('common.delete')}</PillButton
						>
					{/if}
				</div>
			</GlassPanel>
		{/each}
	</div>
{/if}

<!-- Editor -->
<Modal
	bind:open={editorOpen}
	title={editing ? t('statusPages.editPage') : t('statusPages.newPage')}
>
	<div class="form">
		<div class="two">
			<TextField
				label={t('statusPages.slug')}
				bind:value={f.slug}
				placeholder="acme"
				hint={t('statusPages.slugHint')}
				maxlength={64}
				required
			/>
			<TextField
				label={t('statusPages.title')}
				bind:value={f.title}
				placeholder={t('statusPages.titlePlaceholder')}
				required
			/>
		</div>
		<TextArea label={t('statusPages.description')} bind:value={f.description} rows={2} />
		<TextArea label={t('statusPages.footerText')} bind:value={f.footerText} rows={1} />
		<Select label={t('statusPages.language')} options={languageOptions} bind:value={f.locale} />
		<div>
			<span class="pl">{t('statusPages.branding')}</span>
			<div class="form">
				<ImageDropzone
					label={t('statusPages.logo')}
					hint={editing ? t('statusPages.logoHint') : t('statusPages.logoSaveFirst')}
					src={f.logoPath}
					busy={logoBusy}
					disabled={!session.canEdit || !editing}
					promptText={t('statusPages.logoDropPrompt')}
					chooseLabel={t('statusPages.logoChoose')}
					removeLabel={t('statusPages.logoRemove')}
					previewAlt={t('statusPages.logoPreviewAlt')}
					onselect={uploadLogo}
					onremove={removeLogo}
				/>
				<ColorField
					label={t('statusPages.accentColor')}
					bind:value={f.accentColor}
					hint={t('statusPages.accentHint')}
					clearLabel={t('statusPages.accentReset')}
					disabled={!session.canEdit}
				/>
			</div>
		</div>
		<div class="two">
			<TextField
				label={t('statusPages.customDomain')}
				bind:value={f.customDomain}
				placeholder="status.acme.com"
			/>
			<div class="toggles">
				<Checkbox label={t('statusPages.published')} bind:checked={f.published} />
				<Checkbox label={t('statusPages.allowIndexing')} bind:checked={f.allowIndexing} />
			</div>
		</div>
		<div class="two">
			<div>
				{#if f.hasPassword}
					<Checkbox label={t('statusPages.removePassword')} bind:checked={f.clearPassword} />
				{/if}
				<TextField
					label={f.hasPassword ? t('statusPages.newPassword') : t('statusPages.passwordOptional')}
					type="password"
					bind:value={f.password}
					disabled={f.clearPassword}
				/>
			</div>
		</div>
		<div>
			<span class="pl">{t('statusPages.monitorsGroups')}</span>
			<div class="monlist">
				{#each f.rows as row (row.monitorId)}
					<div class="monrow" class:on={row.selected}>
						<Checkbox
							label={allMonitors.find((m) => m.id === row.monitorId)?.name ?? row.monitorId}
							bind:checked={row.selected}
						/>
						{#if row.selected}
							<input
								class="grpin"
								placeholder={t('statusPages.groupOptional')}
								bind:value={row.groupName}
							/>
							<label class="showup">
								<input type="checkbox" bind:checked={row.showUptime} />
								{t('statusPages.uptime')}
							</label>
						{/if}
					</div>
				{/each}
				{#if f.rows.length === 0}<span class="sub">{t('statusPages.noMonitors')}</span>{/if}
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

<!-- Manage incidents -->
<Modal
	bind:open={manageOpen}
	title={managePage
		? t('statusPages.manageTitle', { title: managePage.title })
		: t('common.manage')}
	onclose={() => (pmOpenFor = undefined)}
>
	<Tabs tabs={manageTabs} bind:active={manageTab} />
	{#if manageTab === 'incidents'}
		<div class="form manage">
			<GlassPanel radius="md" class="newinc">
				<span class="pl">{t('statusPages.newIncident')}</span>
				<TextField
					label={t('statusPages.incidentTitle')}
					bind:value={incTitle}
					placeholder={t('statusPages.incidentTitlePlaceholder')}
				/>
				<TextArea label={t('statusPages.incidentMessage')} bind:value={incBody} rows={2} />
				<div class="two">
					<Select
						label={t('statusPages.severity')}
						options={severityOptions}
						bind:value={incSeverity}
					/>
					<Select label={t('statusPages.status')} options={statusOptions} bind:value={incStatus} />
				</div>
				<PillButton size="sm" loading={incSaving} onclick={createIncident}
					>{t('statusPages.createIncident')}</PillButton
				>
			</GlassPanel>

			{#each incidents as inc (inc.id)}
				<GlassPanel radius="md" class="inccard">
					<div class="titleline">
						<strong>{inc.title}</strong>
						<span class="badge sev-{inc.severity}">{severityLabel(inc.severity)}</span>
						<span class="badge">{incidentStatusLabel(inc.status)}</span>
					</div>
					<ol class="timeline">
						{#each inc.updates as u (u.id)}
							<li><b>{incidentStatusLabel(u.status)}</b> — {u.body}</li>
						{/each}
					</ol>
					{#if inc.status !== 'resolved'}
						<div class="upd">
							<Select
								label={t('statusPages.status')}
								options={statusOptions}
								value={updateStatus[inc.id] ?? inc.status}
								onchange={(v) => (updateStatus[inc.id] = v as IncidentStatus)}
							/>
							<input
								class="grpin"
								placeholder={t('statusPages.updatePlaceholder')}
								bind:value={updateBody[inc.id]}
							/>
							<PillButton size="sm" onclick={() => postUpdate(inc)}>{t('common.post')}</PillButton>
						</div>
					{/if}

					<!-- Postmortem. Offered on every incident, not only resolved ones:
					     the notes get written while the outage is fresh. Publishing is
					     what requires a closed incident, and the button says so. -->
					<div class="pmrow">
						<PillButton variant="ghost" size="sm" onclick={() => openPostmortem(inc)}>
							{pmOpenFor === inc.id
								? t('statusPages.postmortemHide')
								: t('statusPages.postmortemEdit')}
						</PillButton>
						{#if inc.postmortemPublishedAt}
							<span class="badge pm-pub">{t('statusPages.postmortemPublished')}</span>
						{:else if inc.postmortem}
							<span class="badge">{t('statusPages.postmortemDraft')}</span>
						{/if}
						<PillButton variant="ghost" size="sm" onclick={() => deleteIncident(inc)}
							>{t('common.delete')}</PillButton
						>
					</div>

					{#if pmOpenFor === inc.id}
						<div class="pmedit">
							<div class="pmtools">
								<!-- disabled during the save for the same reason the textarea is:
								     the response is re-seeded into the draft, so a scaffold
								     inserted mid-flight is silently overwritten. -->
								<PillButton
									variant="secondary"
									size="sm"
									disabled={pmBusy}
									onclick={() => insertTemplate(inc)}
								>
									{t('statusPages.postmortemTemplate')}
								</PillButton>
								<PillButton
									variant="ghost"
									size="sm"
									aria-pressed={pmPreview}
									onclick={() => (pmPreview = !pmPreview)}
								>
									{t('statusPages.postmortemPreview')}
								</PillButton>
							</div>
							{#if pmPreview}
								<div class="pmpreview">
									{#if draftOf(inc).trim()}
										<Postmortem source={draftOf(inc)} />
									{:else}
										<span class="sub">{t('statusPages.postmortemNone')}</span>
									{/if}
								</div>
							{:else}
								<!-- disabled while the save is in flight: the response is
								     re-seeded into this very binding, so anything typed in
								     the meantime was silently overwritten and the caret
								     jumped to the end of the server's text. -->
								<TextArea
									label={t('statusPages.postmortem')}
									bind:value={pmDrafts[inc.id]}
									rows={12}
									disabled={pmBusy}
									placeholder={t('statusPages.postmortemPlaceholder')}
								/>
							{/if}
							{#if !inc.postmortemPublishedAt}
								<span class="sub">{t('statusPages.postmortemDraftNotice')}</span>
							{/if}
							<div class="pmactions">
								<PillButton
									variant="secondary"
									size="sm"
									loading={pmBusy}
									onclick={() => savePostmortem(inc, false)}
								>
									{inc.postmortemPublishedAt
										? t('statusPages.postmortemUnpublish')
										: t('statusPages.postmortemSaveDraft')}
								</PillButton>
								<PillButton
									size="sm"
									loading={pmBusy}
									disabled={!draftOf(inc).trim() ||
										(inc.status !== 'resolved' && !inc.postmortemPublishedAt)}
									title={inc.status !== 'resolved'
										? t('statusPages.postmortemNeedsResolved')
										: undefined}
									onclick={() => savePostmortem(inc, true)}
								>
									{t('statusPages.postmortemPublish')}
								</PillButton>
							</div>
						</div>
					{/if}
				</GlassPanel>
			{/each}
			{#if incidents.length === 0}<span class="sub">{t('statusPages.noIncidents')}</span>{/if}
		</div>
	{:else}
		<div class="form manage">
			{#if !session.canEdit}
				<span class="sub">{t('statusPages.layoutReadOnly')}</span>
			{:else if layout.length === 0}
				<Skeleton variant="rect" height="8rem" />
			{:else if layout.length === 1 && layout[0].monitors.length === 0}
				<span class="sub">{t('statusPages.noMonitors')}</span>
			{:else}
				<LayoutSorter bind:containers={layout} busy={layoutBusy} oncommit={commitLayout} />
			{/if}
		</div>
	{/if}
	{#snippet footer()}
		<!-- No discard prompt: the drafts are kept per incident and survive this,
		     which is the only behaviour Escape and the scrim could have honoured
		     anyway. -->
		<PillButton
			onclick={() => {
				pmOpenFor = undefined;
				manageOpen = false;
			}}>{t('common.done')}</PillButton
		>
	{/snippet}
</Modal>

<Modal
	open={confirmId != null}
	title={t('statusPages.deleteTitle')}
	onclose={() => (confirmId = undefined)}
>
	<p style="margin:0">{t('statusPages.deleteBody')}</p>
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
	/* ---- postmortem editor ---- */
	.pmrow {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		flex-wrap: wrap;
	}
	.badge.pm-pub {
		background: var(--status-up-tint);
	}
	.pmedit {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
		margin-top: var(--space-3);
		padding-top: var(--space-3);
		border-top: 1px solid var(--hairline);
	}
	.pmtools,
	.pmactions {
		display: flex;
		gap: var(--space-2);
		flex-wrap: wrap;
	}
	.pmpreview {
		padding: var(--space-3);
		border: 1px solid var(--hairline);
		border-radius: var(--radius-squircle-md);
	}

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
	:global(.sp-wrap) {
		padding: var(--space-5);
	}
	.list {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}
	:global(.sp-row) {
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
		flex-wrap: wrap;
	}
	.badge {
		font-size: var(--text-caption-2);
		padding: 1px var(--space-3);
		border-radius: var(--radius-pill);
		background: var(--color-fill-secondary, rgba(128, 128, 128, 0.15));
		text-transform: capitalize;
	}
	.badge.sev-major {
		background: rgba(249, 115, 22, 0.2);
	}
	.badge.sev-critical {
		background: rgba(239, 68, 68, 0.2);
	}
	.sub {
		font-size: var(--text-caption-1);
		color: var(--color-text-tertiary);
	}
	.rowactions {
		display: flex;
		gap: var(--space-1);
		flex-shrink: 0;
		align-items: center;
	}
	.viewlink {
		font-size: var(--text-footnote);
		color: var(--color-accent, #6366f1);
		padding: 0 var(--space-2);
	}
	.form {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}
	.form.manage {
		max-height: 60vh;
		overflow-y: auto;
		margin-top: var(--space-4);
	}
	.two {
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: var(--space-4);
	}
	.toggles {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
		justify-content: center;
	}
	.pl {
		display: block;
		font-size: var(--text-footnote);
		font-weight: var(--weight-medium);
		color: var(--color-text-secondary);
		margin-bottom: var(--space-3);
	}
	.monlist {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		max-height: 14rem;
		overflow-y: auto;
	}
	.monrow {
		display: flex;
		align-items: center;
		gap: var(--space-3);
	}
	.grpin {
		flex: 1;
		min-width: 6rem;
		padding: 4px 8px;
		border-radius: var(--radius-md, 8px);
		border: 1px solid var(--color-border, rgba(128, 128, 128, 0.25));
		background: transparent;
		color: inherit;
	}
	.showup {
		display: flex;
		align-items: center;
		gap: 4px;
		font-size: var(--text-caption-1);
		color: var(--color-text-secondary);
	}
	:global(.newinc),
	:global(.inccard) {
		padding: var(--space-4);
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}
	.timeline {
		list-style: none;
		display: flex;
		flex-direction: column;
		gap: 2px;
		font-size: var(--text-caption-1);
		color: var(--color-text-secondary);
	}
	.upd {
		display: flex;
		gap: var(--space-2);
		align-items: center;
	}
	@media (max-width: 640px) {
		.two {
			grid-template-columns: 1fr;
		}
		/* Four actions beside the title leave it a one-word column on a phone. */
		:global(.sp-row) {
			flex-direction: column;
			align-items: stretch;
			gap: var(--space-3);
		}
		.rowactions {
			flex-wrap: wrap;
		}
	}
</style>
