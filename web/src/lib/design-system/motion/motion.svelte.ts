/**
 * Shared motion presets — one spring vocabulary for every component,
 * with prefers-reduced-motion respected centrally.
 */
import { browser } from '$app/environment';
import { Spring } from 'svelte/motion';
import { cubicOut } from 'svelte/easing';
import { flip, type FlipParams, type AnimationConfig } from 'svelte/animate';
import { duration } from '../tokens/tokens.js';

export interface SpringParams {
	stiffness?: number;
	damping?: number;
	precision?: number;
}

/** svelte/motion spring parameter presets (Apple-ish feel). */
export const springPresets = {
	/** Default UI response — quick, tiny overshoot. */
	snappy: { stiffness: 0.25, damping: 0.7 } as SpringParams,
	/** Calm settle for large surfaces (sheets, modals). */
	gentle: { stiffness: 0.12, damping: 0.75 } as SpringParams,
	/** Playful bounce for small elements (toggles, badges, toasts). */
	bouncy: { stiffness: 0.3, damping: 0.5 } as SpringParams,
	/** Follows pointer/drag almost 1:1. */
	stiff: { stiffness: 0.4, damping: 0.9 } as SpringParams
} as const;
export type SpringPreset = keyof typeof springPresets;

let reduced = $state(
	browser ? window.matchMedia('(prefers-reduced-motion: reduce)').matches : false
);
if (browser) {
	window
		.matchMedia('(prefers-reduced-motion: reduce)')
		.addEventListener('change', (e) => (reduced = e.matches));
}

/**
 * Create a Spring with a shared preset. Under prefers-reduced-motion the
 * spring is made near-instant so values still land, just without motion.
 */
export function createSpring<T extends number | Record<string, number>>(
	value: T,
	preset: SpringPreset = 'snappy'
): Spring<T> {
	const opts = reduced ? { stiffness: 1, damping: 1 } : springPresets[preset];
	return new Spring(value, opts);
}

/** Motion-safe duration: collapses to 0 under prefers-reduced-motion. */
export function safeDuration(ms: number): number {
	return reduced ? 0 : ms;
}

/**
 * FLIP helper for list reordering (e.g. a monitor going DOWN and being
 * sorted to the top). Design-system easing/duration baked in, disabled
 * under prefers-reduced-motion.
 *
 * Usage: `{#each items as item (item.id)} <li animate:glassFlip> …`
 */
export function glassFlip(
	node: Element,
	fromTo: { from: DOMRect; to: DOMRect },
	params: FlipParams = {}
): AnimationConfig {
	return flip(node, fromTo, {
		duration: safeDuration(duration.lg),
		easing: cubicOut,
		...params,
		...(reduced ? { duration: 0 } : {})
	});
}
