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
