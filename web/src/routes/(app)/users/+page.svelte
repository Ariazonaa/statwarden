<script lang="ts">
	import { onMount } from 'svelte';
	import { api, problemMessage, type Role, type User } from '$lib/api';
	import { session } from '$lib/stores/session.svelte';
	import RelativeTime from '$lib/components/RelativeTime.svelte';
	import { t } from '$lib/i18n';
	import {
		Avatar,
		GlassPanel,
		Modal,
		PillButton,
		PillToggle,
		Select,
		Skeleton,
		TextField,
		toast
	} from '$lib/design-system';

	let items = $state<User[]>([]);
	let loaded = $state(false);

	let editorOpen = $state(false);
	let editing = $state<User | undefined>(undefined);
	let fUsername = $state('');
	let fEmail = $state('');
	let fPassword = $state('');
	let fRole = $state<Role>('viewer');
	let fDisabled = $state(false);

	// The server requires (and hashes) an initial password on create; its own
	// validator rejects anything shorter, so mirror the 8-char floor here.
	const MIN_PASSWORD = 8;
	let saving = $state(false);
	let confirmId = $state<string | undefined>(undefined);

	const roleOptions = $derived([
		{ value: 'admin', label: t('users.roleAdmin') },
		{ value: 'user', label: t('users.roleUser') },
		{ value: 'viewer', label: t('users.roleViewer') }
	]);
	const roleLabel = (role: Role) =>
		role === 'admin'
			? t('users.roleAdminShort')
			: role === 'user'
				? t('users.roleUserShort')
				: t('users.roleViewerShort');

	onMount(async () => {
		items = await api.listUsers();
		loaded = true;
	});

	function openCreate() {
		editing = undefined;
		fUsername = '';
		fEmail = '';
		fPassword = '';
		fRole = 'viewer';
		fDisabled = false;
		editorOpen = true;
	}
	function openEdit(u: User) {
		editing = u;
		fUsername = u.username;
		fEmail = u.email;
		fPassword = '';
		fRole = u.role;
		fDisabled = u.disabled;
		editorOpen = true;
	}
	async function save() {
		if (!fUsername.trim()) {
			toast.error(t('users.usernameRequired'));
			return;
		}
		// On create the email is the login identity the server validates, and the
		// password is required and hashed; the update DTO carries neither, so both
		// are checked only when minting a new account.
		if (!editing && !fEmail.includes('@')) {
			toast.error(t('users.emailRequired'));
			return;
		}
		if (!editing && fPassword.length < MIN_PASSWORD) {
			toast.error(t('users.passwordTooShort'));
			return;
		}
		saving = true;
		try {
			const payload = { username: fUsername, email: fEmail, role: fRole, disabled: fDisabled };
			if (editing) {
				const u = await api.updateUser(editing.id, payload);
				items = items.map((i) => (i.id === u.id ? u : i));
			} else {
				items = [...items, await api.createUser({ ...payload, password: fPassword })];
			}
			editorOpen = false;
			toast.success(t('users.saved'));
		} catch (e) {
			toast.error(problemMessage(e) ?? 'Could not save the user.');
		} finally {
			saving = false;
		}
	}
	async function remove(id: string) {
		try {
			await api.deleteUser(id);
			items = items.filter((i) => i.id !== id);
			confirmId = undefined;
			toast.success(t('users.removed'));
		} catch (e) {
			toast.error(problemMessage(e) ?? 'Could not remove the user.');
		}
	}
</script>

<svelte:head><title>{t('users.pageTitle')}</title></svelte:head>

<div class="pagehead">
	<p class="lead">{t('users.lead')}</p>
	<PillButton size="sm" onclick={openCreate}>{t('users.newUser')}</PillButton>
</div>

{#if !loaded}
	<Skeleton variant="rect" height="8rem" />
{:else}
	<div class="list">
		{#each items as u (u.id)}
			<GlassPanel radius="md" class="us-row">
				<Avatar name={u.username} src={u.avatarUrl} />
				<div class="info">
					<div class="titleline">
						<strong>{u.username}</strong>
						<span class="role" data-role={u.role}>{roleLabel(u.role)}</span>
						{#if u.totpEnabled}<span class="tag">2FA</span>{/if}
						{#if u.disabled}<span class="tag off">{t('common.disabled')}</span>{/if}
						{#if u.id === session.user?.id}<span class="tag you">{t('common.you')}</span>{/if}
					</div>
					<span class="sub">
						{u.email || t('common.noEmail')} · {t('users.lastLogin')}
						<RelativeTime value={u.lastLoginAt} />
					</span>
				</div>
				<div class="rowactions">
					<PillButton variant="ghost" size="sm" onclick={() => openEdit(u)}
						>{t('common.edit')}</PillButton
					>
					{#if u.id !== session.user?.id}
						<PillButton variant="ghost" size="sm" onclick={() => (confirmId = u.id)}
							>{t('common.remove')}</PillButton
						>
					{/if}
				</div>
			</GlassPanel>
		{/each}
	</div>
{/if}

<Modal bind:open={editorOpen} title={editing ? t('users.editUser') : t('users.newUser')}>
	<div class="form">
		<TextField label={t('users.username')} bind:value={fUsername} required />
		<TextField
			label={t('users.email')}
			type="email"
			bind:value={fEmail}
			placeholder="user@example.com"
		/>
		{#if !editing}
			<TextField
				label={t('users.password')}
				type="password"
				bind:value={fPassword}
				required
				hint={t('users.passwordHint')}
			/>
		{/if}
		<Select
			label={t('users.role')}
			value={fRole}
			options={roleOptions}
			onchange={(v) => (fRole = v as Role)}
		/>
		<PillToggle label={t('users.accountDisabled')} bind:checked={fDisabled} />
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
	title={t('users.deleteTitle')}
	onclose={() => (confirmId = undefined)}
>
	<p style="margin:0">{t('users.deleteBody')}</p>
	{#snippet footer()}
		<PillButton variant="ghost" onclick={() => (confirmId = undefined)}
			>{t('common.cancel')}</PillButton
		>
		<PillButton variant="destructive" onclick={() => confirmId && remove(confirmId)}
			>{t('common.remove')}</PillButton
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
	.list {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}
	:global(.us-row) {
		display: flex;
		align-items: center;
		gap: var(--space-4);
		padding: var(--space-4) var(--space-5);
	}
	.info {
		flex: 1;
		min-width: 0;
	}
	.titleline {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		flex-wrap: wrap;
	}
	.role {
		font-size: var(--text-caption-2);
		padding: 1px var(--space-3);
		border-radius: var(--radius-pill);
		text-transform: capitalize;
		background: var(--glass-bg-thin);
		border: 1px solid var(--hairline);
		color: var(--color-text-secondary);
	}
	.role[data-role='admin'] {
		color: var(--color-accent);
		background: var(--color-accent-tint);
		border-color: transparent;
	}
	.tag {
		font-size: var(--text-caption-2);
		padding: 1px var(--space-3);
		border-radius: var(--radius-pill);
		background: var(--status-up-tint);
		color: var(--status-up-text);
	}
	.tag.off {
		background: var(--status-paused-tint);
		color: var(--status-paused-text);
	}
	.tag.you {
		background: var(--color-accent-tint);
		color: var(--color-accent);
	}
	.sub {
		font-size: var(--text-caption-1);
		color: var(--color-text-tertiary);
	}
	.rowactions {
		display: flex;
		gap: var(--space-1);
		flex-shrink: 0;
	}
	.form {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}
	/* On a phone the actions wrap under the name, indented past the avatar
	   (2.5rem + the row gap), instead of squeezing the e-mail into a sliver. */
	@media (max-width: 640px) {
		:global(.us-row) {
			flex-wrap: wrap;
		}
		.rowactions {
			flex-basis: 100%;
			padding-left: calc(2.5rem + var(--space-4));
		}
	}
</style>
