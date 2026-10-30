/**
 * The post-mortem template (./postmortem.ts).
 *
 * Named `template.test.ts` rather than `postmortem.test.ts` on purpose: this
 * directory holds both `postmortem.ts` (the template module) and
 * `Postmortem.svelte` (the renderer), and their test files would differ only in
 * the case of the first letter — which on a case-insensitive filesystem is the
 * same file. The renderer's tests live in Postmortem.svelte.test.ts.
 */
import { describe, it, expect } from 'vitest';
import { formatDuration, postmortemTemplate } from './postmortem';
import type { TemplateT } from './postmortem';

/**
 * A translator that echoes the key and interpolates params, so the assertions
 * read against stable strings rather than English copy that a wording change
 * would break. It is typed as TemplateT, which is keyed on MessageKey, so a key
 * this module invents but the catalogue does not carry fails to compile.
 */
const t: TemplateT = (key, vars) =>
	vars
		? `${key}(${Object.entries(vars)
				.map(([k, v]) => `${k}=${v}`)
				.join(',')})`
		: key;

const H = 3600_000;

describe('formatDuration', () => {
	it('reports anything under a minute as such', () => {
		expect(formatDuration('2026-03-14T09:00:00Z', '2026-03-14T09:00:30Z', t)).toBe(
			'postmortem.durationUnderAMinute'
		);
	});

	it('reports whole hours without a minutes part', () => {
		expect(formatDuration('2026-03-14T09:00:00Z', '2026-03-14T11:00:00Z', t)).toBe(
			'postmortem.durationHours(n=2)'
		);
	});

	it('reports minutes alone under an hour', () => {
		expect(formatDuration('2026-03-14T09:00:00Z', '2026-03-14T09:45:00Z', t)).toBe(
			'postmortem.durationMinutes(n=45)'
		);
	});

	it('combines hours and minutes', () => {
		expect(formatDuration('2026-03-14T09:00:00Z', '2026-03-14T11:15:00Z', t)).toBe(
			'postmortem.durationHoursMinutes(h=2,m=15)'
		);
	});

	it('does not throw on an unparseable timestamp', () => {
		expect(formatDuration('not a date', '2026-03-14T11:15:00Z', t)).toBe(
			'postmortem.durationUnderAMinute'
		);
	});
});

describe('postmortemTemplate', () => {
	const base = {
		title: 'Checkout unavailable',
		startedAt: '2026-03-14T09:10:00Z',
		resolvedAt: new Date(Date.parse('2026-03-14T09:10:00Z') + 2 * H).toISOString(),
		updates: [
			{ status: 'investigating', body: 'Looking into it.', createdAt: '2026-03-14T09:12:00Z' },
			{ status: 'resolved', body: 'Rolled back.', createdAt: '2026-03-14T11:05:00Z' }
		],
		monitorNames: ['checkout-api', 'payments']
	};

	it('opens with the incident title as a heading', () => {
		expect(postmortemTemplate(base, t).split('\n')[0]).toBe('# Checkout unavailable');
	});

	it('carries the timeline over verbatim', () => {
		// The updates posted DURING the incident are the chronology, in the words
		// used at the time. Losing them means reconstructing it from memory later.
		const out = postmortemTemplate(base, t);
		expect(out).toContain('Looking into it.');
		expect(out).toContain('Rolled back.');
	});

	/**
	 * Timestamps are UTC and say so. The document is published to a page read
	 * from every timezone, and a bare "09:10" that silently meant Europe/Berlin is
	 * the kind of ambiguity a retrospective exists to remove.
	 */
	it('stamps every time in UTC', () => {
		const out = postmortemTemplate(base, t);
		expect(out).toContain('2026-03-14 09:10 UTC');
		for (const line of out.split('\n')) {
			if (/\d{4}-\d{2}-\d{2} \d{2}:\d{2}/.test(line)) expect(line).toContain('UTC');
		}
	});

	it('names the affected monitors', () => {
		expect(postmortemTemplate(base, t)).toContain('checkout-api, payments');
	});

	it('omits the resolved and duration lines while the incident is open', () => {
		const out = postmortemTemplate({ ...base, resolvedAt: undefined }, t);
		expect(out).not.toContain('postmortem.fieldResolved');
		expect(out).not.toContain('postmortem.fieldDuration');
		// The scaffold is still offered: the notes get written while it is fresh.
		expect(out).toContain('postmortem.sectionSummary');
	});

	it('falls back to a hint when there is no timeline yet', () => {
		expect(postmortemTemplate({ ...base, updates: [] }, t)).toContain('postmortem.hintTimeline');
	});

	it('emits every section a reader needs', () => {
		const out = postmortemTemplate(base, t);
		for (const section of [
			'postmortem.sectionSummary',
			'postmortem.sectionImpact',
			'postmortem.sectionTimeline',
			'postmortem.sectionRootCause',
			'postmortem.sectionActions'
		]) {
			expect(out).toContain(`## ${section}`);
		}
	});
});
