/**
 * A deliberately tiny Markdown subset, parsed to a block list.
 *
 * It exists for exactly one document: the incident post-mortem, which is written
 * in Markdown by an operator and read on the PUBLIC status page. That page has
 * never used `{@html}` anywhere, and this must not be the thing that introduces
 * it — a retrospective is authored by a writer-role account and rendered to
 * anonymous visitors, so raw HTML there is stored XSS against everyone reading
 * the status page.
 *
 * So this returns DATA, not markup. The caller renders the blocks with ordinary
 * Svelte elements and Svelte escapes every string on the way out. There is no
 * code path in which operator text becomes HTML.
 *
 * The subset is the five things a retrospective actually uses:
 *
 *   # / ## / ###   headings
 *   - / *          bullet list items
 *   **bold**       inline emphasis
 *   `code`         inline code
 *   blank line     paragraph break
 *
 * Everything else — links, images, tables, raw HTML, block quotes — is left as
 * literal text. That is a feature: the failure mode of an unsupported construct
 * is "it shows up as you typed it", never "it does something you did not intend".
 * A link in particular is omitted on purpose; a clickable URL an operator pasted
 * onto a public page is a phishing surface, and the URL still reads fine as text.
 */

export type Inline =
	{ kind: 'text'; text: string } | { kind: 'bold'; text: string } | { kind: 'code'; text: string };

export type Block =
	| { kind: 'heading'; level: 1 | 2 | 3; content: Inline[] }
	| { kind: 'paragraph'; content: Inline[] }
	| { kind: 'list'; items: Inline[][] };

/**
 * Splits one line into inline runs.
 *
 * `**bold**` and `` `code` `` only. Both are matched non-greedily and a marker
 * with no partner stays literal, so an unbalanced `**` never swallows the rest of
 * the document.
 */
export function parseInline(line: string): Inline[] {
	const out: Inline[] = [];
	// One pass, one regex: alternation keeps the two markers from interleaving
	// wrongly (a `**` inside backticks stays literal, and vice versa).
	const re = /\*\*(.+?)\*\*|`([^`]+)`/g;
	let last = 0;
	let m: RegExpExecArray | null;
	while ((m = re.exec(line)) !== null) {
		if (m.index > last) out.push({ kind: 'text', text: line.slice(last, m.index) });
		if (m[1] !== undefined) out.push({ kind: 'bold', text: m[1] });
		else out.push({ kind: 'code', text: m[2] });
		last = m.index + m[0].length;
	}
	if (last < line.length) out.push({ kind: 'text', text: line.slice(last) });
	// An empty line still yields one empty run, so a caller never has to special
	// case a zero-length content array.
	return out.length > 0 ? out : [{ kind: 'text', text: '' }];
}

/** Parses a Markdown-subset document into renderable blocks. */
export function parseBlocks(src: string): Block[] {
	const blocks: Block[] = [];
	// Paragraph and list runs are accumulated and flushed on the first line that
	// does not continue them, which is what makes a blank line a separator rather
	// than a block of its own.
	let paragraph: string[] = [];
	let list: string[] = [];

	const flushParagraph = () => {
		if (paragraph.length === 0) return;
		blocks.push({ kind: 'paragraph', content: parseInline(paragraph.join(' ')) });
		paragraph = [];
	};
	const flushList = () => {
		if (list.length === 0) return;
		blocks.push({ kind: 'list', items: list.map(parseInline) });
		list = [];
	};
	const flush = () => {
		flushParagraph();
		flushList();
	};

	for (const raw of src.split(/\r?\n/)) {
		const line = raw.trim();
		if (line === '') {
			flush();
			continue;
		}
		const heading = /^(#{1,3})\s+(.*)$/.exec(line);
		if (heading) {
			flush();
			blocks.push({
				kind: 'heading',
				level: heading[1].length as 1 | 2 | 3,
				content: parseInline(heading[2])
			});
			continue;
		}
		const item = /^[-*]\s+(.*)$/.exec(line);
		if (item) {
			// A list interrupts a paragraph but continues another list.
			flushParagraph();
			list.push(item[1]);
			continue;
		}
		flushList();
		paragraph.push(line);
	}
	flush();
	return blocks;
}
