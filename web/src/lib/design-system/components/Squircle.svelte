<script lang="ts">
	// Clips its content to a mathematically exact Apple-style superellipse
	// (|x|^n + |y|^n = 1) via an SVG clip-path, rather than the CSS
	// border-radius approximation used elsewhere in the radius scale. For hero
	// surfaces where the corner curvature is large enough to read.
	//
	// The corner outline is sampled in the element's own pixel space
	// (userSpaceOnUse) off a live offset-box measurement, so the curvature is
	// exact at any size and does not stretch the way an objectBoundingBox clip
	// would on a non-square box. Where clip-path is unsupported it degrades to
	// the plain border-radius fallback (--squircle-radius).
	import type { Snippet } from 'svelte';
	import type { HTMLAttributes } from 'svelte/elements';

	interface Props extends HTMLAttributes<HTMLDivElement> {
		/** Corner radius in px for the superellipse corners. */
		radius?: number;
		/**
		 * Corner smoothing, 0..1. 0 is a true circular corner (n = 2), ~0.6 the
		 * iOS squircle look, 1 an almost-square continuous corner (n = 5).
		 */
		smoothing?: number;
		children?: Snippet;
	}

	let {
		radius = 40,
		smoothing = 0.6,
		children,
		class: className = '',
		style = '',
		...rest
	}: Props = $props();

	// SSR-safe, hydration-stable unique id for the clipPath reference.
	const clipId = $props.id();

	// Border-box measurement — matches clip-path's default reference box.
	let w = $state(0);
	let h = $state(0);

	// clip-path url() is widely supported, but we only *apply* it once the
	// browser confirms support; otherwise the border-radius fallback stands.
	// Set from an effect so SSR emits the fallback and hydration matches.
	let canClip = $state(false);
	$effect(() => {
		canClip =
			typeof CSS !== 'undefined' &&
			typeof CSS.supports === 'function' &&
			CSS.supports('clip-path', 'inset(0 round 1px)');
	});

	const clamp = (v: number, lo: number, hi: number) => Math.min(hi, Math.max(lo, v));
	const round = (v: number) => Math.round(v * 100) / 100;

	// Superellipse exponent: smoothing 0 → n = 2 (circle), 1 → n = 5 (near square).
	const exponent = $derived(2 + clamp(smoothing, 0, 1) * 3);

	const path = $derived(w > 0 && h > 0 ? squirclePath(w, h, radius, exponent) : '');
	const clip = $derived(canClip && path ? `url(#${clipId})` : undefined);

	/**
	 * Closed rounded-rectangle outline whose four corners each trace an exact
	 * quarter of the superellipse |x|^n + |y|^n = 1, sampled as a fine polyline.
	 */
	function squirclePath(width: number, height: number, r: number, n: number): string {
		const rad = clamp(r, 0, Math.min(width, height) / 2);
		if (rad <= 0) {
			return `M0 0H${round(width)}V${round(height)}H0Z`;
		}
		const steps = 24;
		const p = 2 / n;
		const seg: string[] = [`M${round(rad)} 0`];

		const corner = (cx: number, cy: number, map: (c: number, s: number) => [number, number]) => {
			for (let i = 0; i <= steps; i++) {
				const phi = (i / steps) * (Math.PI / 2);
				const c = Math.pow(Math.cos(phi), p);
				const s = Math.pow(Math.sin(phi), p);
				const [dx, dy] = map(c, s);
				seg.push(`L${round(cx + dx * rad)} ${round(cy + dy * rad)}`);
			}
		};

		seg.push(`L${round(width - rad)} 0`);
		corner(width - rad, rad, (c, s) => [s, -c]); // top-right
		seg.push(`L${round(width)} ${round(height - rad)}`);
		corner(width - rad, height - rad, (c, s) => [c, s]); // bottom-right
		seg.push(`L${round(rad)} ${round(height)}`);
		corner(rad, height - rad, (c, s) => [-s, c]); // bottom-left
		seg.push(`L0 ${round(rad)}`);
		corner(rad, rad, (c, s) => [-c, -s]); // top-left
		seg.push('Z');
		return seg.join('');
	}
</script>

<div
	class="squircle {className}"
	bind:offsetWidth={w}
	bind:offsetHeight={h}
	style="--squircle-radius: {radius}px; {clip
		? `clip-path: ${clip}; -webkit-clip-path: ${clip};`
		: ''} {style}"
	{...rest}
>
	{@render children?.()}
</div>
<svg class="squircle-defs" width="0" height="0" aria-hidden="true" focusable="false">
	<defs>
		<clipPath id={clipId} clipPathUnits="userSpaceOnUse">
			<path d={path} />
		</clipPath>
	</defs>
</svg>

<style>
	/* Fallback for browsers without clip-path support, and the shape before the
	   first measurement lands. The exact SVG clip supersedes it when applied. */
	.squircle {
		border-radius: var(--squircle-radius);
	}
	.squircle-defs {
		position: absolute;
		width: 0;
		height: 0;
		overflow: hidden;
		pointer-events: none;
	}
</style>
