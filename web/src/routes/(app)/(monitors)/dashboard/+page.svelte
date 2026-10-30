<script lang="ts">
	// The dashboard home: the right-hand pane of the monitor area. Laid out as
	// Uptime Kuma's home by default — quick stats over the instance-wide event
	// log — but still the arrangeable widget board it has been, so every other
	// widget remains one "Customise" away. The monitor list that used to be a
	// widget here now sits permanently on the left (see the (monitors) layout),
	// together with its search, filters, bulk actions, folders and the bin.
	import { onMount } from 'svelte';
	import {
		api,
		problemStatus,
		type AuditEntry,
		type CertificateRow,
		type MaintenanceWindow
	} from '$lib/api';
	import { monitors } from '$lib/stores/monitors.svelte';
	import { monitorList } from '$lib/stores/monitorList.svelte';
	import { prefs } from '$lib/stores/prefs.svelte';
	import { session } from '$lib/stores/session.svelte';
	import WidgetFrame from '$lib/components/dashboard/WidgetFrame.svelte';
	import MonitorListWidget from '$lib/components/dashboard/MonitorListWidget.svelte';
	import CertificateWidget from '$lib/components/dashboard/CertificateWidget.svelte';
	import MaintenanceWidget from '$lib/components/dashboard/MaintenanceWidget.svelte';
	import ActivityWidget from '$lib/components/dashboard/ActivityWidget.svelte';
	import EventsWidget from '$lib/components/dashboard/EventsWidget.svelte';
	import {
		addWidget,
		availableWidgets,
		columnSpan,
		nextWidth,
		removeWidget,
		reorderWidget,
		setWidth,
		widgetSpec,
		type WidgetType
	} from '$lib/dashboard/layout';
	import { t } from '$lib/i18n';
	import { EmptyState, Modal, PillButton, monitorStatuses } from '$lib/design-system';

	// Widget data that does not come from the monitors store. All three are
	// fetched unconditionally rather than per widget: they are two small requests, and
	// gating them on the current layout would make adding a widget in edit mode
	// show an empty tile until the next reload.
	//
	// The audit feed is admin-only on the server, so a non-admin never asks for
	// it — a 403 in the console on every dashboard load is noise that teaches
	// everyone to ignore the console.
	let maintenance = $state<MaintenanceWindow[]>([]);
	// Every other widget here has one. Without it the maintenance tile says
	// "Nothing planned." while the request is in flight and, worse, keeps saying
	// it after the request failed — the one thing it cannot know.
	// `loaded` means "we have an answer", NOT "the request finished". Setting it
	// in .finally() made a failed fetch render the empty state — which is the
	// exact thing the comment above says the flag exists to prevent.
	let maintenanceLoaded = $state(false);
	let maintenanceFailed = $state(false);
	let certificates = $state<CertificateRow[]>([]);
	let certsLoaded = $state(false);
	// Same reasoning as maintenanceFailed: a failed fetch is not "nothing here".
	let certsFailed = $state(false);
	let auditEntries = $state<AuditEntry[]>([]);
	let auditLoaded = $state(false);
	let auditFailed = $state(false);

	onMount(() => {
		// Each widget fetch catches its own failure. A widget that cannot load
		// shows its empty state — an operator cannot act on "the maintenance list
		// 500'd" from a dashboard tile — and a `.then` without a `.catch` leaves an
		// unhandled rejection in the console on every failed load.
		api
			.listMaintenance()
			.then((w) => {
				maintenance = w;
				maintenanceLoaded = true;
			})
			.catch(() => {
				maintenance = [];
				maintenanceFailed = true;
			});
		// Certificates answer 503 when reporting is unavailable — DEV_MODE without
		// a reporter — which is the ordinary case and reads as "none inspected",
		// not as a failure. Anything else is a failure and says so.
		api
			.certificateOverview()
			.then((rows) => {
				certificates = rows;
				certsLoaded = true;
			})
			.catch((e) => {
				certificates = [];
				if (problemStatus(e) === 503) certsLoaded = true;
				else certsFailed = true;
			});
		if (session.isAdmin) {
			api
				.auditLog()
				.then((rows) => {
					auditEntries = rows;
					auditLoaded = true;
				})
				.catch(() => {
					auditEntries = [];
					auditFailed = true;
				});
		} else {
			auditLoaded = true;
		}
	});

	const counts = $derived(monitors.byStatus);
	const total = $derived(monitors.items.length);

	const statusRank: Record<string, number> = {
		down: 0,
		pending: 1,
		maintenance: 2,
		up: 3,
		paused: 4
	};

	// ---- widget board --------------------------------------------------------

	let editingBoard = $state(false);
	let addOpen = $state(false);
	let dragWidget = $state<WidgetType | undefined>(undefined);

	/**
	 * The widgets actually rendered.
	 *
	 * An admin-only widget is skipped for everyone else rather than dropped from
	 * the stored layout: a viewer must not silently rewrite an arrangement they
	 * cannot see all of, and an admin who is temporarily demoted and promoted back
	 * finds their dashboard as they left it.
	 */
	const visibleWidgets = $derived(
		prefs.dashboard.filter((w) => !widgetSpec(w.type)?.adminOnly || session.isAdmin)
	);
	const addable = $derived(
		availableWidgets(prefs.dashboard).filter(
			(type) => !widgetSpec(type)?.adminOnly || session.isAdmin
		)
	);

	const widgetTitle = (type: WidgetType) => {
		const spec = widgetSpec(type);
		return spec ? t(spec.labelKey) : type;
	};
	const widgetDescription = (type: WidgetType) => {
		const spec = widgetSpec(type);
		return spec ? t(spec.descriptionKey) : '';
	};

	// Every mutation goes through prefs.setDashboard, which persists. Assigning
	// prefs.dashboard directly would update the screen and lose the change on
	// reload — the failure that looks like the whole feature not working.
	/**
	 * Moves a widget past its neighbour ON SCREEN.
	 *
	 * Not "one slot in the stored order": the stored layout can hold a widget
	 * this user cannot see (an admin-only one they no longer have the role for),
	 * and moving one slot in the stored order would then swap the visible widget
	 * with the hidden one — a button that changes the saved layout and changes
	 * nothing on screen. The neighbour is picked from what is rendered, and the
	 * reorder is expressed as "put this one where that one is", which is the same
	 * operation the drop handler performs.
	 */
	function move(type: WidgetType, delta: -1 | 1) {
		const i = visibleWidgets.findIndex((w) => w.type === type);
		const neighbour = visibleWidgets[i + delta];
		if (i < 0 || !neighbour) return;
		prefs.setDashboard(reorderWidget(prefs.dashboard, type, neighbour.type));
	}
	function cycleWidth(type: WidgetType) {
		const current = prefs.dashboard.find((w) => w.type === type);
		if (!current) return;
		prefs.setDashboard(setWidth(prefs.dashboard, type, nextWidth(current.width)));
	}
	function remove(type: WidgetType) {
		prefs.setDashboard(removeWidget(prefs.dashboard, type));
	}
	function add(type: WidgetType) {
		prefs.setDashboard(addWidget(prefs.dashboard, type));
		addOpen = false;
	}
	function dropWidget(target: WidgetType) {
		if (!dragWidget) return;
		prefs.setDashboard(reorderWidget(prefs.dashboard, dragWidget, target));
		dragWidget = undefined;
	}

	// ---- widget data ---------------------------------------------------------
	//
	// All derived from the monitors store, which the WebSocket keeps live, so the
	// widgets update with the list instead of on their own timers.

	/** Anything not up: down first, then pending, then in maintenance. */
	const attention = $derived(
		monitors.items
			.filter((m) => m.status === 'down' || m.status === 'pending' || m.status === 'maintenance')
			.sort((a, b) => statusRank[a.status] - statusRank[b.status] || a.name.localeCompare(b.name))
	);

	/**
	 * Worst 24-hour availability first.
	 *
	 * A monitor with no conclusive heartbeat in the window is excluded, not sorted
	 * as 0%: "no data" is a different answer from "down all day", and putting a
	 * monitor that was only just created at the top of a problem list is how a
	 * dashboard trains people to stop reading it.
	 */
	const worstUptime = $derived(
		monitors.items
			.filter((m) => m.uptime24h !== undefined && m.status !== 'paused')
			.sort((a, b) => (a.uptime24h ?? 100) - (b.uptime24h ?? 100))
	);

	/** Slowest last measurement first; monitors without one are excluded. */
	const slowest = $derived(
		monitors.items
			.filter((m) => m.latencyMs !== undefined && m.status !== 'paused')
			.sort((a, b) => (b.latencyMs ?? 0) - (a.latencyMs ?? 0))
	);
</script>

<svelte:head><title>{t('dashboard.pageTitle')}</title></svelte:head>

<!--
  The dashboard is a widget board.

  Each block below is a snippet or a widget component, and `prefs.dashboard`
  decides which ones render, in what order and at what width. The default is
  Kuma's home: the quick stats over the event log.
-->

{#snippet summaryWidget()}
	<!-- Kuma's quick stats. Each count is also a filter for the list on the
	     left: click "Down" and the list narrows to the down monitors. -->
	<div class="summary">
		<button
			class="stat"
			class:sel={monitorList.status === 'all'}
			aria-pressed={monitorList.status === 'all'}
			onclick={() => (monitorList.status = 'all')}
			data-k="total"
		>
			<span class="l">{t('status.total')}</span><span class="n">{total}</span>
		</button>
		{#each monitorStatuses as s (s)}
			<button
				class="stat"
				class:sel={monitorList.status === s}
				aria-pressed={monitorList.status === s}
				data-k={s}
				onclick={() => monitorList.toggleStatus(s)}
			>
				<span class="l">{t(`status.${s}`)}</span><span class="n">{counts[s]}</span>
			</button>
		{/each}
	</div>
{/snippet}

<!-- Board toolbar. Always present, even with an empty board — it is the only way
     back from one. -->
<div class="boardbar">
	{#if editingBoard}
		<PillButton
			variant="secondary"
			size="sm"
			disabled={addable.length === 0}
			onclick={() => (addOpen = true)}
		>
			{t('widget.add')}
		</PillButton>
		<PillButton variant="ghost" size="sm" onclick={() => prefs.resetDashboard()}>
			{t('widget.reset')}
		</PillButton>
	{/if}
	<PillButton
		variant={editingBoard ? 'primary' : 'ghost'}
		size="sm"
		aria-pressed={editingBoard}
		onclick={() => {
			editingBoard = !editingBoard;
			addOpen = false;
		}}
	>
		{editingBoard ? t('widget.editDone') : t('widget.edit')}
	</PillButton>
</div>

{#if visibleWidgets.length === 0}
	<div class="empty-wrap ds-glass">
		<EmptyState title={t('widget.boardEmptyTitle')} description={t('widget.boardEmptyBody')}>
			{#snippet action()}
				<PillButton onclick={() => prefs.resetDashboard()}>{t('widget.reset')}</PillButton>
			{/snippet}
		</EmptyState>
	</div>
{:else}
	<div class="board">
		{#each visibleWidgets as w, i (w.type)}
			<div class="slot" style="--span:{columnSpan(w.width)}">
				<WidgetFrame
					type={w.type}
					title={widgetTitle(w.type)}
					width={w.width}
					editing={editingBoard}
					first={i === 0}
					last={i === visibleWidgets.length - 1}
					dragging={dragWidget === w.type}
					onmove={(d) => move(w.type, d)}
					oncycleWidth={() => cycleWidth(w.type)}
					onremove={() => remove(w.type)}
					ondragstart={() => (dragWidget = w.type)}
					ondrop={() => dropWidget(w.type)}
					ondragend={() => (dragWidget = undefined)}
				>
					{#if w.type === 'summary'}
						{@render summaryWidget()}
					{:else if w.type === 'events'}
						<EventsWidget />
					{:else if w.type === 'attention'}
						<MonitorListWidget
							monitors={attention}
							metric="status"
							emptyText={t('widget.attentionEmpty')}
						/>
					{:else if w.type === 'uptime'}
						<MonitorListWidget
							monitors={worstUptime}
							metric="uptime"
							emptyText={t('widget.uptimeEmpty')}
						/>
					{:else if w.type === 'latency'}
						<MonitorListWidget
							monitors={slowest}
							metric="latency"
							sparkline
							emptyText={t('widget.latencyEmpty')}
						/>
					{:else if w.type === 'certificates'}
						<CertificateWidget rows={certificates} loaded={certsLoaded} failed={certsFailed} />
					{:else if w.type === 'maintenance'}
						<MaintenanceWidget
							windows={maintenance}
							loaded={maintenanceLoaded}
							failed={maintenanceFailed}
						/>
					{:else if w.type === 'activity'}
						<ActivityWidget entries={auditEntries} loaded={auditLoaded} failed={auditFailed} />
					{/if}
				</WidgetFrame>
			</div>
		{/each}
	</div>
{/if}

<Modal bind:open={addOpen} title={t('widget.addTitle')}>
	<div class="addlist">
		{#each addable as type (type)}
			<button type="button" class="addrow" onclick={() => add(type)}>
				<strong>{widgetTitle(type)}</strong>
				<span>{widgetDescription(type)}</span>
			</button>
		{/each}
		{#if addable.length === 0}
			<span class="sub">{t('widget.addNoneLeft')}</span>
		{/if}
	</div>
	{#snippet footer()}
		<PillButton variant="ghost" onclick={() => (addOpen = false)}>{t('common.cancel')}</PillButton>
	{/snippet}
</Modal>

<style>
	/* ---- widget board ---- */
	.boardbar {
		display: flex;
		justify-content: flex-end;
		gap: var(--space-2);
		margin-bottom: var(--space-4);
	}
	/* Six columns, so a widget can be a third, a half or the full width and every
	   combination still lines up. Below the breakpoint every widget spans the
	   whole row: a "third" on a phone is a column of clipped text, and the widths
	   are a desktop arrangement, not a responsive intent. */
	.board {
		display: grid;
		grid-template-columns: repeat(6, minmax(0, 1fr));
		gap: var(--space-5);
		align-items: start;
	}
	.slot {
		grid-column: span var(--span);
		min-width: 0;
	}
	@media (max-width: 960px) {
		.slot {
			grid-column: 1 / -1;
		}
	}
	.addlist {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	.addrow {
		display: flex;
		flex-direction: column;
		gap: 2px;
		width: 100%;
		padding: var(--space-3) var(--space-4);
		border-radius: var(--radius-squircle-md);
		border: 1px solid var(--glass-border);
		background: var(--glass-bg-thin);
		font: inherit;
		text-align: left;
		cursor: pointer;
	}
	.addrow:hover {
		border-color: var(--glass-border-strong);
	}
	.addrow strong {
		color: var(--color-text-primary);
		font-size: var(--text-subheadline);
	}
	.addrow span {
		color: var(--color-text-secondary);
		font-size: var(--text-caption-1);
	}

	/* Kuma's quick stats: the label over a big coloured count, centred, one
	   column per state. */
	.summary {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-3);
	}
	.stat {
		display: flex;
		flex-direction: column;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-4) var(--space-3);
		border-radius: var(--radius-squircle-sm);
		border: 1px solid transparent;
		background: transparent;
		color: var(--color-text-primary);
		font: inherit;
		cursor: pointer;
		flex: 1 1 6.5rem;
		min-width: 6.5rem;
		transition:
			border-color var(--duration-sm) ease,
			background-color var(--duration-sm) ease;
	}
	.stat:hover {
		border-color: var(--hairline-strong);
	}
	.stat.sel {
		border-color: var(--color-accent);
		background: var(--color-accent-tint);
	}
	.stat .n {
		font-size: var(--text-large-title);
		font-weight: var(--weight-bold);
		font-variant-numeric: tabular-nums;
		line-height: 1.1;
	}
	.stat .l {
		font-size: var(--text-subheadline);
		font-weight: var(--weight-semibold);
	}
	.stat[data-k='down'] .n {
		color: var(--status-down-text);
	}
	.stat[data-k='up'] .n {
		color: var(--status-up-text);
	}
	.stat[data-k='pending'] .n {
		color: var(--status-pending-text);
	}
	.stat[data-k='maintenance'] .n {
		color: var(--status-maintenance-text);
	}
	.stat[data-k='paused'] .n {
		color: var(--status-paused-text);
	}

	.empty-wrap {
		border-radius: var(--radius-squircle-lg);
		padding: var(--space-6);
	}
</style>
