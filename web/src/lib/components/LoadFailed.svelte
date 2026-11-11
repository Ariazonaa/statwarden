<script lang="ts">
	// What a list screen shows when its first load fails. Without it those pages
	// awaited the request with no catch, so a 5xx or a dropped connection left
	// them on the skeleton forever, with nothing to say a retry might help.
	import { t } from '$lib/i18n';
	import { EmptyState, GlassPanel, PillButton } from '$lib/design-system';

	let { onretry }: { onretry: () => void } = $props();
</script>

<GlassPanel radius="lg">
	<EmptyState title={t('common.loadError')}>
		{#snippet action()}
			<PillButton size="sm" variant="secondary" onclick={onretry}>{t('common.retry')}</PillButton>
		{/snippet}
	</EmptyState>
</GlassPanel>
