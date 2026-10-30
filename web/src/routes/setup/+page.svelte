<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { api, ApiError } from '$lib/api';
	import { session } from '$lib/stores/session.svelte';
	import { t } from '$lib/i18n';
	import { GlassPanel, PillButton, TextField, toast } from '$lib/design-system';

	let username = $state('admin');
	let email = $state('');
	let password = $state('');
	let confirm = $state('');
	let loading = $state(false);
	let error = $state<string | undefined>(undefined);

	onMount(async () => {
		await session.load();
		// If setup is already done, don't allow re-running the wizard.
		if (!session.needsSetup) await goto(session.authenticated ? '/dashboard' : '/login');
	});

	const passwordError = $derived(
		password && password.length < 8 ? t('setup.passwordTooShort') : undefined
	);
	const confirmError = $derived(
		confirm && confirm !== password ? t('setup.passwordsMismatch') : undefined
	);
	const valid = $derived(
		username.trim() !== '' && email.trim() !== '' && password.length >= 8 && confirm === password
	);

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		if (!valid) return;
		loading = true;
		error = undefined;
		try {
			const user = await api.setup({ username, email, password });
			session.setUser(user);
			toast.success(t('setup.created'));
			await goto('/dashboard');
		} catch (err) {
			error = err instanceof ApiError ? err.message : t('setup.failed');
		} finally {
			loading = false;
		}
	}
</script>

<svelte:head><title>{t('setup.pageTitle')}</title></svelte:head>

<main class="auth">
	<GlassPanel blur="thick" radius="lg" class="card">
		<span class="badge">{t('setup.badge')}</span>
		<h1>{t('setup.heading')}</h1>
		<p class="lead">{t('setup.lead')}</p>

		<form onsubmit={submit} class="form">
			<TextField
				label={t('setup.username')}
				bind:value={username}
				autocomplete="username"
				required
			/>
			<TextField
				label={t('setup.email')}
				type="email"
				bind:value={email}
				autocomplete="email"
				placeholder="you@example.com"
				required
			/>
			<TextField
				label={t('setup.password')}
				type="password"
				bind:value={password}
				autocomplete="new-password"
				hint={t('setup.passwordHint')}
				error={passwordError}
				required
			/>
			<TextField
				label={t('setup.confirmPassword')}
				type="password"
				bind:value={confirm}
				autocomplete="new-password"
				error={confirmError}
				required
			/>
			{#if error}<p class="err">{error}</p>{/if}
			<PillButton type="submit" {loading} disabled={!valid} class="submit">
				{t('setup.submit')}
			</PillButton>
		</form>
	</GlassPanel>
</main>

<style>
	.auth {
		min-height: 100dvh;
		display: grid;
		place-items: center;
		padding: var(--space-6);
	}
	.auth :global(.card) {
		width: min(26rem, 100%);
		padding: var(--space-8);
	}
	.badge {
		display: inline-block;
		margin-bottom: var(--space-4);
		padding: var(--space-1) var(--space-4);
		border-radius: var(--radius-pill);
		background: var(--color-accent-tint);
		color: var(--color-accent);
		font-size: var(--text-caption-1);
		font-weight: var(--weight-semibold);
	}
	h1 {
		margin: 0;
		font-size: var(--text-title-2);
		font-weight: var(--weight-bold);
		letter-spacing: var(--tracking-tight);
	}
	.lead {
		margin: var(--space-3) 0 var(--space-6);
		color: var(--color-text-secondary);
		font-size: var(--text-subheadline);
		line-height: var(--leading-relaxed);
	}
	.form {
		display: flex;
		flex-direction: column;
		gap: var(--space-5);
	}
	.err {
		margin: 0;
		font-size: var(--text-caption-1);
		color: var(--color-danger);
	}
	.auth :global(.submit) {
		margin-top: var(--space-2);
	}
</style>
