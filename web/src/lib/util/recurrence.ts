/**
 * Client-side reading of a maintenance recurrence: when does it next fire, and
 * what does it say in words.
 *
 * This is presentation only. The server (internal/maintenance) remains the sole
 * authority on whether a window is in effect — nothing here decides that, so
 * the two cannot disagree about suppression. What it does have to match is the
 * server's *anchoring* rule: occurrences are pinned to the LOCAL WALL CLOCK of
 * the window's own timezone, so 02:00 is 02:00 on both sides of a DST change
 * rather than a fixed UTC offset. Getting that wrong here would show an
 * operator a next-occurrence an hour away from the one the engine will use.
 */
import { i18n, t } from '$lib/i18n/store.svelte';
import { humanDuration } from './time';
import type { MaintenanceRecurrence } from '$lib/api/types';

/** Parse the strict `HH:MM` the server accepts. Returns null on anything else. */
export function parseClock(value: string): { hour: number; minute: number } | null {
	const m = /^(\d{2}):(\d{2})$/.exec(value);
	if (!m) return null;
	const hour = Number(m[1]);
	const minute = Number(m[2]);
	if (hour > 23 || minute > 59) return null;
	return { hour, minute };
}

/** True when the recurrence would pass the server's validator. */
export function isValidRecurrence(rec: MaintenanceRecurrence): boolean {
	if (rec.weekdays.length === 0) return false;
	if (new Set(rec.weekdays).size !== rec.weekdays.length) return false;
	if (rec.weekdays.some((d) => d < 0 || d > 6)) return false;
	if (!parseClock(rec.start)) return false;
	// Mirrors the server's cap of seven days.
	return rec.durationMinutes > 0 && rec.durationMinutes <= 7 * 24 * 60;
}

/** The offset of `zone` from UTC at a given instant, in milliseconds. */
function zoneOffset(zone: string, at: Date): number {
	const parts = new Intl.DateTimeFormat('en-US', {
		timeZone: zone,
		hourCycle: 'h23',
		year: 'numeric',
		month: '2-digit',
		day: '2-digit',
		hour: '2-digit',
		minute: '2-digit',
		second: '2-digit'
	}).formatToParts(at);
	const get = (type: Intl.DateTimeFormatPartTypes) =>
		Number(parts.find((p) => p.type === type)?.value);
	const asUTC = Date.UTC(
		get('year'),
		get('month') - 1,
		get('day'),
		get('hour'),
		get('minute'),
		get('second')
	);
	// The instant's own sub-second part is not represented above, so drop it
	// from both sides rather than letting it leak into the offset.
	return asUTC - Math.floor(at.getTime() / 1000) * 1000;
}

/** The wall clock `at` reads as in `zone`. */
function zonedParts(
	zone: string,
	at: Date
): { year: number; month: number; day: number; hour: number; minute: number } {
	const parts = new Intl.DateTimeFormat('en-US', {
		timeZone: zone,
		hourCycle: 'h23',
		year: 'numeric',
		month: '2-digit',
		day: '2-digit',
		hour: '2-digit',
		minute: '2-digit'
	}).formatToParts(at);
	const get = (type: Intl.DateTimeFormatPartTypes) =>
		Number(parts.find((p) => p.type === type)?.value);
	return {
		year: get('year'),
		month: get('month'),
		day: get('day'),
		hour: get('hour'),
		minute: get('minute')
	};
}

/**
 * DST fold sizes in use worldwide (Lord Howe shifts 30 minutes, almost
 * everywhere else 60, a few historical zones 120) — the same probe list the
 * server uses to resolve an ambiguous wall clock.
 */
const FOLD_PROBES = [2 * 36e5, 36e5, 30 * 6e4];

/**
 * The instant at which the given wall clock occurs in `zone`.
 *
 * Two passes: guess by treating the wall clock as UTC and subtracting the
 * offset in force around that guess, then re-read the offset at the resulting
 * instant and correct once if a DST transition sits between the two. One
 * correction is enough — no real zone changes offset twice within a day.
 *
 * On a spring-forward gap the named wall clock does not exist; the result lands
 * at the instant the clock jumps, which is what the server's `time.Date`
 * normalisation also produces.
 *
 * On an autumn-back fold it happens twice, and the two passes above settle on
 * the LATER of the two. The server explicitly picks the earlier one, so a fold
 * probe follows to match it — without this the two disagree by an hour once a
 * year, on exactly the night an operator is most likely to be watching.
 */
function instantAt(
	zone: string,
	year: number,
	month: number,
	day: number,
	hour: number,
	minute: number
): Date {
	const wall = Date.UTC(year, month - 1, day, hour, minute);
	const guess = wall - zoneOffset(zone, new Date(wall));
	const corrected = wall - zoneOffset(zone, new Date(guess));
	for (const probe of FOLD_PROBES) {
		const candidate = new Date(corrected - probe);
		const p = zonedParts(zone, candidate);
		if (
			p.year === year &&
			p.month === month &&
			p.day === day &&
			p.hour === hour &&
			p.minute === minute
		) {
			return candidate;
		}
	}
	return new Date(corrected);
}

/**
 * The start of the first occurrence at or after `from`, or null if the
 * recurrence is not valid.
 *
 * Days are enumerated as calendar dates rather than by adding 24 h to an
 * instant: a DST day is 23 or 25 hours long, and instant arithmetic would skip
 * or repeat a local date around the transition. Eight days are scanned so that
 * today counts even when its occurrence has already passed, and every weekday
 * is still reachable.
 */
export function nextOccurrence(
	rec: MaintenanceRecurrence,
	zone: string,
	from: Date = new Date()
): Date | null {
	if (!isValidRecurrence(rec)) return null;
	const clock = parseClock(rec.start);
	if (!clock) return null;
	const wanted = new Set(rec.weekdays);

	const today = zonedParts(zone, from);
	// Noon UTC as the cursor: adding whole days to it always lands on the next
	// calendar date, since no offset shift is anywhere near 12 hours.
	const cursor = Date.UTC(today.year, today.month - 1, today.day, 12);
	for (let i = 0; i <= 7; i++) {
		const day = new Date(cursor + i * 864e5);
		if (!wanted.has(day.getUTCDay())) continue;
		const start = instantAt(
			zone,
			day.getUTCFullYear(),
			day.getUTCMonth() + 1,
			day.getUTCDate(),
			clock.hour,
			clock.minute
		);
		if (start.getTime() >= from.getTime()) return start;
	}
	return null;
}

/**
 * Whether a recurring window is IN EFFECT at `at`, mirroring the server's
 * coversRecurring (internal/maintenance/window.go).
 *
 * This is a lookback, not a step back from the next occurrence. The obvious
 * shortcut — take nextOccurrence and subtract a week — is only correct for a
 * series with exactly one weekday: on a daily or Mon+Thu schedule it lands on an
 * occurrence a week old whose duration has long elapsed, so a window that IS
 * running reports as not running. It is also wrong across an autumn DST fold,
 * where seven calendar days are 7×24h+1h of elapsed time.
 *
 * The server walks back `durationMinutes/1440 + 1` candidate start days — enough
 * for an occurrence that began earlier and is still running, including one that
 * crosses midnight or runs for days — and asks whether any of them covers `at`.
 * So does this.
 *
 * The series bounds are honoured too: the server refuses coverage outside
 * [startsAt, endsAt), and ignoring them here made the UI announce occurrences of
 * a series the engine had already finished with.
 */
export function isRecurringInEffect(
	rec: MaintenanceRecurrence,
	zone: string,
	bounds: { startsAt?: string; endsAt?: string },
	at: Date = new Date()
): boolean {
	if (!isValidRecurrence(rec)) return false;
	const clock = parseClock(rec.start);
	if (!clock) return false;

	// Series bounds first, exactly as the server orders them.
	if (bounds.startsAt) {
		const from = Date.parse(bounds.startsAt);
		if (!Number.isNaN(from) && at.getTime() < from) return false;
	}
	if (bounds.endsAt) {
		const to = Date.parse(bounds.endsAt);
		if (!Number.isNaN(to) && at.getTime() >= to) return false;
	}

	const wanted = new Set(rec.weekdays);
	const durationMs = rec.durationMinutes * 60_000;
	const lookback = Math.floor(rec.durationMinutes / (24 * 60)) + 1;

	const today = zonedParts(zone, at);
	// Noon UTC as the cursor, for the reason nextOccurrence uses it: adding whole
	// days always lands on the intended calendar date, since no offset shift is
	// anywhere near twelve hours.
	const cursor = Date.UTC(today.year, today.month - 1, today.day, 12);
	for (let back = 0; back <= lookback; back++) {
		const day = new Date(cursor - back * 864e5);
		if (!wanted.has(day.getUTCDay())) continue;
		const start = instantAt(
			zone,
			day.getUTCFullYear(),
			day.getUTCMonth() + 1,
			day.getUTCDate(),
			clock.hour,
			clock.minute
		);
		const t = at.getTime();
		// Half-open [start, start+duration), matching the server.
		if (t >= start.getTime() && t < start.getTime() + durationMs) return true;
	}
	return false;
}

/**
 * Weekday values in display order — Monday first, as both UI languages read a
 * week. The values themselves stay `time.Weekday` (0 = Sunday), which is what
 * the server validates; only the order shown differs.
 */
export const WEEKDAY_ORDER = [1, 2, 3, 4, 5, 6, 0];

/** Short weekday names indexed 0..6, in the active UI locale. */
export function weekdayShortNames(): string[] {
	const fmt = new Intl.DateTimeFormat(i18n.localeTag, { weekday: 'short', timeZone: 'UTC' });
	// 2023-01-01 was a Sunday, so index 0 lines up with time.Weekday(0).
	return Array.from({ length: 7 }, (_, i) => fmt.format(new Date(Date.UTC(2023, 0, 1 + i))));
}

/**
 * The selected weekdays as localised short names, in week order regardless of
 * the order they were clicked in.
 */
export function formatWeekdays(weekdays: number[]): string {
	const names = weekdayShortNames();
	return [...new Set(weekdays)]
		.filter((d) => d >= 0 && d <= 6)
		.sort((a, b) => a - b)
		.map((d) => names[d])
		.join(', ');
}

/**
 * One human-readable line describing the recurrence, so an operator can check
 * what they configured without reading JSON, e.g.
 * "Mon, Wed, Fri · 02:00 for 2 h · Europe/Berlin".
 */
export function describeRecurrence(rec: MaintenanceRecurrence, zone: string): string {
	if (!isValidRecurrence(rec)) return '';
	return t('maintenance.recurrenceSummary', {
		days: formatWeekdays(rec.weekdays),
		start: rec.start,
		duration: humanDuration(rec.durationMinutes * 60),
		zone
	});
}
