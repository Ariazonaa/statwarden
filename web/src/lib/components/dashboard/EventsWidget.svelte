<script lang="ts">
	// "Important events": every status change of every monitor, newest first,
	// paged — the table Uptime Kuma shows on its dashboard home.
	//
	// It refreshes itself when a monitor changes state, but only while the
	// first page is showing: someone reading page 3 must not have the rows
	// shift under them because something went down meanwhile.
	import { onMount } from 'svelte';
	import { api, problemMessage, type MonitorEvent } from '$lib/api';
	import { monitors } from '$lib/stores/monitors.svelte';
	import { ws } from '$lib/ws/store.svelte';
	import { t, formatDateTime } from '$lib/i18n';
	import { PillButton, Skeleton, StatusPill } from '$lib/design-system';

	const PAGE = 25;

	let items = $state<MonitorEvent[]>([]);
	let total = $state(0);
	let offset = $state(0);
	let loaded = $state(false);
	let error = $state<string | undefined>(undefined);
	let timer: ReturnType<typeof setTimeout> | undefined;

	async function load(at = offset) {
		try {
			const page = await api.monitorEvents({ limit: PAGE, offset: at });
			items = page.items;
			total = page.total;
			offset = at;
			error = undefined;
			loaded = true;
		} catch (err) {
			// Keep whatever was showing; a failed refresh is not "no events".
			error = problemMessage(err) ?? t('events.loadError');
		}
	}

	onMount(() => {
		load(0);
		const off = ws.on((env) => {
			if (env.type !== 'state_change' || env.topic !== 'dashboard:all' || offset !== 0) return;
			// Debounced: a flapping fleet sends a burst, and one refetch answers it.
			clearTimeout(timer);
			timer = setTimeout(() => load(0), 800);
		});
		const offReconnect = ws.onReconnect(() => load(offset));
		return () => {
			off();
			offReconnect();
			clearTimeout(timer);
		};
	});

	const pages = $derived(Math.max(1, Math.ceil(total / PAGE)));
	const current = $derived(Math.floor(offset / PAGE) + 1);

	/** The monitor's name, or its id if it is not in the list (filtered, just deleted). */
	const nameOf = (id: string) => monitors.get(id)?.name ?? `#${id}`;
	const when = (at: string) =>
		formatDateTime(at, {
			year: 'numeric',
			month: '2-digit',
			day: '2-digit',
			hour: '2-digit',
			minute: '2-digit',
			second: '2-digit'
		});
</script>

{#if !loaded && !error}
	<Skeleton lines={5} />
{:else if !loaded && error}
	<p class="muted">{error}</p>
	<PillButton variant="secondary" size="sm" onclick={() => load(0)}>{t('common.retry')}</PillButton>
{:else if items.length === 0}
	<p class="muted">{t('events.empty')}</p>
{:else}
	{#if error}<p class="muted" role="status">{error}</p>{/if}
	<div class="scroll">
		<table class="ev">
			<thead>
				<tr>
					<th scope="col">{t('events.colName')}</th>
					<th scope="col">{t('events.colStatus')}</th>
					<th scope="col">{t('events.colTime')}</th>
					<th scope="col">{t('events.colMessage')}</th>
				</tr>
			</thead>
			<tbody>
				{#each items as ev (ev.id)}
					<tr>
						<td class="name"><a href="/monitors/{ev.monitorId}">{nameOf(ev.monitorId)}</a></td>
						<td><StatusPill status={ev.status} label={t(`status.${ev.status}`)} /></td>
						<td class="time">{when(ev.at)}</td>
						<td class="msg">{ev.message ?? '—'}</td>
					</tr>
				{/each}
			</tbody>
		</table>
	</div>
	{#if pages > 1}
		<nav class="pager" aria-label={t('events.pager')}>
			<PillButton
				variant="secondary"
				size="sm"
				disabled={current <= 1}
				onclick={() => load(Math.max(0, offset - PAGE))}
			>
				{t('events.prev')}
			</PillButton>
			<span class="where">{t('events.page', { page: current, pages })}</span>
			<PillButton
				variant="secondary"
				size="sm"
				disabled={current >= pages}
				onclick={() => load(offset + PAGE)}
			>
				{t('events.next')}
			</PillButton>
		</nav>
	{/if}
{/if}

<style>
	.muted {
		margin: 0 0 var(--space-3);
		color: var(--color-text-secondary);
		font-size: var(--text-subheadline);
	}
	.scroll {
		overflow-x: auto;
	}
	.ev {
		width: 100%;
		min-width: 40rem;
		border-collapse: collapse;
		font-size: var(--text-subheadline);
	}
	th {
		text-align: left;
		padding: var(--space-3) var(--space-4);
		font-size: var(--text-footnote);
		font-weight: var(--weight-semibold);
		color: var(--color-text-secondary);
		border-bottom: 1px solid var(--hairline);
	}
	td {
		padding: var(--space-4);
		border-bottom: 1px solid var(--hairline);
		vertical-align: middle;
	}
	tbody tr:last-child td {
		border-bottom: 0;
	}
	.name a {
		color: var(--color-text-primary);
		font-weight: var(--weight-medium);
		text-decoration: none;
	}
	.name a:hover {
		color: var(--color-accent);
		text-decoration: underline;
	}
	.time {
		white-space: nowrap;
		color: var(--color-text-secondary);
		font-variant-numeric: tabular-nums;
	}
	.msg {
		overflow-wrap: anywhere;
		color: var(--color-text-secondary);
	}
	.pager {
		display: flex;
		align-items: center;
		justify-content: center;
		gap: var(--space-4);
		margin-top: var(--space-5);
	}
	.where {
		font-size: var(--text-footnote);
		color: var(--color-text-secondary);
		font-variant-numeric: tabular-nums;
	}
</style>
