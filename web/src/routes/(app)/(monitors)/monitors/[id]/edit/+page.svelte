<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api, problemMessage, problemStatus, type Monitor } from '$lib/api';
	import MonitorForm from '$lib/components/MonitorForm.svelte';
	import { t } from '$lib/i18n';
	import { EmptyState, PillButton, Skeleton, toast } from '$lib/design-system';

	const id = page.params.id ?? '';
	let monitor = $state<Monitor | undefined>(undefined);
	// Set when the fetch fails so the operator sees an escape instead of a
	// skeleton that never resolves — same shape as the clone fetch in
	// monitors/new, but here there is no blank form to fall through to.
	let loadError = $state<string | undefined>(undefined);

	async function load() {
		loadError = undefined;
		try {
			monitor = await api.getMonitor(id);
		} catch (err) {
			// A 404 means the monitor is gone (deleted, or a hand-typed /
			// bookmarked id — the server serves index.html for the path, so this
			// view has to notice). Nothing to edit, so back to the dashboard
			// with a short note; loadError doubles as the fallback if the
			// navigation is cancelled. Every other failure stays put for retry.
			if (problemStatus(err) === 404) {
				loadError = t('monitor.notFound');
				toast.info(t('monitor.notFound'));
				goto('/dashboard').catch(() => {});
				return;
			}
			const msg = problemMessage(err) ?? 'Could not load this monitor.';
			loadError = msg;
			toast.error(msg);
		}
	}
	onMount(load);
</script>

<svelte:head><title>{t('title.editMonitor')} · Statwarden</title></svelte:head>

<h1 class="title">{t('title.editMonitor')}</h1>

{#if monitor}
	<MonitorForm existing={monitor} />
{:else if loadError}
	<div class="loaderr ds-glass">
		<EmptyState title={loadError ?? ''}>
			{#snippet action()}
				<PillButton variant="secondary" size="sm" onclick={() => goto('/dashboard')}>
					{t('common.back')}
				</PillButton>
				<PillButton size="sm" onclick={load}>Retry</PillButton>
			{/snippet}
		</EmptyState>
	</div>
{:else}
	<Skeleton variant="rect" height="16rem" />
{/if}

<style>
	.title {
		margin: 0;
		font-size: var(--text-title-1);
		font-weight: var(--weight-bold);
		letter-spacing: var(--tracking-tight);
	}
	.loaderr {
		border-radius: var(--radius-squircle-lg);
		padding: var(--space-6);
	}
	/* EmptyState renders its CTA in a child .action; reach it to space the pair. */
	.loaderr :global(.action) {
		display: flex;
		flex-wrap: wrap;
		justify-content: center;
		gap: var(--space-3);
	}
</style>
