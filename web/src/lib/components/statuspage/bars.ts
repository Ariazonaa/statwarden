// Pure mapping from a status-page uptime bucket to what the heatmap should show.
// In a plain module, like layout.ts, so it can be tested without rendering.
import type { PublicDayBar } from '$lib/api';
import type { MessageKey } from '$lib/i18n';
import type { MonitorStatus } from '$lib/design-system/tokens/tokens';

/** The bucket vocabulary the server sends (see buildBars in the Go API). */
export type BucketStatus = PublicDayBar['status'];

/**
 * The palette entry a bucket is tinted with.
 *
 * The server's vocabulary is wider than the five-value monitor-status palette, so
 * two of its values have to borrow a colour: a partially-degraded bucket takes the
 * pending tint, and a bucket with no data takes the paused one. That is a
 * deliberate collapse and it is only about COLOUR — see bucketLabelKey for why the
 * words cannot be collapsed the same way.
 */
export function bucketTone(s: BucketStatus): MonitorStatus {
	if (s === 'up') return 'up';
	if (s === 'down') return 'down';
	if (s === 'degraded') return 'pending';
	return 'paused';
}

/**
 * The message key naming a bucket, or null when the palette's own name is
 * accurate.
 *
 * The tooltip and the aria-label are the whole reason bucket status is not
 * conveyed by colour alone, so they must not inherit the colour collapse:
 * `bucketTone` maps a degraded bucket to `pending`, which in this product means
 * "retrying", and a bucket with no data to `paused`, which claims the monitor was
 * paused when it never was. A screen reader was being read both of those as fact,
 * and a no-data bucket rendered "paused · no data", contradicting itself.
 *
 * up and down return null: there the palette name is exactly right, and an
 * override would be a second place to keep the same word.
 */
export function bucketLabelKey(s: BucketStatus): MessageKey | null {
	if (s === 'up' || s === 'down') return null;
	if (s === 'degraded') return 'public.barDegraded';
	return 'public.barNoData';
}
