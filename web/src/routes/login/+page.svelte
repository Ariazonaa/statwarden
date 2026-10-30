<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api, problemMessage, problemStatus } from '$lib/api';
	import type { OidcProvider } from '$lib/api';
	import { session } from '$lib/stores/session.svelte';
	import { t, type MessageKey } from '$lib/i18n';
	import { GlassPanel, PillButton, TextField, toast } from '$lib/design-system';

	let username = $state('');
	let password = $state('');
	let code = $state('');
	let backupCode = $state('');
	// Second-factor entry mode inside the twofa stage: an authenticator TOTP code
	// or a one-time backup code. The server accepts either on the same endpoint.
	let mode = $state<'totp' | 'backup'>('totp');
	let stage = $state<'credentials' | 'twofa'>('credentials');
	let loading = $state(false);
	let error = $state<string | undefined>(undefined);
	// Kept apart from `error`, which renders under the password field: an SSO
	// failure has nothing to do with what was typed there.
	let ssoError = $state<string | undefined>(undefined);
	let providers = $state<OidcProvider[]>([]);

	/**
	 * The SSO callback cannot answer problem+json — it is a browser navigation,
	 * not an XHR — so it redirects back here with a coarse, stable code. The codes
	 * are deliberately vague about which check failed; this maps the ones we know
	 * and falls back to the generic message for anything a newer server invents.
	 */
	const SSO_ERRORS: Record<string, MessageKey> = {
		sso_failed: 'login.ssoFailed',
		sso_no_account: 'login.ssoNoAccount',
		sso_email_taken: 'login.ssoEmailTaken'
	};

	/**
	 * The demo-account hint, split around its {user} placeholder so the <code>
	 * wrapper is markup in THIS template rather than a string handed to `t()`.
	 *
	 * It used to be `{@html t('login.tip', { user: '<code>ops</code>' })}`. The
	 * argument was a literal, so nothing was exploitable — but `t()` interpolates
	 * without escaping, so that line was a working recipe for turning any future
	 * caller-supplied value into stored XSS on the unauthenticated login page.
	 * Splitting keeps one translatable sentence, keeps the placeholder in both
	 * catalogues, and needs no {@html} at all. `$derived` because `t()` reads the
	 * locale rune, so this re-renders on a language switch.
	 */
	const tipParts = $derived(t('login.tip').split('{user}'));

	onMount(async () => {
		// Named ssoCode, not code: `code` is the 2FA input field above.
		const ssoCode = page.url.searchParams.get('error');
		if (ssoCode) ssoError = t(SSO_ERRORS[ssoCode] ?? 'login.ssoFailed');

		// Bounce to setup if the instance has no admin yet; skip login if already in.
		await session.load();
		if (session.needsSetup) return await goto('/setup');
		if (session.authenticated) return await goto('/dashboard');

		// SSO is optional and additive: a server without it answers [] or 404, and
		// either way the section simply does not render. A failure here must never
		// block the password form, which is why it is not awaited into the guard
		// above and never surfaces an error.
		try {
			providers = await api.oidcProviders();
		} catch {
			providers = [];
		}
	});

	/**
	 * A full top-level navigation, not a fetch: the IdP round-trip is a redirect
	 * chain that has to happen in the address bar, and the flow cookie the server
	 * sets is SameSite=Lax, which only rides along on a top-level GET.
	 */
	function startSso(id: string) {
		window.location.href = `/api/v1/auth/oidc/${encodeURIComponent(id)}/start`;
	}

	/**
	 * /auth/login verifies TOTP inline and answers 401 with an "invalid or missing
	 * two-factor code" detail when a 2FA-enabled account has supplied no second
	 * factor. That detail is what tells "needs a code" apart from a plain wrong
	 * password (also 401, but "invalid credentials"), so the code field is revealed
	 * only on this signal rather than on every 401.
	 */
	function needsSecondFactor(err: unknown): boolean {
		return problemStatus(err) === 401 && /two-factor/i.test(problemMessage(err) ?? '');
	}

	async function submitCredentials(e: SubmitEvent) {
		e.preventDefault();
		error = undefined;
		loading = true;
		try {
			const res = await api.login({ username, password });
			session.setUser(res.user);
			toast.success(t('login.welcomeBack', { user: res.user.username }));
			await goto('/dashboard');
		} catch (err) {
			if (needsSecondFactor(err)) {
				stage = 'twofa';
			} else {
				error = problemMessage(err) ?? t('login.failed');
			}
		} finally {
			loading = false;
		}
	}

	async function submit2fa(e: SubmitEvent) {
		e.preventDefault();
		error = undefined;
		loading = true;
		try {
			const res = await api.login({
				username,
				password,
				totpCode: mode === 'totp' ? code : undefined,
				backupCode: mode === 'backup' ? backupCode : undefined
			});
			session.setUser(res.user);
			toast.success(t('login.signedIn'));
			await goto('/dashboard');
		} catch (err) {
			error = problemMessage(err) ?? t('login.invalidCode');
		} finally {
			loading = false;
		}
	}

	function toggleFactorMode() {
		mode = mode === 'totp' ? 'backup' : 'totp';
		error = undefined;
	}

	function backToCredentials() {
		stage = 'credentials';
		mode = 'totp';
		code = '';
		backupCode = '';
		error = undefined;
	}
</script>

<svelte:head><title>{t('login.pageTitle')}</title></svelte:head>

<main class="auth">
	<GlassPanel blur="thick" radius="lg" class="card">
		<div class="brand">
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
			<h1>Statwarden</h1>
		</div>

		{#if stage === 'credentials'}
			<p class="lead">{t('login.lead')}</p>
			{#if ssoError}<p class="err" role="alert">{ssoError}</p>{/if}
			<form onsubmit={submitCredentials} class="form">
				<TextField
					label={t('login.username')}
					bind:value={username}
					autocomplete="username"
					placeholder="admin"
					required
				/>
				<TextField
					label={t('login.password')}
					type="password"
					bind:value={password}
					autocomplete="current-password"
					required
					{error}
				/>
				<PillButton type="submit" {loading} class="submit">{t('login.signIn')}</PillButton>
			</form>
			<p class="forgot"><a href="/forgot-password">{t('login.forgotPassword')}</a></p>
			{#if providers.length > 0}
				<div class="sso">
					<span class="divider">{t('login.ssoDivider')}</span>
					{#each providers as p (p.id)}
						<PillButton variant="ghost" type="button" onclick={() => startSso(p.id)}>
							{t('login.ssoSignInWith', { provider: p.displayName })}
						</PillButton>
					{/each}
				</div>
			{/if}
			<p class="hint">{tipParts[0]}<code>ops</code>{tipParts[1] ?? ''}</p>
		{:else}
			{#if mode === 'totp'}
				<p class="lead">{t('login.twofaLead')}</p>
			{/if}
			<form onsubmit={submit2fa} class="form">
				{#if mode === 'totp'}
					<TextField
						label={t('login.code')}
						bind:value={code}
						inputmode="numeric"
						placeholder="123456"
						autocomplete="one-time-code"
						required
						{error}
					/>
				{:else}
					<TextField
						label={t('profile.backupCodes')}
						bind:value={backupCode}
						placeholder="xxxx-xxxx"
						autocomplete="one-time-code"
						required
						{error}
					/>
				{/if}
				<PillButton type="submit" {loading} class="submit">{t('login.verify')}</PillButton>
				<PillButton variant="ghost" type="button" onclick={backToCredentials}
					>{t('common.back')}</PillButton
				>
			</form>
			<p class="forgot">
				<button type="button" class="linklike" onclick={toggleFactorMode}>
					{mode === 'totp' ? t('profile.backupCodes') : t('login.code')}
				</button>
			</p>
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
		width: min(24rem, 100%);
		padding: var(--space-8) var(--space-8) var(--space-7);
	}
	.brand {
		display: flex;
		align-items: center;
		gap: var(--space-4);
		margin-bottom: var(--space-5);
	}
	.mark {
		display: inline-grid;
		place-items: center;
		width: 2.5rem;
		height: 2.5rem;
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
	h1 {
		margin: 0;
		font-size: var(--text-title-2);
		font-weight: var(--weight-bold);
		letter-spacing: var(--tracking-tight);
	}
	.lead {
		margin: 0 0 var(--space-6);
		color: var(--color-text-secondary);
		font-size: var(--text-subheadline);
	}
	.form {
		display: flex;
		flex-direction: column;
		gap: var(--space-5);
	}
	.auth :global(.submit) {
		margin-top: var(--space-2);
	}
	.err {
		margin: 0 0 var(--space-5);
		font-size: var(--text-caption-1);
		color: var(--color-danger);
	}
	.forgot {
		margin: var(--space-4) 0 0;
		text-align: center;
		font-size: var(--text-caption-1);
	}
	.forgot a {
		color: var(--color-accent);
		text-decoration: none;
	}
	.forgot a:hover {
		text-decoration: underline;
	}
	.linklike {
		background: none;
		border: none;
		padding: 0;
		font: inherit;
		color: var(--color-accent);
		cursor: pointer;
	}
	.linklike:hover {
		text-decoration: underline;
	}
	.sso {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
		margin-top: var(--space-6);
	}
	.divider {
		display: flex;
		align-items: center;
		gap: var(--space-4);
		font-size: var(--text-caption-1);
		color: var(--color-text-tertiary);
	}
	.divider::before,
	.divider::after {
		content: '';
		flex: 1;
		height: 1px;
		background: var(--hairline);
	}
	.hint {
		margin: var(--space-6) 0 0;
		font-size: var(--text-caption-1);
		color: var(--color-text-tertiary);
	}
	.hint :global(code) {
		font-family: var(--font-mono);
		color: var(--color-text-secondary);
	}
</style>
