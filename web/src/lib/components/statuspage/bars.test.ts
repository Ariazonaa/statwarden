import { describe, expect, it } from 'vitest';
import { bucketLabelKey, bucketTone, type BucketStatus } from './bars';
import { en } from '$lib/i18n/locales/en';
import { de } from '$lib/i18n/locales/de';

// Every value buildBars in the Go API can put on a bucket.
const ALL: BucketStatus[] = ['up', 'down', 'degraded', 'none'];

describe('bucketTone', () => {
	it('collapses the server vocabulary into the five-value palette', () => {
		expect(bucketTone('up')).toBe('up');
		expect(bucketTone('down')).toBe('down');
		expect(bucketTone('degraded')).toBe('pending');
		expect(bucketTone('none')).toBe('paused');
	});
});

describe('bucketLabelKey', () => {
	// The defect: the label used to be derived from the TONE, so a degraded bucket
	// announced "pending" (which means retrying here) and a bucket with no data
	// announced "paused" — of a monitor that was never paused. Both went into the
	// tooltip and the aria-label, so a screen reader was read them as fact.
	it('does not inherit the colour collapse', () => {
		expect(bucketLabelKey('degraded')).not.toBe('status.pending');
		expect(bucketLabelKey('none')).not.toBe('status.paused');
		expect(bucketLabelKey('degraded')).toBe('public.barDegraded');
		expect(bucketLabelKey('none')).toBe('public.barNoData');
	});

	it('leaves up and down to the palette, which names them correctly', () => {
		expect(bucketLabelKey('up')).toBeNull();
		expect(bucketLabelKey('down')).toBeNull();
	});

	// A key that resolves in neither catalogue renders as the raw key — visible
	// text like "public.barNoData" on the public status page, which is the most
	// exposed surface in the product.
	it('returns keys that exist in both catalogues', () => {
		for (const s of ALL) {
			const key = bucketLabelKey(s);
			if (key === null) continue;
			expect(en[key], `en is missing ${key}`).toBeTruthy();
			expect(de[key], `de is missing ${key}`).toBeTruthy();
		}
	});

	// Whatever the server adds next must not silently land on "paused".
	it('names every bucket status the server can send', () => {
		for (const s of ALL) {
			expect(bucketTone(s), `no tone for ${s}`).toBeTruthy();
		}
	});
});
