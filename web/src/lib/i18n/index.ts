/**
 * i18n barrel. Import `t` for translations, `i18n` for the locale/timezone
 * store, and the `format*` helpers for localized dates/numbers.
 */
export {
	t,
	i18n,
	browserTimezone,
	COMMON_TIMEZONES,
	LOCALES,
	type Locale,
	type LocaleMeta
} from './store.svelte';
export { formatDateTime, formatNumber } from './format';
export type { MessageKey, Messages } from './locales/en';
