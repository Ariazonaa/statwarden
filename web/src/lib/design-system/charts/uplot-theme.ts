/**
 * Shared uPlot theming for the Liquid Glass look: recessive hairline
 * grid, text-token axis labels, thin marks. Colours are read from the
 * live CSS custom properties so charts follow the active theme.
 */
import type uPlot from 'uplot';
import { cssVar } from '../tokens/tokens.js';

export interface ChartPalette {
	text: string;
	grid: string;
	accent: string;
	font: string;
}

/** Resolve the current chart palette from CSS custom properties. */
export function chartPalette(): ChartPalette {
	return {
		text: cssVar('--color-text-tertiary') || '#666e87',
		grid: cssVar('--hairline') || '#1f1f1f',
		accent: cssVar('--color-accent') || '#4f46e5',
		font: `11px ${cssVar('--font-sans') || 'sans-serif'}`
	};
}

/** Recessive axis config (hairline grid, muted labels, no axis stroke). */
export function glassAxis(p: ChartPalette, extra: Partial<uPlot.Axis> = {}): uPlot.Axis {
	return {
		stroke: p.text,
		font: p.font,
		size: 36,
		gap: 6,
		grid: { stroke: p.grid, width: 1 },
		ticks: { show: false },
		border: { show: false },
		...extra
	};
}

/** Vertical fade fill for area series (solid top → transparent baseline). */
export function areaGradient(u: uPlot, color: string, alphaTop = 0.28): CanvasGradient {
	const gradient = u.ctx.createLinearGradient(0, u.bbox.top, 0, u.bbox.top + u.bbox.height);
	const alphaHex = Math.round(alphaTop * 255)
		.toString(16)
		.padStart(2, '0');
	gradient.addColorStop(0, `${color}${alphaHex}`);
	gradient.addColorStop(1, `${color}00`);
	return gradient;
}
