/**
 * Relative + absolute time formatting for timestamps across the dashboard.
 * `relativeTime` gives "3 min ago" (localized via Intl.RelativeTimeFormat);
 * `absoluteTime` gives the full local string used in tooltips, honouring the
 * user's selected timezone. Both read the active locale from the i18n store,
 * so timestamps re-render when the language or timezone changes.
 */
import { i18n } from '$lib/i18n/store.svelte';

const DIVISIONS: { amount: number; unit: Intl.RelativeTimeFormatUnit }[] = [
	{ amount: 60, unit: 'second' },
	{ amount: 60, unit: 'minute' },
	{ amount: 24, unit: 'hour' },
	{ amount: 7, unit: 'day' },
	{ amount: 4.34524, unit: 'week' },
	{ amount: 12, unit: 'month' },
	{ amount: Number.POSITIVE_INFINITY, unit: 'year' }
];

export function relativeTime(input: string | number | Date | undefined | null): string {
	if (input == null) return '—';
	const date = input instanceof Date ? input : new Date(input);
	if (Number.isNaN(date.getTime())) return '—';
	const rtf = new Intl.RelativeTimeFormat(i18n.localeTag, { numeric: 'auto' });
	let duration = (date.getTime() - Date.now()) / 1000;
	for (const division of DIVISIONS) {
		if (Math.abs(duration) < division.amount) {
			return rtf.format(Math.round(duration), division.unit);
		}
		duration /= division.amount;
	}
	return date.toLocaleDateString(i18n.localeTag);
}

export function absoluteTime(input: string | number | Date | undefined | null): string {
	if (input == null) return '—';
	const date = input instanceof Date ? input : new Date(input);
	if (Number.isNaN(date.getTime())) return '—';
	return new Intl.DateTimeFormat(i18n.localeTag, {
		year: 'numeric',
		month: 'short',
		day: 'numeric',
		hour: '2-digit',
		minute: '2-digit',
		second: '2-digit',
		timeZone: i18n.effectiveTimezone
	}).format(date);
}

/** Humanise a duration in seconds, e.g. 3720 → "1 h 2 min". */
export function humanDuration(seconds: number | undefined | null): string {
	if (seconds == null) return '—';
	const s = Math.max(0, Math.round(seconds));
	if (s < 60) return `${s} s`;
	const m = Math.floor(s / 60);
	if (m < 60) return `${m} min`;
	const h = Math.floor(m / 60);
	const rm = m % 60;
	if (h < 24) return rm ? `${h} h ${rm} min` : `${h} h`;
	const d = Math.floor(h / 24);
	const rh = h % 24;
	return rh ? `${d} d ${rh} h` : `${d} d`;
}
