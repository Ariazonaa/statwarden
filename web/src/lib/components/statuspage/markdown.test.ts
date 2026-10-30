import { describe, it, expect } from 'vitest';
import { parseBlocks, parseInline } from './markdown';

describe('parseInline', () => {
	it('splits bold and code out of a line', () => {
		expect(parseInline('a **b** c `d` e')).toEqual([
			{ kind: 'text', text: 'a ' },
			{ kind: 'bold', text: 'b' },
			{ kind: 'text', text: ' c ' },
			{ kind: 'code', text: 'd' },
			{ kind: 'text', text: ' e' }
		]);
	});

	it('leaves an unbalanced marker literal', () => {
		// An opener with no partner must not swallow the rest of the document.
		expect(parseInline('a ** b')).toEqual([{ kind: 'text', text: 'a ** b' }]);
		expect(parseInline('a ` b')).toEqual([{ kind: 'text', text: 'a ` b' }]);
	});

	it('always yields at least one run', () => {
		expect(parseInline('')).toEqual([{ kind: 'text', text: '' }]);
	});

	/**
	 * The whole reason this module returns data instead of a string of HTML: the
	 * source is operator-authored text rendered on a PUBLIC status page. Angle
	 * brackets must survive as literal characters so Svelte escapes them; nothing
	 * here may ever produce markup.
	 */
	it('treats HTML as literal text', () => {
		const runs = parseInline('<img src=x onerror=alert(1)>');
		expect(runs).toEqual([{ kind: 'text', text: '<img src=x onerror=alert(1)>' }]);
		expect(runs.every((r) => r.kind === 'text' || r.kind === 'bold' || r.kind === 'code')).toBe(
			true
		);
	});

	it('does not turn a link into anything clickable', () => {
		// Links are deliberately unsupported: a URL an operator pasted onto a
		// public page is a phishing surface, and it reads fine as text.
		expect(parseInline('[click](https://evil.test)')).toEqual([
			{ kind: 'text', text: '[click](https://evil.test)' }
		]);
	});
});

describe('parseBlocks', () => {
	it('reads headings at three levels', () => {
		const blocks = parseBlocks('# One\n## Two\n### Three');
		expect(blocks.map((b) => b.kind === 'heading' && b.level)).toEqual([1, 2, 3]);
	});

	it('joins wrapped lines into one paragraph and breaks on a blank line', () => {
		const blocks = parseBlocks('one\ntwo\n\nthree');
		expect(blocks).toHaveLength(2);
		expect(blocks[0]).toEqual({ kind: 'paragraph', content: [{ kind: 'text', text: 'one two' }] });
	});

	it('groups consecutive bullets into one list', () => {
		const blocks = parseBlocks('- a\n- b\n* c');
		expect(blocks).toHaveLength(1);
		expect(blocks[0].kind).toBe('list');
		expect(blocks[0].kind === 'list' && blocks[0].items).toHaveLength(3);
	});

	it('lets a list interrupt a paragraph without merging them', () => {
		const blocks = parseBlocks('intro\n- a\n- b');
		expect(blocks.map((b) => b.kind)).toEqual(['paragraph', 'list']);
	});

	it('flushes a trailing block with no closing blank line', () => {
		const blocks = parseBlocks('# Head\ntail');
		expect(blocks.map((b) => b.kind)).toEqual(['heading', 'paragraph']);
	});

	it('renders an empty document as no blocks', () => {
		expect(parseBlocks('')).toEqual([]);
		expect(parseBlocks('\n\n  \n')).toEqual([]);
	});

	it('handles CRLF the same as LF', () => {
		expect(parseBlocks('# a\r\n\r\nb')).toEqual(parseBlocks('# a\n\nb'));
	});

	it('leaves an unsupported construct as literal text', () => {
		// A block quote, a table row and a fenced block are all just paragraphs.
		// The failure mode of anything unsupported is "it shows up as you typed
		// it", never "it does something you did not intend".
		const blocks = parseBlocks('> quoted\n\n| a | b |\n\n```js');
		expect(blocks.every((b) => b.kind === 'paragraph')).toBe(true);
	});
});
