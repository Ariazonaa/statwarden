<script lang="ts">
	import { onMount } from 'svelte';
	import { api, problemMessage, type Tag } from '$lib/api';
	import { session } from '$lib/stores/session.svelte';
	import { t } from '$lib/i18n';
	import {
		EmptyState,
		GlassPanel,
		Modal,
		PillButton,
		Skeleton,
		TextField,
		toast
	} from '$lib/design-system';

	let items = $state<Tag[]>([]);
	let loaded = $state(false);

	let editorOpen = $state(false);
	let editing = $state<Tag | undefined>(undefined);
	let fName = $state('');
	let fColor = $state('#8b5cf6');
	let saving = $state(false);
	let confirmId = $state<string | undefined>(undefined);

	const swatches = ['#ef4444', '#f59e0b', '#10b981', '#0ea5e9', '#8b5cf6', '#ec4899', '#64748b'];

	onMount(async () => {
		items = await api.listTags();
		loaded = true;
	});

	function openCreate() {
		editing = undefined;
		fName = '';
		fColor = swatches[Math.floor(Math.random() * swatches.length)];
		editorOpen = true;
	}
	function openEdit(t: Tag) {
		editing = t;
		fName = t.name;
		fColor = t.color;
		editorOpen = true;
	}
	async function save() {
		if (!fName.trim()) {
			toast.error(t('tags.nameRequired'));
			return;
		}
		saving = true;
		try {
			if (editing) {
				const u = await api.updateTag(editing.id, { name: fName, color: fColor });
				items = items.map((i) => (i.id === u.id ? u : i));
			} else {
				items = [...items, await api.createTag({ name: fName, color: fColor })];
			}
			editorOpen = false;
			toast.success(t('tags.saved'));
		} catch (e) {
			toast.error(problemMessage(e) ?? 'Could not save the tag.');
		} finally {
			saving = false;
		}
	}
	async function remove(id: string) {
		try {
			await api.deleteTag(id);
			items = items.filter((i) => i.id !== id);
			confirmId = undefined;
			toast.success(t('tags.deleted'));
		} catch (e) {
			toast.error(problemMessage(e) ?? 'Could not delete the tag.');
		}
	}
</script>

<svelte:head><title>{t('tags.pageTitle')}</title></svelte:head>

<div class="pagehead">
	<p class="lead">{t('tags.lead')}</p>
	{#if session.canEdit}
		<PillButton size="sm" onclick={openCreate}>{t('tags.newTag')}</PillButton>
	{/if}
</div>

{#if !loaded}
	<Skeleton variant="rect" height="6rem" />
{:else if items.length === 0}
	<GlassPanel radius="lg" class="tg-wrap">
		<EmptyState title={t('tags.emptyTitle')} description={t('tags.emptyDescription')}>
			{#snippet action()}<PillButton onclick={openCreate}>{t('tags.newTag')}</PillButton>{/snippet}
		</EmptyState>
	</GlassPanel>
{:else}
	<div class="grid">
		{#each items as tag (tag.id)}
			<GlassPanel radius="md" class="tagcard">
				<span class="swatch" style="background: {tag.color}"></span>
				<strong class="name" title={tag.name}>{tag.name}</strong>
				{#if session.canEdit}
					<div class="acts">
						<PillButton variant="ghost" size="sm" onclick={() => openEdit(tag)}
							>{t('common.edit')}</PillButton
						>
						<PillButton variant="ghost" size="sm" onclick={() => (confirmId = tag.id)}
							>{t('common.delete')}</PillButton
						>
					</div>
				{/if}
			</GlassPanel>
		{/each}
	</div>
{/if}

<Modal bind:open={editorOpen} title={editing ? t('tags.editTag') : t('tags.newTag')}>
	<div class="form">
		<TextField
			label={t('tags.name')}
			bind:value={fName}
			placeholder={t('tags.namePlaceholder')}
			required
		/>
		<div class="colorpick">
			<span class="pl">{t('tags.colour')}</span>
			<div class="swatches">
				{#each swatches as c (c)}
					<button
						type="button"
						class="pick ds-focusable"
						class:on={fColor === c}
						style="background: {c}"
						aria-label={t('tags.colourNamed', { colour: c })}
						aria-pressed={fColor === c}
						onclick={() => (fColor = c)}
					></button>
				{/each}
				<input
					class="native"
					type="color"
					bind:value={fColor}
					aria-label={t('tags.customColour')}
				/>
			</div>
		</div>
	</div>
	{#snippet footer()}
		<PillButton variant="ghost" onclick={() => (editorOpen = false)}
			>{t('common.cancel')}</PillButton
		>
		<PillButton loading={saving} onclick={save}>{t('common.save')}</PillButton>
	{/snippet}
</Modal>

<Modal
	open={confirmId != null}
	title={t('tags.deleteTitle')}
	onclose={() => (confirmId = undefined)}
>
	<p style="margin:0">{t('tags.deleteBody')}</p>
	{#snippet footer()}
		<PillButton variant="ghost" onclick={() => (confirmId = undefined)}
			>{t('common.cancel')}</PillButton
		>
		<PillButton variant="destructive" onclick={() => confirmId && remove(confirmId)}
			>{t('common.delete')}</PillButton
		>
	{/snippet}
</Modal>

<style>
	.pagehead {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-4);
		margin-bottom: var(--space-5);
	}
	.lead {
		margin: 0;
		color: var(--color-text-secondary);
		font-size: var(--text-subheadline);
	}
	:global(.tg-wrap) {
		padding: var(--space-5);
	}
	.grid {
		display: grid;
		grid-template-columns: repeat(auto-fill, minmax(min(22rem, 100%), 1fr));
		gap: var(--space-3);
	}
	:global(.tagcard) {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-4) var(--space-5);
	}
	.swatch {
		width: 1rem;
		height: 1rem;
		border-radius: 50%;
		flex-shrink: 0;
	}
	.name {
		min-width: 0;
		flex: 1;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	:global(.tagcard) .acts {
		margin-left: auto;
		display: flex;
		gap: var(--space-1);
	}
	.form {
		display: flex;
		flex-direction: column;
		gap: var(--space-5);
	}
	.pl {
		display: block;
		font-size: var(--text-footnote);
		font-weight: var(--weight-medium);
		color: var(--color-text-secondary);
		margin-bottom: var(--space-3);
	}
	.swatches {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-3);
		align-items: center;
	}
	.pick {
		width: 1.75rem;
		height: 1.75rem;
		border-radius: 50%;
		border: 2px solid transparent;
		cursor: pointer;
	}
	.pick.on {
		border-color: var(--color-text-primary);
		box-shadow: 0 0 0 2px var(--color-bg-base);
	}
	.native {
		width: 1.75rem;
		height: 1.75rem;
		padding: 0;
		border: 1px solid var(--hairline);
		border-radius: 50%;
		background: none;
		cursor: pointer;
	}
</style>
