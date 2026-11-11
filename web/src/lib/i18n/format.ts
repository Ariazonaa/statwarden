/**
 * Locale- and timezone-aware formatting built on `Intl`. Every function reads
 * the active locale/timezone from the i18n store, so calling them inside a
 * template makes the output reactive to a language or zone switch (19.07.2026
 * for German vs. Jul 19, 2026 for English, and so on).
 */
import { i18n } from './store.svelte';

/** Localised date/time. Applies the user's effective timezone. */
export function formatDateTime(
	input: string | number | Date | undefined | null,
	options: Intl.DateTimeFormatOptions = {
		year: 'numeric',
		month: 'short',
		day: 'numeric',
		hour: '2-digit',
		minute: '2-digit'
	}
): string {
	if (input == null) return '—';
	const date = input instanceof Date ? input : new Date(input);
	if (Number.isNaN(date.getTime())) return '—';
	return new Intl.DateTimeFormat(i18n.localeTag, {
		...options,
		timeZone: i18n.effectiveTimezone
	}).format(date);
}

/** Localised number, e.g. 1,234.5 (en) vs 1.234,5 (de). */
export function formatNumber(value: number, options?: Intl.NumberFormatOptions): string {
	return new Intl.NumberFormat(i18n.localeTag, options).format(value);
}

/**
 * An uptime percentage (0..100), TRUNCATED to two decimals — never rounded.
 *
 * Rounding renders 99.996 % as "100 %", and an availability figure that calls
 * a window with downtime in it perfect is the one number a monitor must not
 * produce: truncation can only understate, so "100 %" means exactly that. Every
 * surface that shows an uptime goes through here, so the list, the detail page
 * and the public pages cannot disagree about the same window.
 *
 * `undefined` (no conclusive check in the window) renders as `empty`, never as
 * 0 — no data is a different answer from total downtime.
 */
export function formatUptime(pct: number | undefined, empty = '—'): string {
	if (pct === undefined || !Number.isFinite(pct)) return empty;
	// Rounded to a millionth of a hundredth first, so binary noise in a value
	// like 0.29 × 100 = 28.999999999999996 does not truncate a whole step down.
	const truncated = Math.floor(Math.round(pct * 100 * 1e6) / 1e6) / 100;
	return `${formatNumber(truncated, { maximumFractionDigits: truncated === 100 ? 0 : 2 })} %`;
}
