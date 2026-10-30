/**
 * Tests for the client-side reading of a maintenance recurrence.
 *
 * The cases that matter are the DST ones. `nextOccurrence` deliberately does
 * NOT do instant arithmetic (`+ 24h`) or naive UTC arithmetic, because both
 * drift by an hour around a transition — and an operator would then be shown a
 * next-occurrence that disagrees with the one the Go engine will actually use.
 * Each DST assertion below fails under the naive implementations: see the notes
 * on the individual tests for the value the wrong version produces.
 */
import { describe, expect, it } from 'vitest';
import {
	describeRecurrence,
	formatWeekdays,
	isValidRecurrence,
	nextOccurrence,
	parseClock
} from './recurrence';
import type { MaintenanceRecurrence } from '$lib/api/types';

const rec = (over: Partial<MaintenanceRecurrence> = {}): MaintenanceRecurrence => ({
	weekdays: [1],
	start: '02:00',
	durationMinutes: 120,
	...over
});

describe('parseClock', () => {
	it('accepts exactly HH:MM, like the server', () => {
		expect(parseClock('02:00')).toEqual({ hour: 2, minute: 0 });
		expect(parseClock('23:59')).toEqual({ hour: 23, minute: 59 });
	});

	it('rejects what the server rejects', () => {
		// Single-digit hours and trailing garbage are the exact variants the Go
		// side refuses by not using time.Parse.
		expect(parseClock('2:00')).toBeNull();
		expect(parseClock('02:00:00')).toBeNull();
		expect(parseClock('24:00')).toBeNull();
		expect(parseClock('02:60')).toBeNull();
		expect(parseClock('')).toBeNull();
	});
});

describe('isValidRecurrence', () => {
	it('requires at least one weekday', () => {
		expect(isValidRecurrence(rec({ weekdays: [] }))).toBe(false);
	});

	it('rejects duplicate and out-of-range weekdays', () => {
		expect(isValidRecurrence(rec({ weekdays: [1, 1] }))).toBe(false);
		expect(isValidRecurrence(rec({ weekdays: [7] }))).toBe(false);
		expect(isValidRecurrence(rec({ weekdays: [-1] }))).toBe(false);
	});

	it('enforces the same duration bounds as the server', () => {
		expect(isValidRecurrence(rec({ durationMinutes: 0 }))).toBe(false);
		expect(isValidRecurrence(rec({ durationMinutes: 7 * 24 * 60 }))).toBe(true);
		expect(isValidRecurrence(rec({ durationMinutes: 7 * 24 * 60 + 1 }))).toBe(false);
	});
});

describe('nextOccurrence', () => {
	it('returns null rather than a wrong date for an invalid recurrence', () => {
		expect(
			nextOccurrence(rec({ weekdays: [] }), 'UTC', new Date('2026-03-02T00:00:00Z'))
		).toBeNull();
		expect(
			nextOccurrence(rec({ start: '2:00' }), 'UTC', new Date('2026-03-02T00:00:00Z'))
		).toBeNull();
	});

	it("returns today's occurrence when it is still ahead", () => {
		// 2026-03-02 is a Monday; 00:00 UTC is before the 02:00 start.
		const next = nextOccurrence(rec(), 'UTC', new Date('2026-03-02T00:00:00Z'));
		expect(next?.toISOString()).toBe('2026-03-02T02:00:00.000Z');
	});

	it('skips to next week once the day passed', () => {
		const next = nextOccurrence(rec(), 'UTC', new Date('2026-03-02T02:00:00.001Z'));
		expect(next?.toISOString()).toBe('2026-03-09T02:00:00.000Z');
	});

	it('treats a start exactly at now as still due', () => {
		const next = nextOccurrence(rec(), 'UTC', new Date('2026-03-02T02:00:00Z'));
		expect(next?.toISOString()).toBe('2026-03-02T02:00:00.000Z');
	});

	it('picks the nearest of several weekdays', () => {
		// From Monday 03:00, with Mon/Wed/Fri, the next is Wednesday.
		const next = nextOccurrence(
			rec({ weekdays: [1, 3, 5] }),
			'UTC',
			new Date('2026-03-02T03:00:00Z')
		);
		expect(next?.toISOString()).toBe('2026-03-04T02:00:00.000Z');
	});

	it('anchors to local wall clock, not a fixed UTC offset', () => {
		// Berlin in winter is UTC+1, so 02:00 local is 01:00Z.
		const winter = nextOccurrence(rec(), 'Europe/Berlin', new Date('2026-01-05T00:00:00Z'));
		expect(winter?.toISOString()).toBe('2026-01-05T01:00:00.000Z');

		// In summer it is UTC+2, so the SAME 02:00 local is 00:00Z. An
		// implementation that cached one offset would report 01:00Z here.
		const summer = nextOccurrence(rec(), 'Europe/Berlin', new Date('2026-07-06T00:00:00Z'));
		expect(summer?.toISOString()).toBe('2026-07-06T00:00:00.000Z');
	});

	it('lands on the jump instant when the local start time does not exist', () => {
		// Berlin springs forward 2026-03-29 (a Sunday): 02:00–03:00 local never
		// happens. The occurrence begins as the clock jumps, 01:00Z = 03:00
		// local — matching the Go side's time.Date normalisation. A naive
		// "+24h from the previous occurrence" would give 02:00Z instead.
		const next = nextOccurrence(
			rec({ weekdays: [0] }),
			'Europe/Berlin',
			new Date('2026-03-29T00:00:00Z')
		);
		expect(next?.toISOString()).toBe('2026-03-29T01:00:00.000Z');
	});

	it('resolves an ambiguous autumn-back start to the earlier instant', () => {
		// Berlin falls back 2026-10-25 (a Sunday): 02:00 local happens twice,
		// at 00:00Z (CEST) and again at 01:00Z (CET). The earlier one is the
		// occurrence, which is also what the server's startOn() selects.
		const next = nextOccurrence(
			rec({ weekdays: [0] }),
			'Europe/Berlin',
			new Date('2026-10-24T12:00:00Z')
		);
		expect(next?.toISOString()).toBe('2026-10-25T00:00:00.000Z');
	});

	it('does not lose or repeat a local day across a transition', () => {
		// Every weekday selected: from Saturday midday Berlin, the next four
		// occurrences must be four consecutive local dates spanning the
		// spring-forward Sunday, each at 02:00 local (03:00 on the gap day).
		const all = rec({ weekdays: [0, 1, 2, 3, 4, 5, 6] });
		const seen: string[] = [];
		let cursor = new Date('2026-03-28T12:00:00Z');
		for (let i = 0; i < 4; i++) {
			const next = nextOccurrence(all, 'Europe/Berlin', cursor);
			expect(next).not.toBeNull();
			seen.push(
				new Intl.DateTimeFormat('en-CA', {
					timeZone: 'Europe/Berlin',
					year: 'numeric',
					month: '2-digit',
					day: '2-digit'
				}).format(next as Date)
			);
			cursor = new Date((next as Date).getTime() + 1);
		}
		expect(seen).toEqual(['2026-03-29', '2026-03-30', '2026-03-31', '2026-04-01']);
	});

	it('handles a zone with a sub-hour offset', () => {
		// Kolkata is UTC+05:30 year-round; a half-hour offset catches
		// arithmetic that assumes whole hours. 2026-03-01T19:00Z is Monday
		// 00:30 local, so Monday's 02:00 local is still ahead.
		const next = nextOccurrence(rec(), 'Asia/Kolkata', new Date('2026-03-01T19:00:00Z'));
		expect(next?.toISOString()).toBe('2026-03-01T20:30:00.000Z');
	});

	it('counts the local day, not the UTC day, when deciding what "today" is', () => {
		// 2026-03-02T00:00Z is already Monday 05:30 in Kolkata, so Monday's
		// occurrence has passed and the answer is the following Monday. A
		// version that read the weekday off the UTC date would wrongly offer
		// 2026-03-01T20:30Z, an instant in the past.
		const next = nextOccurrence(rec(), 'Asia/Kolkata', new Date('2026-03-02T00:00:00Z'));
		expect(next?.toISOString()).toBe('2026-03-08T20:30:00.000Z');
	});
});

describe('formatWeekdays', () => {
	it('renders in week order regardless of click order', () => {
		expect(formatWeekdays([5, 1, 3])).toBe(formatWeekdays([1, 3, 5]));
		expect(formatWeekdays([1, 3, 5])).toBe('Mon, Wed, Fri');
	});

	it('puts Sunday first, matching time.Weekday(0)', () => {
		expect(formatWeekdays([0, 6])).toBe('Sun, Sat');
	});
});

describe('describeRecurrence', () => {
	it('renders a line an operator can read without decoding JSON', () => {
		expect(describeRecurrence(rec({ weekdays: [1, 3, 5] }), 'Europe/Berlin')).toBe(
			'Mon, Wed, Fri · 02:00 for 2 h · Europe/Berlin'
		);
	});

	it('renders nothing for a recurrence the server would reject', () => {
		expect(describeRecurrence(rec({ weekdays: [] }), 'UTC')).toBe('');
	});
});
