import { describe, it, expect } from 'vitest';
import { renderMarkdown } from './markdown';

// Independent hostile-input probe. The rendered output reaches {@html} on the
// monitor detail page, and descriptions are authored by writer-role users but
// shown to viewers — so a bypass here is stored XSS across a privilege
// boundary, not a cosmetic escaping bug.
describe('renderMarkdown refuses to emit active content', () => {
	const hostile = [
		'<script>alert(1)</script>',
		'<img src=x onerror="alert(1)">',
		'<svg/onload=alert(1)>',
		'<a href="javascript:alert(1)">x</a>',
		'[click](javascript:alert(1))',
		'[click](data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==)',
		'<iframe src="https://evil.example"></iframe>',
		'<style>body{display:none}</style>',
		'javascript:alert(1)',
		'JaVaScRiPt:alert(1)',
		'<div onmouseover="alert(1)">hover</div>',
		'`<script>alert(1)</script>`',
		'**<script>alert(1)</script>**',
		'- <script>alert(1)</script>',
		'https://ok.example" onmouseover="alert(1)',
		'<!--<script>alert(1)</script>-->'
	];

	// Only the tags the renderer itself emits may appear. Anything the input
	// contributed is escaped, so it is text and cannot be a tag at all.
	const ALLOWED = new Set(['p', 'br', 'strong', 'em', 'code', 'pre', 'ul', 'li', 'a']);

	/** Every real HTML tag in the output, as [name, rawAttributes]. */
	function tags(html: string): Array<[string, string]> {
		return [...html.matchAll(/<\/?([a-z0-9-]+)((?:"[^"]*"|'[^']*'|[^>])*)>/gi)].map((m) => [
			m[1].toLowerCase(),
			m[2] ?? ''
		]);
	}

	for (const input of hostile) {
		it(`neutralises ${JSON.stringify(input).slice(0, 46)}`, () => {
			const out = renderMarkdown(input);

			for (const [name, attrs] of tags(out)) {
				// Nothing the attacker wrote may become an element.
				expect(ALLOWED).toContain(name);
				// No inline event handler on a tag the renderer emitted.
				expect(attrs.toLowerCase()).not.toMatch(/\bon\w+\s*=/);
				// No executable scheme in an href the renderer emitted.
				expect(attrs.toLowerCase()).not.toMatch(/href\s*=\s*["']?\s*(javascript|data|vbscript):/);
			}
		});
	}

	it('still renders the safe subset it promises', () => {
		expect(renderMarkdown('**bold**')).toMatch(/<strong>bold<\/strong>/);
		expect(renderMarkdown('`code`')).toMatch(/<code>code<\/code>/);
		expect(renderMarkdown('https://example.com')).toMatch(/<a [^>]*href="https:\/\/example\.com"/);
	});

	it('escapes idempotently, so a re-render cannot decode into markup', () => {
		const once = renderMarkdown('<script>alert(1)</script>');
		const twice = renderMarkdown(once);
		expect(twice.toLowerCase()).not.toMatch(/<script/);
	});
});
