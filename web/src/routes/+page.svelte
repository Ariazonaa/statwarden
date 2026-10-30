<script lang="ts">
	// Entry point: resolve auth state and route to the right place —
	// first-run setup, login, or the dashboard.
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { session } from '$lib/stores/session.svelte';
	import { Skeleton } from '$lib/design-system';

	onMount(async () => {
		await session.load();
		if (session.needsSetup) await goto('/setup');
		else if (!session.authenticated) await goto('/login');
		else await goto('/dashboard');
	});
</script>

<svelte:head><title>Statwarden</title></svelte:head>

<div class="splash">
	<div class="mark" aria-hidden="true">
		<svg viewBox="0 0 64 64" role="img">
			<path
				d="M20 22v14a12 12 0 0 0 24 0V22"
				fill="none"
				stroke="currentColor"
				stroke-width="6"
				stroke-linecap="round"
			/>
		</svg>
	</div>
	<Skeleton width="8rem" height="0.75rem" />
</div>

<style>
	.splash {
		min-height: 100dvh;
		display: grid;
		place-items: center;
		gap: var(--space-6);
	}
	.mark {
		display: inline-grid;
		place-items: center;
		width: 4rem;
		height: 4rem;
		border-radius: var(--radius-squircle-md);
		color: var(--color-text-primary);
		background: var(--color-accent-tint);
		border: 1px solid var(--glass-border);
		box-shadow: var(--highlight-top), var(--shadow-2);
	}
	.mark svg {
		width: 2.25rem;
		height: 2.25rem;
	}
</style>
