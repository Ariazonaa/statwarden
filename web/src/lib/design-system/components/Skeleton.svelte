<script lang="ts">
	// Loading placeholder with a glassy sheen sweep. Static under
	// prefers-reduced-motion.
	interface Props {
		variant?: 'text' | 'circle' | 'rect';
		/** CSS size overrides, e.g. width="12rem". */
		width?: string;
		height?: string;
		/** Number of stacked text lines (variant="text"). */
		lines?: number;
		class?: string;
	}

	let { variant = 'text', width, height, lines = 1, class: className = '' }: Props = $props();
</script>

{#if variant === 'text' && lines > 1}
	<span class="stack {className}" aria-hidden="true">
		{#each Array(lines) as _, i (i)}
			<span class="bone text" style:width={i === lines - 1 ? '60%' : (width ?? '100%')} style:height
			></span>
		{/each}
	</span>
{:else}
	<span class="bone {variant} {className}" style:width style:height aria-hidden="true"></span>
{/if}

<style>
	.stack {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
		width: 100%;
	}

	.bone {
		position: relative;
		display: inline-block;
		background: var(--glass-bg-thin);
		border: 1px solid var(--hairline);
		overflow: hidden;
	}
	.bone::after {
		content: '';
		position: absolute;
		inset: 0;
		transform: translateX(-100%);
		background: linear-gradient(100deg, transparent 30%, var(--glass-sheen) 50%, transparent 70%);
		animation: shimmer 1.8s var(--ease-in-out) infinite;
	}

	.text {
		width: 100%;
		height: 0.875em;
		border-radius: var(--radius-pill);
	}
	.circle {
		width: 2.5rem;
		height: 2.5rem;
		border-radius: 50%;
	}
	.rect {
		width: 100%;
		height: 5rem;
		border-radius: var(--radius-squircle-sm);
	}

	@keyframes shimmer {
		to {
			transform: translateX(100%);
		}
	}

	@media (prefers-reduced-motion: reduce) {
		.bone::after {
			animation: none;
		}
	}
</style>
