<script lang="ts">
	// User avatar: image or initials on a deterministic gradient derived
	// from the name, with optional status dot.
	import type { MonitorStatus } from '../tokens/tokens.js';

	interface Props {
		name: string;
		src?: string;
		size?: 'sm' | 'md' | 'lg';
		status?: MonitorStatus;
		class?: string;
	}

	let { name, src, size = 'md', status, class: className = '' }: Props = $props();

	const initials = $derived(
		name
			.split(/\s+/)
			.filter(Boolean)
			.slice(0, 2)
			.map((p) => p[0]!.toUpperCase())
			.join('') || '?'
	);

	// Deterministic hue pair from the name.
	const hue = $derived([...name].reduce((acc, ch) => (acc * 31 + ch.charCodeAt(0)) % 360, 7));
</script>

<span class="avatar {size} {className}" role="img" aria-label={name}>
	{#if src}
		<img {src} alt="" loading="lazy" />
	{:else}
		<span
			class="initials"
			style="background: linear-gradient(135deg, hsl({hue} 72% 58%), hsl({(hue + 50) %
				360} 68% 46%))"
			aria-hidden="true"
		>
			{initials}
		</span>
	{/if}
	{#if status}
		<span class="dot" data-status={status} aria-hidden="true"></span>
	{/if}
</span>

<style>
	.avatar {
		position: relative;
		display: inline-flex;
		flex-shrink: 0;
		border-radius: 50%;
		box-shadow:
			inset 0 1px 0 rgba(255, 255, 255, 0.4),
			0 2px 6px rgba(var(--glass-shadow-color), 0.2);
	}
	.sm {
		width: 1.75rem;
		height: 1.75rem;
		font-size: var(--text-caption-2);
	}
	.md {
		width: 2.5rem;
		height: 2.5rem;
		font-size: var(--text-footnote);
	}
	.lg {
		width: 3.5rem;
		height: 3.5rem;
		font-size: var(--text-body);
	}

	img,
	.initials {
		width: 100%;
		height: 100%;
		border-radius: 50%;
		object-fit: cover;
	}

	.initials {
		display: inline-grid;
		place-items: center;
		color: #fff;
		font-weight: var(--weight-semibold);
		letter-spacing: 0.02em;
		user-select: none;
	}

	.dot {
		position: absolute;
		right: -1px;
		bottom: -1px;
		width: 30%;
		height: 30%;
		border-radius: 50%;
		border: 2px solid var(--color-bg-base);
	}
	.dot[data-status='up'] {
		background: var(--status-up);
	}
	.dot[data-status='down'] {
		background: var(--status-down);
	}
	.dot[data-status='pending'] {
		background: var(--status-pending);
	}
	.dot[data-status='maintenance'] {
		background: var(--status-maintenance);
	}
	.dot[data-status='paused'] {
		background: var(--status-paused);
	}
</style>
