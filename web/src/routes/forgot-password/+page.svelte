<script lang="ts">
	// Password-reset request form. Public and unauthenticated.
	//
	// The server answers 202 with an identical body for a known address, an
	// unknown address, a deactivated account and an unconfigured mailer — it has
	// exactly one exit path, so that an attacker cannot use this form to learn
	// which addresses have accounts. This screen must not undo that: on success it
	// says a mail has been sent IF the address has an account, and it never
	// reports "no such user". The only errors it can surface are transport
	// failures and the rate limiter.
	import { goto } from '$app/navigation';
	import { api, problemStatus, problemMessage } from '$lib/api';
	import { t } from '$lib/i18n';
	import { GlassPanel, PillButton, TextField } from '$lib/design-system';

	let email = $state('');
	let loading = $state(false);
	let error = $state<string | undefined>(undefined);
	let sent = $state(false);

	const valid = $derived(email.trim() !== '');

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		if (!valid || loading) return;
		loading = true;
		error = undefined;
		try {
			await api.requestPasswordReset(email.trim());
			sent = true;
		} catch (err) {
			// 429 is the outbound-mail rate limiter and is worth naming, because
			// "try again" is actionable. Everything else gets the server's own
			// detail, falling back to a generic failure.
			error =
				problemStatus(err) === 429
					? t('passwordReset.rateLimited')
					: (problemMessage(err) ?? t('passwordReset.requestFailed'));
		} finally {
			loading = false;
		}
	}
</script>

<svelte:head><title>{t('passwordReset.requestPageTitle')}</title></svelte:head>

<main class="auth">
	<GlassPanel blur="thick" radius="lg" class="card">
		{#if sent}
			<h1>{t('passwordReset.sentHeading')}</h1>
			<p class="lead">{t('passwordReset.sentBody')}</p>
			<p class="lead">{t('passwordReset.sentHint')}</p>
			<PillButton type="button" class="submit" onclick={() => goto('/login')}>
				{t('passwordReset.backToLogin')}
			</PillButton>
		{:else}
			<h1>{t('passwordReset.requestHeading')}</h1>
			<p class="lead">{t('passwordReset.requestLead')}</p>
			<form onsubmit={submit} class="form">
				<TextField
					label={t('passwordReset.email')}
					type="email"
					bind:value={email}
					autocomplete="email"
					placeholder="you@example.com"
					required
				/>
				{#if error}<p class="err" role="alert">{error}</p>{/if}
				<PillButton type="submit" {loading} disabled={!valid} class="submit">
					{t('passwordReset.requestSubmit')}
				</PillButton>
			</form>
			<p class="hint"><a href="/login">{t('passwordReset.backToLogin')}</a></p>
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
		text-align: center;
		font-size: var(--text-caption-1);
	}
	.hint a {
		color: var(--color-accent);
		text-decoration: none;
	}
	.hint a:hover {
		text-decoration: underline;
	}
</style>
