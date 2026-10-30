<script lang="ts">
	import { onMount } from 'svelte';
	import {
		api,
		configFromForm,
		configToForm,
		notificationTypeSpecs,
		problemMessage,
		type Notification
	} from '$lib/api';
	import { session } from '$lib/stores/session.svelte';
	import RelativeTime from '$lib/components/RelativeTime.svelte';
	import ConfigFields from '$lib/components/ConfigFields.svelte';
	import { t } from '$lib/i18n';
	import {
		EmptyState,
		GlassPanel,
		Modal,
		PillButton,
		PillToggle,
		Select,
		Skeleton,
		TextField,
		toast
	} from '$lib/design-system';

	let items = $state<Notification[]>([]);
	let loaded = $state(false);
	let testing = $state<string | undefined>(undefined);

	let editorOpen = $state(false);
	let editing = $state<Notification | undefined>(undefined);
	let fName = $state('');
	let fType = $state('slack');
	let fDefault = $state(false);
	let fEnabled = $state(true);
	let fConfig = $state<Record<string, unknown>>({});
	let saving = $state(false);

	let confirmId = $state<string | undefined>(undefined);

	const spec = $derived(
		notificationTypeSpecs.find((s) => s.type === fType) ?? notificationTypeSpecs[0]
	);

	onMount(load);
	async function load() {
		items = await api.listNotifications();
		loaded = true;
	}

	function openCreate() {
		editing = undefined;
		fName = '';
		fType = 'slack';
		fDefault = false;
		fEnabled = true;
		fConfig = {};
		editorOpen = true;
	}
	// initialConfigJson is the form's config as it looked when the editor
	// opened. Comparing against it at save time distinguishes "the user cleared
	// this field" from "the user never typed in it" — which an empty form
	// cannot express on its own, and which decides whether the server is told
	// to replace the configuration or to leave it alone.
	let initialConfigJson = $state('');

	function openEdit(n: Notification) {
		editing = n;
		fName = n.name;
		fType = n.type;
		fDefault = n.isDefault;
		fEnabled = n.enabled;
		// Unpack arrays/objects into the text their widgets hold; save() packs
		// them back with configFromForm.
		fConfig = configToForm(
			notificationTypeSpecs.find((s) => s.type === n.type)?.fields ?? [],
			n.config
		);
		initialConfigJson = JSON.stringify($state.snapshot(fConfig));
		editorOpen = true;
	}

	async function save() {
		if (!fName.trim()) {
			toast.error(t('notifications.nameRequired'));
			return;
		}
		saving = true;
		try {
			// config is UNDEFINED when editing a form the user never typed in.
			//
			// A channel's config is hidden wholesale on read, so the editor opens
			// blank; submitting that blank form as the new configuration is how
			// renaming a channel came to wipe its credentials. Leaving the field
			// out tells the server to keep what it has — and undefined rather
			// than `{}`, because an empty object is a real, empty configuration.
			const untouched =
				editing !== undefined && JSON.stringify($state.snapshot(fConfig)) === initialConfigJson;
			const payload: Partial<Notification> = {
				name: fName,
				type: fType,
				isDefault: fDefault,
				enabled: fEnabled,
				// The only place form state becomes a wire config — see registry.ts.
				config: untouched ? undefined : configFromForm(spec.fields, $state.snapshot(fConfig))
			};
			if (editing) {
				const u = await api.updateNotification(editing.id, payload);
				items = items.map((i) => (i.id === u.id ? u : i));
			} else {
				items = [...items, await api.createNotification(payload)];
			}
			editorOpen = false;
			toast.success(t('notifications.saved'));
		} catch (e) {
			toast.error(problemMessage(e) ?? 'Could not save the notification channel.');
		} finally {
			saving = false;
		}
	}

	async function remove(id: string) {
		try {
			await api.deleteNotification(id);
			items = items.filter((i) => i.id !== id);
			confirmId = undefined;
			toast.success(t('notifications.deleted'));
		} catch (e) {
			toast.error(problemMessage(e) ?? 'Could not delete the channel.');
		}
	}

	async function test(n: Notification) {
		testing = n.id;
		try {
			await api.testNotification(n.id);
			toast.success(t('notifications.testSent', { name: n.name }));
		} catch (e) {
			toast.error(e instanceof Error ? e.message : t('notifications.testFailed'));
		} finally {
			testing = undefined;
		}
	}

	function typeLabel(type: string): string {
		const s = notificationTypeSpecs.find((x) => x.type === type);
		return s ? t(s.label) : type;
	}
</script>

<svelte:head><title>{t('notifications.pageTitle')}</title></svelte:head>

<div class="pagehead">
	<p class="lead">{t('notifications.lead')}</p>
	{#if session.canEdit}
		<PillButton size="sm" onclick={openCreate}>{t('notifications.newChannel')}</PillButton>
	{/if}
</div>

{#if !loaded}
	<Skeleton variant="rect" height="8rem" />
{:else if items.length === 0}
	<GlassPanel radius="lg" class="nt-wrap">
		<EmptyState
			title={t('notifications.emptyTitle')}
			description={t('notifications.emptyDescription')}
		>
			{#snippet action()}
				<PillButton onclick={openCreate}>{t('notifications.newChannel')}</PillButton>
			{/snippet}
		</EmptyState>
	</GlassPanel>
{:else}
	<div class="list">
		{#each items as n (n.id)}
			<GlassPanel radius="md" class="nt-row">
				<div class="info">
					<div class="titleline">
						<span class="type">{typeLabel(n.type)}</span>
						<strong>{n.name}</strong>
						{#if n.isDefault}<span class="badge">{t('common.default')}</span>{/if}
						{#if !n.enabled}<span class="badge off">{t('common.disabled')}</span>{/if}
					</div>
					<span class="sub">{t('notifications.created')} <RelativeTime value={n.createdAt} /></span>
				</div>
				<div class="rowactions">
					<PillButton
						variant="secondary"
						size="sm"
						loading={testing === n.id}
						onclick={() => test(n)}
					>
						{t('common.test')}
					</PillButton>
					{#if session.canEdit}
						<PillButton variant="ghost" size="sm" onclick={() => openEdit(n)}
							>{t('common.edit')}</PillButton
						>
						<PillButton variant="ghost" size="sm" onclick={() => (confirmId = n.id)}
							>{t('common.delete')}</PillButton
						>
					{/if}
				</div>
			</GlassPanel>
		{/each}
	</div>
{/if}

<Modal
	bind:open={editorOpen}
	title={editing ? t('notifications.editChannel') : t('notifications.newChannel')}
>
	<div class="form">
		<TextField
			label={t('notifications.name')}
			bind:value={fName}
			placeholder={t('notifications.namePlaceholder')}
			required
		/>
		{#if !editing}
			<Select
				label={t('notifications.type')}
				bind:value={fType}
				options={notificationTypeSpecs.map((s) => ({ value: s.type, label: t(s.label) }))}
			/>
		{/if}
		<p class="spec-desc">{t(spec.description)}</p>
		<ConfigFields fields={spec.fields} bind:config={fConfig} />
		<div class="toggles">
			<PillToggle label={t('notifications.enabled')} bind:checked={fEnabled} />
			<PillToggle label={t('notifications.attachDefault')} bind:checked={fDefault} />
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
	title={t('notifications.deleteTitle')}
	onclose={() => (confirmId = undefined)}
>
	<p style="margin:0">{t('notifications.deleteBody')}</p>
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
	:global(.nt-wrap) {
		padding: var(--space-5);
	}
	.list {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}
	:global(.nt-row) {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-4);
		padding: var(--space-4) var(--space-5);
	}
	.titleline {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		flex-wrap: wrap;
	}
	.type {
		font-size: var(--text-caption-1);
		color: var(--color-text-tertiary);
		text-transform: uppercase;
		letter-spacing: var(--tracking-wide);
	}
	.badge {
		font-size: var(--text-caption-2);
		padding: 1px var(--space-3);
		border-radius: var(--radius-pill);
		background: var(--color-accent-tint);
		color: var(--color-accent);
	}
	.badge.off {
		background: var(--status-paused-tint);
		color: var(--status-paused-text);
	}
	.sub {
		font-size: var(--text-caption-1);
		color: var(--color-text-tertiary);
	}
	.rowactions {
		display: flex;
		gap: var(--space-2);
		flex-shrink: 0;
	}
	.form {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}
	.spec-desc {
		margin: 0;
		font-size: var(--text-caption-1);
		color: var(--color-text-tertiary);
	}
	.toggles {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
		margin-top: var(--space-2);
	}
	@media (max-width: 640px) {
		:global(.nt-row) {
			flex-direction: column;
			align-items: stretch;
			gap: var(--space-3);
		}
	}
</style>
