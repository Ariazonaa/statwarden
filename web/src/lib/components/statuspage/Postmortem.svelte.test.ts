/**
 * Postmortem is the XSS boundary of the public status page.
 *
 * Its source is Markdown written by a writer-role account and rendered to
 * anonymous visitors, and the page has never used `{@html}` anywhere. The parser
 * is tested separately (./markdown.test.ts) and returns DATA — but a parser that
 * returns safe data still renders unsafely if the component ever interpolates
 * it as HTML, and that is a one-line change somebody could make in good faith
 * while "adding link support".
 *
 * So these tests assert against the rendered DOM rather than the block list:
 * they fail if the component starts producing elements the source asked for.
 */
import { describe, it, expect } from 'vitest';
import { render } from '@testing-library/svelte';
import Postmortem from './Postmortem.svelte';

/** Renders a source string and hands back the wrapper. */
function pm(source: string): HTMLElement {
	const { container } = render(Postmortem, { props: { source } });
	return container.querySelector('.pm') as HTMLElement;
}

describe('Postmortem rendering', () => {
	it('renders the subset as real elements', () => {
		const el = pm('# Title\n\n## Section\n\nA **bold** word and `code`.\n\n- one\n- two');
		expect(el.querySelector('h3')?.textContent).toBe('Title');
		expect(el.querySelector('h4')?.textContent).toBe('Section');
		expect(el.querySelector('strong')?.textContent).toBe('bold');
		expect(el.querySelector('code')?.textContent).toBe('code');
		expect(el.querySelectorAll('li')).toHaveLength(2);
	});

	// Headings start at h3 because this renders inside an incident article that
	// already owns the h2. A jump back to h1 would read to a screen reader as a
	// new top-level section of the page.
	it('starts its heading levels below the host page', () => {
		const el = pm('# one\n\n## two\n\n### three');
		expect([...el.children].map((c) => c.tagName)).toEqual(['H3', 'H4', 'H5']);
	});

	it('does not insert whitespace around inline runs', () => {
		// `**bold**text` must not gain a space at the seam between the two runs.
		expect(pm('**bold**text and `code`.').textContent).toBe('boldtext and code.');
	});

	/**
	 * The one that matters. Every construct below is a way to get an element into
	 * the page; all of them must come out as literal text.
	 */
	it.each([
		['raw element', '<img src=x onerror=alert(1)>'],
		['script tag', '<script>alert(1)</script>'],
		['inside a heading', '## <b>heading</b>'],
		['inside a list item', '- <iframe src="//evil.test"></iframe>'],
		['inside bold', '**<img src=x onerror=alert(1)>**'],
		['inside code', '`<img src=x onerror=alert(1)>`'],
		['markdown link', '[click](javascript:alert(1))'],
		['markdown image', '![alt](x" onerror="alert(1))'],
		['html entity for a bracket', '&lt;img src=x onerror=alert(1)&gt;']
	])('renders %s as text, not markup', (_name, source) => {
		const el = pm(source);
		for (const tag of ['img', 'script', 'iframe', 'b', 'a', 'object', 'embed', 'svg']) {
			expect(el.querySelector(tag), `${tag} was created from ${source}`).toBeNull();
		}
		// Nothing carries an event handler or an href either — a payload that
		// survived as an attribute on an allowed element would pass the check above.
		for (const node of el.querySelectorAll('*')) {
			for (const attr of node.attributes) {
				expect(attr.name.toLowerCase().startsWith('on')).toBe(false);
				expect(['href', 'src', 'srcdoc', 'data']).not.toContain(attr.name.toLowerCase());
			}
		}
	});

	it('keeps a hostile string readable rather than swallowing it', () => {
		// Escaping must not mean discarding: the operator typed it, and a
		// retrospective that silently drops part of its own text is its own bug.
		expect(pm('<img src=x>').textContent).toContain('<img src=x>');
	});

	it('renders an empty source as nothing', () => {
		expect(pm('').children).toHaveLength(0);
		expect(pm('   \n\n  ').children).toHaveLength(0);
	});
});
