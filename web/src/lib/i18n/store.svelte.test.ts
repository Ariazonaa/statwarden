/**
 * A view-only locale must not leak into the visitor's saved preference.
 *
 * The public status pages switch to the language their owner configured. That
 * used to go through the same setter as the settings screen, which also wrote
 * localStorage — so opening one German status page turned the operator's whole
 * dashboard German on the next visit.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const KEY = 'statwarden:locale';

async function freshStore() {
	vi.resetModules();
	return (await import('./store.svelte')).i18n;
}

beforeEach(() => {
	localStorage.clear();
});
afterEach(() => {
	vi.resetModules();
});

describe('i18n.setLocale', () => {
	it('saves an explicit choice', async () => {
		localStorage.setItem(KEY, 'en');
		const i18n = await freshStore();
		i18n.setLocale('de');
		expect(i18n.locale).toBe('de');
		expect(localStorage.getItem(KEY)).toBe('de');
	});

	it('applies a view-only locale without touching the saved one', async () => {
		localStorage.setItem(KEY, 'en');
		const i18n = await freshStore();
		i18n.setLocale('de', { persist: false });
		expect(i18n.locale).toBe('de');
		expect(document.documentElement.lang).toBe('de');
		expect(localStorage.getItem(KEY)).toBe('en');
	});

	it('returns to the visitor’s own locale when the view-only one is dropped', async () => {
		localStorage.setItem(KEY, 'en');
		const i18n = await freshStore();
		i18n.setLocale('de', { persist: false });
		i18n.resetLocale();
		expect(i18n.locale).toBe('en');
		expect(document.documentElement.lang).toBe('en');
	});

	it('does not create a saved locale for a visitor who never chose one', async () => {
		const i18n = await freshStore();
		i18n.setLocale(i18n.locale === 'de' ? 'en' : 'de', { persist: false });
		expect(localStorage.getItem(KEY)).toBeNull();
	});
});
