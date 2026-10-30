/**
 * Statwarden Design Tokens — typed mirrors of tokens.css.
 * Use these when a value is needed in JS/TS (charts, motion, measurements).
 * Keep in sync with ./tokens.css.
 */

/** Duration scale in ms (CSS: --duration-*). */
export const duration = {
	xs: 120,
	sm: 180,
	md: 280,
	lg: 420,
	xl: 640
} as const;

/** Responsive breakpoints in px. CSS media queries must inline these values. */
export const breakpoints = {
	/** Phones ≥ this are "large phones"; below is the compact layout. */
	sm: 640,
	/** Tablets portrait — sheet → dialog switch happens below this. */
	md: 768,
	/** Tablets landscape / small laptops. */
	lg: 1024,
	/** Desktop. */
	xl: 1280
} as const;

export const monitorStatuses = ['up', 'down', 'pending', 'maintenance', 'paused'] as const;
export type MonitorStatus = (typeof monitorStatuses)[number];

/** Read the current computed value of a CSS custom property. */
export function cssVar(name: string, el: Element = document.documentElement): string {
	return getComputedStyle(el).getPropertyValue(name).trim();
}
