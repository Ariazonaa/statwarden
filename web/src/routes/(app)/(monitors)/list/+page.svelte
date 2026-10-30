<script lang="ts">
	// The phone's monitor list. The list itself is rendered by the area layout
	// (it is the same component the desktop keeps on the left); on a phone this
	// route is the one where the layout shows it. On a wide screen the list is
	// already beside this pane, so the pane only points at it.
	import { t } from '$lib/i18n';
	import { EmptyState } from '$lib/design-system';
</script>

<svelte:head><title>{t('shell.list')} · Statwarden</title></svelte:head>

<div class="hint">
	<EmptyState title={t('monitorList.pickOne')} />
</div>

<style>
	.hint {
		border: 1px solid var(--hairline);
		border-radius: var(--radius-squircle-md);
		background: var(--glass-bg-thin);
		padding: var(--space-8) var(--space-6);
	}
</style>
