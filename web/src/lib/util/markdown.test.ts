/**
 * Tests for the restricted Markdown renderer.
 *
 * The hostile-input block is the reason this renderer exists instead of a
 * library: a monitor description is authored by a writer-role user and rendered
 * to viewers, so anything that survives into the output as markup is a stored
 * XSS across a privilege boundary. Each of those cases fails loudly if the
 * escape-first rule is ever relaxed — dropping escapeHtml from renderInline, or
 * adding `[text](url)` link syntax, breaks them individually.
 */
import { describe, expect, it } from 'vitest';
import { escapeHtml, renderMarkdown } from './markdown';

describe('escapeHtml', () => {
	it('escapes every character that can open a tag or break an attribute', () => {
		expect(escapeHtml(`<&>"'`)).toBe('&lt;&amp;&gt;&quot;&#39;');
	});
});

describe('renderMarkdown — supported subset', () => {
	it('wraps prose in a paragraph and joins soft line breaks', () => {
		expect(renderMarkdown('one\ntwo')).toBe('<p>one<br />two</p>');
	});

	it('separates paragraphs on a blank line', () => {
		expect(renderMarkdown('one\n\ntwo')).toBe('<p>one</p><p>two</p>');
	});

	it('renders bold and italic', () => {
		expect(renderMarkdown('**loud** and *soft* and _also soft_')).toBe(
			'<p><strong>loud</strong> and <em>soft</em> and <em>also soft</em></p>'
		);
	});

	it('leaves emphasis markers alone inside words', () => {
		// Otherwise every snake_case identifier in a runbook turns italic.
		expect(renderMarkdown('restart statwarden_worker_1 now')).toBe(
			'<p>restart statwarden_worker_1 now</p>'
		);
	});

	it('renders inline code without interpreting what is inside it', () => {
		expect(renderMarkdown('run `systemctl restart **x**`')).toBe(
			'<p>run <code>systemctl restart **x**</code></p>'
		);
	});

	it('renders a fenced block verbatim, dropping the info string', () => {
		expect(renderMarkdown('```sh\ncurl -s https://x\n```')).toBe(
			'<pre><code>curl -s https://x</code></pre>'
		);
	});

	it('renders an unordered list from either bullet marker', () => {
		expect(renderMarkdown('- first\n* second')).toBe('<ul><li>first</li><li>second</li></ul>');
	});

	it('autolinks http and https URLs and keeps trailing punctuation as prose', () => {
		expect(renderMarkdown('see https://runbook.example.com/a_b.')).toBe(
			'<p>see <a href="https://runbook.example.com/a_b" rel="noopener noreferrer nofollow" target="_blank">https://runbook.example.com/a_b</a>.</p>'
		);
	});

	it('leaves a bare scheme with no host as prose, and keeps formatting the line', () => {
		// A line STARTING with "https://" and nothing attached is not a URL token —
		// the token needs at least one non-space character after the slashes. It
		// used to be linkified anyway, because the branch tested only the prefix:
		// the whole rest of the line became both the href and the link text, and
		// its emphasis was never rendered. Both halves of that are asserted here.
		expect(renderMarkdown('https:// rest of the **line**')).toBe(
			'<p>https:// rest of the <strong>line</strong></p>'
		);
		expect(renderMarkdown('http:// a b c')).toBe('<p>http:// a b c</p>');
	});

	it('still autolinks a URL that opens the line', () => {
		expect(renderMarkdown('https://ok.example works')).toBe(
			'<p><a href="https://ok.example" rel="noopener noreferrer nofollow" target="_blank">https://ok.example</a> works</p>'
		);
	});
});

describe('renderMarkdown — hostile input', () => {
	it('escapes raw HTML tags instead of passing them through', () => {
		const html = renderMarkdown('<script>alert(1)</script>');
		expect(html).not.toContain('<script');
		expect(html).toBe('<p>&lt;script&gt;alert(1)&lt;/script&gt;</p>');
	});

	it('escapes an event-handler attribute so no element can carry it', () => {
		const html = renderMarkdown('<img src=x onerror=alert(1)>');
		expect(html).not.toContain('<img');
		expect(html).toBe('<p>&lt;img src=x onerror=alert(1)&gt;</p>');
	});

	it('never emits a javascript: URL, in link syntax or bare', () => {
		const html = renderMarkdown('[click](javascript:alert(1)) javascript:alert(2)');
		// There is no link syntax in the subset, so this is literal text — and the
		// autolinker only ever matches http/https.
		expect(html).not.toContain('href');
		expect(html).not.toContain('<a');
	});

	it('never emits a data: URL', () => {
		const html = renderMarkdown('data:text/html;base64,PHNjcmlwdD4=');
		expect(html).not.toContain('href');
	});

	it('cannot break out of a generated href attribute', () => {
		// The URL token stops at the quote, so the injected attribute is escaped
		// into the link text rather than becoming markup.
		const html = renderMarkdown('https://ok.example.com" onmouseover="alert(1)');
		expect(html).toContain('href="https://ok.example.com"');
		expect(html).not.toContain('onmouseover="alert(1)"');
		expect(html).toContain('&quot;');
	});

	it('leaves unbalanced markers literal', () => {
		expect(renderMarkdown('**not bold')).toBe('<p>**not bold</p>');
		expect(renderMarkdown('*not italic')).toBe('<p>*not italic</p>');
		expect(renderMarkdown('an `unclosed code span')).toBe('<p>an `unclosed code span</p>');
	});

	it('closes an unterminated fence rather than leaving an open tag', () => {
		expect(renderMarkdown('```\nrm -rf /')).toBe('<pre><code>rm -rf /</code></pre>');
	});

	it('escapes HTML inside code spans and fenced blocks too', () => {
		expect(renderMarkdown('`<b>x</b>`')).toBe('<p><code>&lt;b&gt;x&lt;/b&gt;</code></p>');
		expect(renderMarkdown('```\n<b>x</b>\n```')).toBe(
			'<pre><code>&lt;b&gt;x&lt;/b&gt;</code></pre>'
		);
	});

	it('escapes HTML inside list items', () => {
		expect(renderMarkdown('- <b>x</b>')).toBe('<ul><li>&lt;b&gt;x&lt;/b&gt;</li></ul>');
	});

	it('renders empty input as nothing', () => {
		expect(renderMarkdown('')).toBe('');
		expect(renderMarkdown('   \n\n  ')).toBe('');
	});
});
