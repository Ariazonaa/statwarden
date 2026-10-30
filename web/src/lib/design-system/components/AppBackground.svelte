<script lang="ts">
	// Flat pure-black backdrop for the AMOLED look. The old Liquid Glass
	// version rendered slow-drifting blurred colour fields behind the glass
	// surfaces; with no glass and no translucency the backdrop is a single
	// #000 field, so this is now just a fixed black plane. It is kept as a
	// component (mounted once per app shell) so the app has one explicit
	// source for the page background and so future tweaks stay localised.
	// The `animated` prop is accepted for API compatibility but is a no-op:
	// there is nothing left to animate.
	let { animated = true }: { animated?: boolean } = $props();
</script>

<div class="backdrop" aria-hidden="true"></div>

<style>
	.backdrop {
		position: fixed;
		inset: 0;
		z-index: -1;
		background: var(--color-bg-base);
	}
</style>
