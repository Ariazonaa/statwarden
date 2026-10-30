import { describe, expect, it } from 'vitest';
import { meanUptime, mergeBeats, worstStatus } from './aggregate';
import type { MonitorStatus } from '$lib/api';

describe('worstStatus', () => {
	it('lets the most severe state win', () => {
		expect(worstStatus([{ status: 'up' }, { status: 'down' }, { status: 'pending' }])).toBe('down');
		expect(worstStatus([{ status: 'up' }, { status: 'maintenance' }])).toBe('maintenance');
	});
	it('ignores paused monitors unless every one is paused', () => {
		expect(worstStatus([{ status: 'paused' }, { status: 'up' }])).toBe('up');
		expect(worstStatus([{ status: 'paused' }])).toBe('paused');
		expect(worstStatus([])).toBe('paused');
	});
});

describe('meanUptime', () => {
	it('averages only the monitors that have a figure', () => {
		expect(meanUptime([{ uptime24h: 100 }, { uptime24h: 90 }, { uptime24h: undefined }])).toBe(95);
	});
	it('is undefined, not 0, when nothing has a figure', () => {
		expect(meanUptime([{ uptime24h: undefined }])).toBeUndefined();
	});
});

describe('mergeBeats', () => {
	const b = (s: string): MonitorStatus[] =>
		s
			.split('')
			.map((c) => ({ u: 'up', d: 'down', p: 'pending', m: 'maintenance' })[c] as MonitorStatus);

	it('aligns histories on the newest beat and keeps the worst per position', () => {
		// a: u u d   b:   p u   →   u p d  (newest-aligned)
		expect(mergeBeats([{ recentBeats: b('uud') }, { recentBeats: b('pu') }])).toEqual(b('upd'));
	});
	it('caps at the slot count, keeping the newest', () => {
		expect(mergeBeats([{ recentBeats: b('dduu') }], 2)).toEqual(b('uu'));
	});
	it('is empty for a folder without history', () => {
		expect(mergeBeats([{ recentBeats: [] }])).toEqual([]);
		expect(mergeBeats([])).toEqual([]);
	});
});
