<script lang="ts">
	// Create form. `?clone=<id>` opens it pre-filled from an existing monitor;
	// the form is only mounted once that monitor has loaded, because it seeds its
	// fields from the prop exactly once (untrack) and would otherwise render empty.
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { api, type Monitor } from '$lib/api';
	import MonitorForm from '$lib/components/MonitorForm.svelte';
	import { t } from '$lib/i18n';
	import { Skeleton, toast } from '$lib/design-system';

	const cloneId = page.url.searchParams.get('clone') ?? '';

	let template = $state<Monitor | undefined>(undefined);
	let ready = $state(cloneId === '');

	onMount(async () => {
		if (!cloneId) return;
		try {
			template = await api.getMonitor(cloneId);
		} catch {
			// A stale or bad clone id must not strand the operator on a spinner:
			// fall through to a blank create form and say why.
			toast.error(t('monitorForm.cloneLoadError'));
		} finally {
			ready = true;
		}
	});
</script>

<svelte:head
	><title>{template ? t('title.cloneMonitor') : t('title.newMonitor')} · Statwarden</title
	></svelte:head
>

<h1 class="title">{template ? t('title.cloneMonitor') : t('title.newMonitor')}</h1>

{#if ready}
	<MonitorForm {template} />
{:else}
	<Skeleton variant="rect" height="12rem" />
{/if}

<style>
	.title {
		margin: 0;
		font-size: var(--text-title-1);
		font-weight: var(--weight-bold);
		letter-spacing: var(--tracking-tight);
	}
</style>
