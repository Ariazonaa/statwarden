/**
 * Locale + timezone store — a tiny, dependency-free i18n runtime built on
 * Svelte 5 runes (chosen over a library to keep the SPA bundle small and the
 * catalogs fully type-checked; see ./locales/en.ts).
 *
 * - Active locale is module-level `$state`, so reading it via `t()` or the
 *   `i18n` getters inside any template/`$derived` makes that UI update live
 *   the moment the locale changes.
 * - Locale + timezone are persisted to localStorage; locale falls back to the
 *   browser's `navigator.languages` on first visit.
 * - Timestamps honour a user-selected IANA timezone (or the browser's).
 */
import { browser } from '$app/environment';
import { en, type MessageKey, type Messages } from './locales/en';
import { de } from './locales/de';

export type Locale = 'en' | 'de';

export interface LocaleMeta {
	code: Locale;
	/** Name in the current UI language (via catalog is overkill; kept native). */
	label: string;
	/** BCP-47 tag used to drive `Intl` formatting. */
	tag: string;
}

export const LOCALES: LocaleMeta[] = [
	{ code: 'en', label: 'English', tag: 'en-US' },
	{ code: 'de', label: 'Deutsch', tag: 'de-DE' }
];

const catalogs: Record<Locale, Messages> = { en, de };
const tagByLocale: Record<Locale, string> = { en: 'en-US', de: 'de-DE' };

const LOCALE_KEY = 'statwarden:locale';
const TZ_KEY = 'statwarden:timezone';
const PLACEHOLDER = /\{(\w+)\}/g;

function isLocale(value: unknown): value is Locale {
	return value === 'en' || value === 'de';
}

/**
 * Zones offered by every timezone picker in the app (display settings, and the
 * per-window zone on a maintenance schedule). Deliberately a short curated list
 * rather than `Intl.supportedValuesOf('timeZone')`: that returns well over 400
 * entries, which is unusable in a dropdown. Callers append any zone already in
 * use so a value outside this list never silently disappears from its picker.
 */
export const COMMON_TIMEZONES = [
	'UTC',
	'Europe/London',
	'Europe/Berlin',
	'Europe/Paris',
	'Europe/Madrid',
	'Europe/Moscow',
	'America/New_York',
	'America/Chicago',
	'America/Denver',
	'America/Los_Angeles',
	'America/Sao_Paulo',
	'Asia/Dubai',
	'Asia/Kolkata',
	'Asia/Shanghai',
	'Asia/Tokyo',
	'Australia/Sydney'
];

/** Best-effort IANA timezone the browser is running in. */
export function browserTimezone(): string {
	try {
		return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
	} catch {
		return 'UTC';
	}
}

function detectLocale(): Locale {
	if (!browser) return 'en';
	try {
		const stored = localStorage.getItem(LOCALE_KEY);
		if (isLocale(stored)) return stored;
	} catch {
		/* storage unavailable */
	}
	const prefs =
		typeof navigator !== 'undefined'
			? navigator.languages?.length
				? navigator.languages
				: [navigator.language]
			: [];
	for (const pref of prefs) {
		const base = pref?.toLowerCase().split('-')[0];
		if (isLocale(base)) return base;
	}
	return 'en';
}

function readStoredTimezone(): string | null {
	if (!browser) return null;
	try {
		return localStorage.getItem(TZ_KEY) || null;
	} catch {
		return null;
	}
}

// Detected once into a plain value, then used for BOTH the initial rune and the
// initial <html lang>. Passing `current` to applyDocumentLang here instead made
// the compiler warn on every build (state_referenced_locally): a module-level read
// of a rune captures its initial value, which is what is wanted here and is
// indistinguishable from the mistake the warning is for. setLocale applies every
// later change. A warning that is always present is a warning nobody reads.
const initialLocale = detectLocale();

let current = $state<Locale>(initialLocale);
let timezone = $state<string | null>(readStoredTimezone());

function applyDocumentLang(locale: Locale): void {
	if (browser) document.documentElement.lang = locale;
}
applyDocumentLang(initialLocale);

export const i18n = {
	/** The active UI locale. */
	get locale(): Locale {
		return current;
	},
	/** BCP-47 tag for the active locale, e.g. "de-DE". */
	get localeTag(): string {
		return tagByLocale[current];
	},
	/** The active message catalog. */
	get messages(): Messages {
		return catalogs[current];
	},
	get locales(): LocaleMeta[] {
		return LOCALES;
	},
	setLocale(next: Locale): void {
		if (!isLocale(next) || next === current) return;
		current = next;
		applyDocumentLang(next);
		if (browser) {
			try {
				localStorage.setItem(LOCALE_KEY, next);
			} catch {
				/* ignore */
			}
		}
	},
	/** User-selected IANA timezone, or null to follow the browser. */
	get timezone(): string | null {
		return timezone;
	},
	/** Timezone actually used for formatting (selection or browser default). */
	get effectiveTimezone(): string {
		return timezone ?? browserTimezone();
	},
	setTimezone(tz: string | null): void {
		timezone = tz && tz.length ? tz : null;
		if (browser) {
			try {
				if (timezone) localStorage.setItem(TZ_KEY, timezone);
				else localStorage.removeItem(TZ_KEY);
			} catch {
				/* ignore */
			}
		}
	}
};

/**
 * Reactive translation. Reading the module-level `current` rune here means any
 * `{t('key')}` in a template re-renders when the locale changes. Unknown keys
 * fall back to English, then to the raw key, so the UI never shows blanks.
 */
export function t(key: MessageKey, params?: Record<string, string | number>): string {
	const message = catalogs[current][key] ?? en[key] ?? key;
	if (!params) return message;
	return message.replace(PLACEHOLDER, (whole, name: string) =>
		name in params ? String(params[name]) : whole
	);
}
