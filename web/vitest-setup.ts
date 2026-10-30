// Adds the jest-dom matchers (toBeInTheDocument, toHaveAttribute, …) to expect.
// @testing-library/svelte registers its own afterEach cleanup as long as the
// globals are present, which `test.globals` in vite.config.ts guarantees.
import '@testing-library/jest-dom/vitest';

// jsdom ships no matchMedia, and the theme store and the motion presets both
// read one at module scope — importing $lib/design-system would throw without
// this. Reports "no preference" for every query, which is the default the
// components are designed around.
if (!window.matchMedia) {
	window.matchMedia = (query: string): MediaQueryList =>
		({
			matches: false,
			media: query,
			onchange: null,
			addEventListener: () => {},
			removeEventListener: () => {},
			addListener: () => {},
			removeListener: () => {},
			dispatchEvent: () => false
		}) as unknown as MediaQueryList;
}

// jsdom implements neither the Web Animations API nor getAnimations(), which
// Svelte's `animate:` machinery calls on every keyed reorder. Returning no
// animations is the honest answer here: the tests assert order and
// announcements, never the tween.
if (!Element.prototype.getAnimations) {
	Element.prototype.getAnimations = () => [];
}
if (!Element.prototype.animate) {
	Element.prototype.animate = () =>
		({
			cancel: () => {},
			finish: () => {},
			addEventListener: () => {},
			removeEventListener: () => {},
			startTime: 0,
			currentTime: 0
		}) as unknown as Animation;
}
