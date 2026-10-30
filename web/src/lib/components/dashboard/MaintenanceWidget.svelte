<script lang="ts">
	/**
	 * Maintenance windows that are in effect now or still ahead.
	 *
	 * Past windows are dropped rather than greyed out: this widget answers "what
	 * is planned", and a list that keeps growing with finished work stops
	 * answering it after a month.
	 */
	import type { MaintenanceWindow } from '$lib/api';
	import { t } from '$lib/i18n';
	import { formatDateTime } from '$lib/i18n';
	import { EmptyState, Skeleton } from '$lib/design-system';
	import { nextOccurrence, describeRecurrence, isRecurringInEffect } from '$lib/util/recurrence';

	interface Props {
		windows: MaintenanceWindow[];
		limit?: number;
		/** True once the fetch has ANSWERED; false renders skeleton rows. */
		loaded?: boolean;
		/** True when the fetch failed, which is not the same as "nothing planned". */
		failed?: boolean;
	}
	let { windows, limit = 5, loaded = true, failed = false }: Props = $props();

	/**
	 * When a window next starts, in epoch milliseconds, or null if it cannot be
	 * placed on a clock.
	 *
	 * A RECURRING window's startsAt is an optional bound on the series, and it is
	 * EMPTY for a series that runs indefinitely (see MaintenanceWindow). Reading
	 * it as the start time produced a literal "Invalid Date" in the row and a
	 * NaN in the comparator — and a comparator returning NaN does not sort, it
	 * leaves the order to whatever the input happened to be. The series is asked
	 * for its next occurrence instead, which is what the maintenance page shows
	 * for the same window.
	 */
	function startOf(w: MaintenanceWindow): number | null {
		if (w.recurrence) {
			const next = nextOccurrence(w.recurrence, w.timezone);
			return next ? next.getTime() : null;
		}
		const at = Date.parse(w.startsAt);
		return Number.isNaN(at) ? null : at;
	}

	/**
	 * In effect now first, then the nearest upcoming, then anything whose start
	 * cannot be determined.
	 *
	 * `active` is the operator's enabled/disabled flag on this DTO — the public
	 * payload has a field with the same JSON name that does mean "in effect", and
	 * the two were conflated here. In-effect is derived from the clock instead,
	 * through the same rules the server evaluates (isRecurringInEffect).
	 *
	 * Date.now() is read inside the derivation rather than once at creation, so
	 * the list is at least current whenever the windows are refetched. It does
	 * not re-sort on a tab left open overnight — nothing here ticks — but a stale
	 * ordering is a far smaller thing than one that was stale from the moment the
	 * component was built.
	 */
	const upcoming = $derived.by(() => {
		const now = Date.now();
		return (
			windows
				// `active` is the operator's ENABLED flag, not "in effect" — so it can
				// never expire a window, and using it here kept every one-off window
				// that had ever been enabled, forever, filling the tile with finished
				// work and pushing real upcoming maintenance off the bottom.
				//
				// A window is worth showing when it is running now, or still ahead. A
				// recurring series is bounded by its own endsAt, not by an occurrence.
				.filter((w) => {
					if (!w.active) return false;
					if (isInEffect(w)) return true;
					if (w.recurrence) {
						const to = Date.parse(w.endsAt);
						return Number.isNaN(to) || to >= now;
					}
					return Date.parse(w.endsAt) >= now;
				})
				.sort((a, b) => {
					const ea = isInEffect(a);
					const eb = isInEffect(b);
					if (ea !== eb) return ea ? -1 : 1;
					const sa = startOf(a);
					const sb = startOf(b);
					// Unplaceable windows go last, and keep their relative order.
					if (sa === null || sb === null) return sa === null ? (sb === null ? 0 : 1) : -1;
					return sa - sb;
				})
		);
	});
	const rows = $derived(upcoming.slice(0, limit));

	/**
	 * Whether the window is running right now.
	 *
	 * Derived from the clock rather than read off `active`, which on the admin
	 * DTO means "enabled", not "in effect". A single window is in effect between
	 * its bounds; a recurring one is in effect when its next occurrence has
	 * already started and its duration has not elapsed.
	 */
	function isInEffect(w: MaintenanceWindow): boolean {
		if (!w.active) return false;
		if (w.recurrence) {
			return isRecurringInEffect(w.recurrence, w.timezone, {
				startsAt: w.startsAt,
				endsAt: w.endsAt
			});
		}
		const now = Date.now();
		const from = Date.parse(w.startsAt);
		const to = Date.parse(w.endsAt);
		if (Number.isNaN(from) || Number.isNaN(to)) return false;
		return from <= now && now < to;
	}

	/** The "when" line: in progress, the next occurrence, or the schedule itself. */
	function whenLabel(w: MaintenanceWindow): string {
		// `active` on THIS type is the operator's enabled/disabled flag, not
		// "in effect right now". PublicMaintenance has a field with the same JSON
		// name that does mean that, and the two were conflated here: every
		// ENABLED window, including one scheduled for next month, rendered "In
		// progress" and sorted to the top. In effect is derived from the clock
		// instead, which is the only thing this DTO gives us.
		if (isInEffect(w)) return t('widget.maintenanceNow');
		const at = startOf(w);
		if (at !== null) return formatDateTime(new Date(at).toISOString());
		// A recurring series with no computable next occurrence still has a
		// schedule worth showing; anything else genuinely has nothing to say.
		return w.recurrence ? describeRecurrence(w.recurrence, w.timezone) : '';
	}
</script>

{#if failed}
	<!-- A failed fetch is not "nothing planned" — that is the one answer this
	     widget cannot give when it never got one. -->
	<EmptyState title={t('widget.maintenanceUnavailable')} />
{:else if !loaded}
	<Skeleton lines={3} />
{:else if rows.length === 0}
	<EmptyState title={t('widget.maintenanceEmpty')} />
{:else}
	<ul class="rows">
		{#each rows as w (w.id)}
			<li>
				<!-- Plain <a href>: SvelteKit's router already intercepts an internal
				     link, and a hand-rolled preventDefault + goto() would swallow
				     ctrl-click and middle-click along with it. -->
				<a href="/maintenance">
					<span class="dot" class:active={w.active} aria-hidden="true"></span>
					<span class="txt">
						<span class="name" title={w.title}>{w.title}</span>
						<span class="when">{whenLabel(w)}</span>
					</span>
				</a>
			</li>
		{/each}
	</ul>
	{#if upcoming.length > rows.length}
		<p class="more">{t('widget.andMore', { n: upcoming.length - rows.length })}</p>
	{/if}
{/if}

<style>
	.rows {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.rows li + li {
		border-top: 1px solid var(--hairline);
	}
	a {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-3) 0;
		color: inherit;
		text-decoration: none;
		min-width: 0;
	}
	a:hover .name {
		color: var(--color-accent);
	}
	.dot {
		flex: 0 0 auto;
		width: 0.5rem;
		height: 0.5rem;
		border-radius: 50%;
		background: var(--color-text-tertiary);
	}
	.dot.active {
		background: var(--status-maintenance);
	}
	.txt {
		display: flex;
		flex-direction: column;
		min-width: 0;
	}
	.name {
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		font-size: var(--text-subheadline);
		color: var(--color-text-primary);
	}
	.when {
		font-size: var(--text-caption-1);
		color: var(--color-text-tertiary);
	}
	.more {
		margin: var(--space-3) 0 0;
		font-size: var(--text-caption-1);
		color: var(--color-text-tertiary);
	}
</style>
