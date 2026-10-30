<script lang="ts">
	// SvelteKit renders this for any route it cannot match (status 404) and for
	// uncaught render errors (status 500). The Go server still answers 200 +
	// index.html for unknown paths (the SPA fallback, internal/api/spa.go) — it
	// is SvelteKit that 404s, so this replaces its bare default with something
	// branded and recoverable.
	//
	// The primary action depends on auth state: an operator who mistypes a URL
	// while signed in goes to the dashboard, an anonymous visitor to the login
	// screen, a first-run instance to setup. session.load() swallows its own
	// errors, so this cannot itself error.
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { session } from '$lib/stores/session.svelte';
	import { t } from '$lib/i18n';
	import { GlassPanel, PillButton } from '$lib/design-system';

	// The error page gets no props: SvelteKit exposes the status on page state.
	const status = $derived(page.status);
	const isNotFound = $derived(status === 404);

	let resolved = $state(false);

	const target = $derived(
		session.needsSetup ? '/setup' : session.authenticated ? '/dashboard' : '/login'
	);
	const actionLabel = $derived(
		session.needsSetup
			? t('error.goSetup')
			: session.authenticated
				? t('error.goDashboard')
				: t('error.goLogin')
	);

	onMount(async () => {
		await session.load();
		resolved = true;
	});

	function retry() {
		location.reload();
	}
</script>

<svelte:head><title>{t('error.pageTitle')}</title></svelte:head>

<main class="page">
	<GlassPanel blur="thick" radius="lg" class="card">
		<span class="mark" aria-hidden="true">
			<svg viewBox="0 0 64 64"
				><path
					d="M32 7 50 14v15c0 12-7.7 21.6-18 26-10.3-4.4-18-14-18-26V14L32 7Zm-12 25h7l4-8 6 16 4-8h3"
					fill="none"
					stroke="currentColor"
					stroke-width="4.5"
					stroke-linecap="round"
					stroke-linejoin="round"
				/></svg
			>
		</span>
		<p class="code" aria-hidden="true">{status}</p>
		<h1>{isNotFound ? t('error.notFoundTitle') : t('error.genericTitle')}</h1>
		<p class="body">{isNotFound ? t('error.notFoundBody') : t('error.genericBody')}</p>
		{#if resolved}
			<div class="actions">
				<PillButton type="button" onclick={() => goto(target)}>{actionLabel}</PillButton>
				{#if !isNotFound}
					<PillButton variant="ghost" onclick={retry}>{t('common.retry')}</PillButton>
				{/if}
			</div>
		{/if}
	</GlassPanel>
</main>

<style>
	.page {
		min-height: 100dvh;
		display: grid;
		place-items: center;
		padding: var(--space-6);
	}
	.page :global(.card) {
		width: min(26rem, 100%);
		padding: var(--space-8);
		display: flex;
		flex-direction: column;
		align-items: center;
		text-align: center;
		gap: var(--space-3);
	}
	.mark {
		display: inline-grid;
		place-items: center;
		width: 2.5rem;
		height: 2.5rem;
		margin-bottom: var(--space-2);
		border-radius: var(--radius-squircle-sm);
		color: var(--color-text-primary);
		background: var(--color-accent-tint);
		border: 1px solid var(--glass-border);
		box-shadow: var(--highlight-top);
	}
	.mark svg {
		width: 1.5rem;
		height: 1.5rem;
	}
	.code {
		margin: 0;
		font-size: var(--text-caption-1);
		font-weight: var(--weight-semibold);
		letter-spacing: 0.2em;
		text-transform: uppercase;
		color: var(--color-text-tertiary);
	}
	h1 {
		margin: 0;
		font-size: var(--text-title-2);
		font-weight: var(--weight-bold);
		letter-spacing: var(--tracking-tight);
	}
	.body {
		margin: 0;
		max-width: 22rem;
		font-size: var(--text-subheadline);
		line-height: var(--leading-relaxed);
		color: var(--color-text-secondary);
	}
	.actions {
		margin-top: var(--space-4);
		display: flex;
		gap: var(--space-3);
	}
</style>
