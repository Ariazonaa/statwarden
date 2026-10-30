<script lang="ts">
	// Line/area history chart (uPlot) with hairline grid, crosshair and a
	// glass tooltip. Single series — response-time / latency history.
	import uPlot from 'uplot';
	import 'uplot/dist/uPlot.min.css';
	import { theme } from '../theme/theme.svelte.js';
	import { areaGradient, chartPalette, glassAxis } from './uplot-theme.js';

	interface Props {
		/** X values — unix seconds when `timeAxis`, else arbitrary numbers. */
		x: number[];
		y: (number | null)[];
		/** Series name, shown in the tooltip. */
		label: string;
		timeAxis?: boolean;
		area?: boolean;
		height?: number;
		formatValue?: (v: number) => string;
		formatX?: (v: number) => string;
		class?: string;
	}

	let {
		x,
		y,
		label,
		timeAxis = true,
		area = true,
		height = 220,
		formatValue = (v) => `${Math.round(v)} ms`,
		formatX,
		class: className = ''
	}: Props = $props();

	let host: HTMLDivElement | undefined = $state();
	let width = $state(0);
	let tip = $state<{ x: number; y: number; xText: string; yText: string } | null>(null);

	const defaultFormatX = (v: number) =>
		timeAxis
			? new Date(v * 1000).toLocaleString(undefined, {
					month: 'short',
					day: 'numeric',
					hour: '2-digit',
					minute: '2-digit'
				})
			: String(v);

	$effect(() => {
		void x;
		void y;
		void area;
		void theme.resolved;
		if (!host || width === 0) return;

		const p = chartPalette();
		const chart = new uPlot(
			{
				width,
				height,
				legend: { show: false },
				scales: { x: { time: timeAxis } },
				cursor: {
					y: false,
					points: { size: 8, width: 2, stroke: p.accent, fill: '#ffffff' }
				},
				axes: [
					glassAxis(p, { grid: { show: false } }),
					glassAxis(p, { size: 48, values: (_u, vals) => vals.map((v) => formatValue(v)) })
				],
				series: [
					{},
					{
						label,
						stroke: p.accent,
						width: 2,
						fill: area ? (u) => areaGradient(u, p.accent) : undefined,
						points: { show: false }
					}
				],
				hooks: {
					setCursor: [
						(u) => {
							const { idx, left, top } = u.cursor;
							if (idx == null || left == null || left < 0) {
								tip = null;
								return;
							}
							const xv = u.data[0][idx];
							const yv = u.data[1][idx];
							if (yv == null) {
								tip = null;
								return;
							}
							tip = {
								x: left,
								y: top ?? 0,
								xText: (formatX ?? defaultFormatX)(xv),
								yText: formatValue(yv)
							};
						}
					]
				}
			},
			[x, y],
			host
		);
		return () => {
			chart.destroy();
			tip = null;
		};
	});
</script>

<div class="wrap {className}" role="img" aria-label={label}>
	<div bind:this={host} bind:clientWidth={width} style:height="{height}px"></div>
	{#if tip}
		<div
			class="tip ds-glass ds-glass--thick"
			style:left="{tip.x}px"
			style:top="{Math.max(tip.y - 14, 0)}px"
			aria-hidden="true"
		>
			<span class="tip-value">{tip.yText}</span>
			<span class="tip-time">{tip.xText}</span>
		</div>
	{/if}
</div>

<style>
	.wrap {
		position: relative;
		width: 100%;
	}

	.tip {
		position: absolute;
		translate: -50% -100%;
		display: flex;
		flex-direction: column;
		align-items: center;
		gap: 1px;
		padding: var(--space-2) var(--space-4);
		border-radius: var(--radius-squircle-sm);
		box-shadow: var(--elevation-glass-2);
		pointer-events: none;
		white-space: nowrap;
		z-index: 5;
	}
	.tip-value {
		font-size: var(--text-footnote);
		font-weight: var(--weight-semibold);
		color: var(--color-text-primary);
		font-variant-numeric: tabular-nums;
	}
	.tip-time {
		font-size: var(--text-caption-2);
		color: var(--color-text-secondary);
	}

	/* Recessive cursor line */
	.wrap :global(.u-cursor-x) {
		border-right: 1px dashed var(--hairline-strong);
	}
</style>
