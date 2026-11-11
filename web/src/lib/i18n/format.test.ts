/**
 * Uptime is truncated, not rounded: a window with any downtime in it must never
 * read as a perfect 100 %.
 */
import { beforeEach, describe, expect, it } from 'vitest';
import { formatUptime } from './format';
import { i18n } from './store.svelte';

beforeEach(() => {
	i18n.setLocale('en', { persist: false });
});

describe('formatUptime', () => {
	it('never rounds a window with downtime up to 100 %', () => {
		expect(formatUptime(99.996)).toBe('99.99 %');
		expect(formatUptime(99.9999)).toBe('99.99 %');
		expect(formatUptime(100)).toBe('100 %');
	});

	it('truncates rather than rounds in the middle of the range too', () => {
		expect(formatUptime(98.759)).toBe('98.75 %');
		expect(formatUptime(50)).toBe('50 %');
	});

	it('is not pushed a step down by floating-point noise', () => {
		// 0.29 * 100 is 28.999999999999996 in binary floating point.
		expect(formatUptime(0.29 * 100)).toBe('29 %');
		expect(formatUptime(0.9999 * 100)).toBe('99.99 %');
	});

	it('renders no data as the placeholder, never as 0', () => {
		expect(formatUptime(undefined)).toBe('—');
		expect(formatUptime(undefined, '–')).toBe('–');
		expect(formatUptime(0)).toBe('0 %');
	});

	it('follows the locale', () => {
		i18n.setLocale('de', { persist: false });
		expect(formatUptime(99.5)).toBe('99,5 %');
	});
});
