/**
 * The post-mortem template.
 *
 * A retrospective is written once, days after the outage, by someone who has
 * already told the story three times in chat. The blank textarea is where that
 * write-up dies — so the editor offers a scaffold instead: the five headings a
 * reader actually needs, pre-filled with everything the incident already knows
 * (when it started, when it closed, how long it lasted, what the timeline said).
 *
 * It is plain Markdown and entirely editable. Nothing here is a required field:
 * the template is a starting point, not a form.
 *
 * The headings are localised through the caller's `t`, because the retrospective
 * is read by the status page's audience, not by the operator — a German instance
 * publishing English section headings would be the odd one out on its own page.
 */

import type { MessageKey } from '$lib/i18n';

export interface PostmortemTemplateInput {
	title: string;
	startedAt: string;
	resolvedAt?: string;
	updates: { status: string; body: string; createdAt: string }[];
	/** Display names of the monitors the incident was filed against. */
	monitorNames: string[];
}

/**
 * The translator, typed against the real key union so a caller can pass `t`
 * straight in. A loose `(key: string) => string` would compile and then let a
 * typo through as a rendered message key on a public page.
 */
export type TemplateT = (key: MessageKey, vars?: Record<string, string | number>) => string;

/**
 * Formats a duration through the catalogue: hours and minutes, minutes alone,
 * or `postmortem.durationUnderAMinute` for anything shorter than a minute (and
 * for a timestamp that will not parse, which is the same thing to a reader).
 *
 * Deliberately coarse. A retrospective that reports an outage as lasting
 * 7384 seconds is reporting a measurement, not a duration; the reader wants to
 * know whether it was minutes or an afternoon.
 *
 * The exact wording is the translator's, not this function's — every branch
 * returns a key.
 */
export function formatDuration(fromISO: string, toISO: string, t: TemplateT): string {
	const ms = new Date(toISO).getTime() - new Date(fromISO).getTime();
	if (!Number.isFinite(ms) || ms < 60_000) return t('postmortem.durationUnderAMinute');
	const totalMinutes = Math.round(ms / 60_000);
	const hours = Math.floor(totalMinutes / 60);
	const minutes = totalMinutes % 60;
	if (hours === 0) return t('postmortem.durationMinutes', { n: minutes });
	if (minutes === 0) return t('postmortem.durationHours', { n: hours });
	return t('postmortem.durationHoursMinutes', { h: hours, m: minutes });
}

/** An ISO timestamp as a readable UTC stamp: 2026-03-14 09:10 UTC. */
function stamp(iso: string): string {
	const d = new Date(iso);
	if (Number.isNaN(d.getTime())) return iso;
	const pad = (n: number) => String(n).padStart(2, '0');
	return (
		`${d.getUTCFullYear()}-${pad(d.getUTCMonth() + 1)}-${pad(d.getUTCDate())} ` +
		`${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())} UTC`
	);
}

/**
 * Builds the starting draft for an incident's retrospective.
 *
 * Timestamps are rendered in UTC, not the operator's local time. The document is
 * published to a public page read from every timezone, and a bare "09:10" that
 * silently meant Europe/Berlin is the kind of ambiguity a retrospective exists
 * to remove. The operator can rewrite them; what they cannot do is notice a
 * missing timezone after the fact.
 */
export function postmortemTemplate(inc: PostmortemTemplateInput, t: TemplateT): string {
	const lines: string[] = [];
	lines.push(`# ${inc.title}`, '');

	lines.push(`- **${t('postmortem.fieldStarted')}:** ${stamp(inc.startedAt)}`);
	if (inc.resolvedAt) {
		lines.push(`- **${t('postmortem.fieldResolved')}:** ${stamp(inc.resolvedAt)}`);
		lines.push(
			`- **${t('postmortem.fieldDuration')}:** ${formatDuration(inc.startedAt, inc.resolvedAt, t)}`
		);
	}
	if (inc.monitorNames.length > 0) {
		lines.push(`- **${t('postmortem.fieldAffected')}:** ${inc.monitorNames.join(', ')}`);
	}
	lines.push('');

	lines.push(`## ${t('postmortem.sectionSummary')}`, '', t('postmortem.hintSummary'), '');
	lines.push(`## ${t('postmortem.sectionImpact')}`, '', t('postmortem.hintImpact'), '');

	// The timeline is the one section that writes itself: the updates posted
	// during the incident already are the chronology, in the words used at the
	// time. Carrying them over is what stops the retrospective being reconstructed
	// from memory a week later.
	lines.push(`## ${t('postmortem.sectionTimeline')}`, '');
	if (inc.updates.length === 0) {
		lines.push(t('postmortem.hintTimeline'), '');
	} else {
		for (const u of inc.updates) {
			lines.push(`- **${stamp(u.createdAt)}** — ${u.body}`);
		}
		lines.push('');
	}

	lines.push(`## ${t('postmortem.sectionRootCause')}`, '', t('postmortem.hintRootCause'), '');
	lines.push(`## ${t('postmortem.sectionActions')}`, '', t('postmortem.hintActions'), '');

	return lines.join('\n');
}
