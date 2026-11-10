<script lang="ts">
	// The Statwarden shield glyph. Only the drawing lives here: every call site
	// frames it in its own tile (header, login card, splash, about panel), so the
	// tile stays local and this stays one path to change.
	//
	// Decorative by default — every current use sits next to the product name or
	// inside an aria-hidden wrapper. Pass `label` where the mark stands alone.
	let {
		size = '1.5rem',
		label,
		class: className = ''
	}: { size?: string; label?: string; class?: string } = $props();
</script>

<svg
	class={className}
	viewBox="0 0 64 64"
	width={size}
	height={size}
	role={label ? 'img' : undefined}
	aria-label={label}
	aria-hidden={label ? undefined : 'true'}
	><path
		d="M32 7 50 14v15c0 12-7.7 21.6-18 26-10.3-4.4-18-14-18-26V14L32 7Zm-12 25h7l4-8 6 16 4-8h3"
		fill="none"
		stroke="currentColor"
		stroke-width="4.5"
		stroke-linecap="round"
		stroke-linejoin="round"
	/></svg
>

<style>
	svg {
		display: block;
		flex-shrink: 0;
	}
</style>
