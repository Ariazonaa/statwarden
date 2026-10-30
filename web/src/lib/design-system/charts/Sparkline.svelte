<script lang="ts">
	// Tiny inline trend line (uPlot, no axes/legend) — e.g. response time
	// in a monitor row. Colour follows an entity: accent or a status.
	import uPlot from 'uplot';
	import { theme } from '../theme/theme.svelte.js';
	import { cssVar, type MonitorStatus } from '../tokens/tokens.js';
	import { areaGradient } from './uplot-theme.js';

	interface Props {
		data: number[];
		color?: 'accent' | MonitorStatus;
		height?: number;
		/** Accessible summary, e.g. "Response time trend, last hour". */
		label: string;
		class?: string;
	}

	let { data, color = 'accent', height = 36, label, class: className = '' }: Props = $props();

	let host: HTMLDivElement | undefined = $state();
	let width = $state(0);

	function strokeColor(): string {
		return color === 'accent' ? cssVar('--color-accent') : cssVar(`--status-${color}`);
	}

	$effect(() => {
		// Re-create on data/size/theme/colour changes.
		void data;
		void color;
		void theme.resolved;
		if (!host || width === 0) return;

		const stroke = strokeColor();
		const chart = new uPlot(
			{
				width,
				height,
				legend: { show: false },
				cursor: { show: false },
				select: { show: false, left: 0, top: 0, width: 0, height: 0 },
				scales: { x: { time: false } },
				axes: [{ show: false }, { show: false }],
				series: [
					{},
					{
						stroke,
						width: 2,
						fill: (u) => areaGradient(u, stroke, 0.22),
						points: { show: false }
					}
				]
			},
			[data.map((_, i) => i), data],
			host
		);
		return () => chart.destroy();
	});
</script>

<div
	class="spark {className}"
	bind:this={host}
	bind:clientWidth={width}
	style:height="{height}px"
	role="img"
	aria-label={label}
></div>

<style>
	.spark {
		width: 100%;
		min-width: 3rem;
	}
	.spark :global(.u-over),
	.spark :global(.u-under) {
		border-radius: 0;
	}
</style>
