<script lang="ts">
	// Reset-completion landing page, reached from the mailed link. Public and
	// unauthenticated; reads ?token= exactly like the subscriber double-opt-in
	// page at routes/status/[slug]/verify.
	//
	// The server answers ONE generic 400 for every rejection — expired, already
	// used, unknown and malformed are indistinguishable on purpose, so that the
	// page cannot be used to probe which tokens once existed. This screen is
	// honest about that instead of papering over it: it says the link is no longer
	// usable and offers a new one, rather than failing silently or claiming to
	// know which of the four happened.
	//
	// Success does NOT sign the user in: the server deliberately issues no session
	// from a mailed link, so this routes to /login.
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api, problemStatus, problemMessage } from '$lib/api';
	import { t } from '$lib/i18n';
	import { GlassPanel, PillButton, TextField, toast } from '$lib/design-system';

	// Read once, at construction: the token must not change under the form.
	const token = page.url.searchParams.get('token') ?? '';

	let password = $state('');
	let confirm = $state('');
	let loading = $state(false);
	let error = $state<string | undefined>(undefined);
	// Set when the server rejects the token itself, which is terminal — the form
	// is replaced rather than left there inviting a retry that cannot succeed.
	let tokenDead = $state(false);

	const passwordError = $derived(
		password && password.length < 8 ? t('passwordReset.passwordTooShort') : undefined
	);
	const confirmError = $derived(
		confirm && confirm !== password ? t('passwordReset.passwordsMismatch') : undefined
	);
	const valid = $derived(password.length >= 8 && confirm === password);

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		if (!valid || loading) return;
		loading = true;
		error = undefined;
		try {
			await api.confirmPasswordReset({ token, password });
			toast.success(t('passwordReset.doneToast'));
			await goto('/login');
		} catch (err) {
			// A 400 here is the token verdict, not a complaint about the password:
			// the client already enforces the 8-character minimum, so the only 400
			// that can realistically arrive is the uniform invalid-or-expired one.
			if (problemStatus(err) === 400) tokenDead = true;
			else error = problemMessage(err) ?? t('passwordReset.confirmFailed');
		} finally {
			loading = false;
		}
	}
</script>

<svelte:head><title>{t('passwordReset.confirmPageTitle')}</title></svelte:head>

<main class="auth">
	<GlassPanel blur="thick" radius="lg" class="card">
		{#if !token || tokenDead}
			<h1>{t('passwordReset.linkDeadHeading')}</h1>
			<p class="lead">
				{token ? t('passwordReset.linkDeadBody') : t('passwordReset.linkMissingBody')}
			</p>
			<PillButton type="button" class="submit" onclick={() => goto('/forgot-password')}>
				{t('passwordReset.requestNewLink')}
			</PillButton>
		{:else}
			<h1>{t('passwordReset.confirmHeading')}</h1>
			<p class="lead">{t('passwordReset.confirmLead')}</p>
			<form onsubmit={submit} class="form">
				<TextField
					label={t('passwordReset.newPassword')}
					type="password"
					bind:value={password}
					autocomplete="new-password"
					hint={t('passwordReset.passwordHint')}
					error={passwordError}
					required
				/>
				<TextField
					label={t('passwordReset.confirmPassword')}
					type="password"
					bind:value={confirm}
					autocomplete="new-password"
					error={confirmError}
					required
				/>
				{#if error}<p class="err" role="alert">{error}</p>{/if}
				<PillButton type="submit" {loading} disabled={!valid} class="submit">
					{t('passwordReset.confirmSubmit')}
				</PillButton>
			</form>
			<p class="hint">{t('passwordReset.signsOutNote')}</p>
		{/if}
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
	.hint {
		margin: var(--space-6) 0 0;
		font-size: var(--text-caption-1);
		color: var(--color-text-tertiary);
	}
</style>
