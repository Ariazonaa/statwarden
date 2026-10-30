<script lang="ts">
	// Relative timestamp ("3 min ago") with the absolute time in a tooltip.
	// Re-renders every 30s so "ago" stays fresh.
	import { onMount } from 'svelte';
	import { relativeTime, absoluteTime } from '$lib/util/time';

	let { value, class: className = '' }: { value?: string | number | Date | null; class?: string } =
		$props();

	let now = $state(Date.now());
	onMount(() => {
		const t = setInterval(() => (now = Date.now()), 30000);
		return () => clearInterval(t);
	});

	// `now` is referenced so the derived value recomputes on the interval.
	const rel = $derived.by(() => {
		void now;
		return relativeTime(value);
	});
	const abs = $derived(absoluteTime(value));
</script>

<time class={className} title={abs} datetime={value ? new Date(value).toISOString() : undefined}>
	{rel}
</time>

<style>
	time {
		cursor: default;
		white-space: nowrap;
	}
</style>
