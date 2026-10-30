/**
 * A deliberately tiny Markdown renderer for monitor descriptions and notes.
 *
 * WHY NOT A LIBRARY: a description is written by a writer-role user and
 * rendered to viewers, so it crosses a privilege boundary. A general Markdown
 * parser passes raw HTML through by design, which turns that boundary into a
 * stored-XSS vector and leaves us maintaining a sanitiser allowlist forever.
 * This renderer inverts the problem: EVERY input character is HTML-escaped
 * first, and the only tags in the output are the ones this file writes itself.
 * There is no path by which caller input becomes markup, so there is nothing to
 * sanitise afterwards.
 *
 * The supported subset is: bold, italic, inline code, fenced code blocks,
 * unordered lists, paragraphs with line breaks, and autolinked http/https URLs.
 * Notably absent is `[text](url)` link syntax — the only hrefs this emits come
 * from the URL token below, which cannot match a `javascript:` or `data:` URL,
 * so those schemes are unreachable rather than filtered.
 *
 * Anything unsupported (headings, tables, images, raw HTML) renders as the
 * literal text the author typed, which is the safe failure mode.
 */

const ESCAPES: Record<string, string> = {
	'&': '&amp;',
	'<': '&lt;',
	'>': '&gt;',
	'"': '&quot;',
	"'": '&#39;'
};

/** Escapes every character that can start markup or break out of an attribute. */
export function escapeHtml(text: string): string {
	return text.replace(/[&<>"']/g, (c) => ESCAPES[c]);
}

/**
 * Inline code spans and bare URLs, matched on the RAW text so neither is
 * subject to the emphasis rules: a `**` inside a code span stays literal, and an
 * `_` inside a URL does not become an `<em>`. Both alternatives are then handled
 * as whole tokens, which is why the pattern captures.
 *
 * The URL alternative stops at whitespace, `<` and quotes, so a crafted
 * `https://x" onmouseover=…` ends the token at the quote and the remainder is
 * escaped as ordinary text.
 */
const INLINE_TOKEN = /(`[^`\n]+`|https?:\/\/[^\s<>`"']+)/;

/**
 * The URL alternative of INLINE_TOKEN, anchored so it matches a WHOLE part.
 *
 * The classification below must ask the same question the split asked, not a
 * weaker one. Testing a part with a bare `/^https?:\/\//` prefix instead let a
 * scheme the token deliberately did NOT match — `https://` with nothing after
 * it, because the token requires at least one non-space character — fall into
 * the link branch anyway. A line beginning "https:// rest of the **line**" is
 * one unmatched part that still starts with the scheme, so the whole line
 * became the href AND the link text, and its emphasis was never rendered. The
 * output stayed escaped, so this was wrong rendering rather than an injection,
 * but the href was nonsense and the rest of the line silently lost formatting.
 */
const URL_TOKEN = /^https?:\/\/[^\s<>`"']+$/;

/** Trailing sentence punctuation that reads as prose, not as part of the URL. */
const URL_TAIL = /[.,;:!?)]+$/;

function renderInline(raw: string): string {
	return raw
		.split(INLINE_TOKEN)
		.map((part) => {
			if (part.length > 1 && part.startsWith('`') && part.endsWith('`')) {
				return `<code>${escapeHtml(part.slice(1, -1))}</code>`;
			}
			if (URL_TOKEN.test(part)) return renderLink(part);
			return renderEmphasis(escapeHtml(part));
		})
		.join('');
}

function renderLink(url: string): string {
	const tail = URL_TAIL.exec(url)?.[0] ?? '';
	const href = tail ? url.slice(0, -tail.length) : url;
	const safe = escapeHtml(href);
	return `<a href="${safe}" rel="noopener noreferrer nofollow" target="_blank">${safe}</a>${escapeHtml(tail)}`;
}

/**
 * Bold and italic, applied to already-escaped text. Every pattern requires a
 * closing marker, so an unbalanced `**` is simply left alone as the literal
 * asterisks the author typed. The word-boundary guards on the single-marker
 * forms keep `snake_case_names` and `2 * 3 * 4` out of it.
 */
function renderEmphasis(escaped: string): string {
	return escaped
		.replace(/\*\*(?=\S)([\s\S]*?\S)\*\*/g, '<strong>$1</strong>')
		.replace(/(?<![\w*])\*(?=\S)([^*\n]*?\S)\*(?![\w*])/g, '<em>$1</em>')
		.replace(/(?<![\w_])_(?=\S)([^_\n]*?\S)_(?![\w_])/g, '<em>$1</em>');
}

const FENCE = /^\s*```/;
const FENCE_CLOSE = /^\s*```\s*$/;
const BULLET = /^\s*[-*]\s+/;

/**
 * Renders the supported subset to an HTML string safe to pass to `{@html}`.
 *
 * An unterminated fence swallows the rest of the input as code rather than
 * leaving a half-open block; that is the same choice CommonMark makes and it
 * cannot produce unbalanced tags.
 */
export function renderMarkdown(source: string): string {
	const lines = source.replace(/\r\n?/g, '\n').split('\n');
	const out: string[] = [];
	let i = 0;
	while (i < lines.length) {
		const line = lines[i];

		if (FENCE.test(line)) {
			const body: string[] = [];
			i++; // the opening fence, plus any info string, is dropped
			while (i < lines.length && !FENCE_CLOSE.test(lines[i])) {
				body.push(lines[i]);
				i++;
			}
			i++; // past the closing fence, or past the end
			out.push(`<pre><code>${escapeHtml(body.join('\n'))}</code></pre>`);
			continue;
		}

		if (BULLET.test(line)) {
			const items: string[] = [];
			while (i < lines.length && BULLET.test(lines[i])) {
				items.push(`<li>${renderInline(lines[i].replace(BULLET, ''))}</li>`);
				i++;
			}
			out.push(`<ul>${items.join('')}</ul>`);
			continue;
		}

		if (line.trim() === '') {
			i++;
			continue;
		}

		const para: string[] = [];
		while (
			i < lines.length &&
			lines[i].trim() !== '' &&
			!FENCE.test(lines[i]) &&
			!BULLET.test(lines[i])
		) {
			para.push(renderInline(lines[i]));
			i++;
		}
		out.push(`<p>${para.join('<br />')}</p>`);
	}
	return out.join('');
}
