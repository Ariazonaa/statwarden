<script lang="ts">
	// Re-authentication prompt for the actions that change how an account can be
	// logged into: minting an API key, enrolling an authenticator, linking single
	// sign-on, regenerating backup codes.
	//
	// Each of those turns a borrowed session — an unlocked laptop, a stolen
	// cookie — into standing access that outlives the session and survives the
	// owner changing their password, so the server (internal/api/reauth.go)
	// demands the password again, plus a code when 2FA is on. This is the one
	// place the prompt is built, because four hand-rolled copies would drift.
	import { Modal, PillButton, TextField, toast } from '$lib/design-system';
	import { t } from '$lib/i18n';
	import { problemMessage, problemStatus, type StepUpConfirmation } from '$lib/api';
	import { session } from '$lib/stores/session.svelte';

	let {
		open = $bindable(false),
		title,
		intro,
		confirmLabel,
		variant = 'primary',
		onconfirm
	}: {
		open?: boolean;
		title: string;
		intro: string;
		confirmLabel: string;
		variant?: 'primary' | 'destructive';
		// Resolves when the action succeeded; throwing keeps the dialog open and
		// shows the message, so a mistyped password does not lose the context.
		onconfirm: (confirm: StepUpConfirmation) => Promise<void>;
	} = $props();

	let password = $state('');
	let totpCode = $state('');
	let busy = $state(false);
	let error = $state<string | undefined>(undefined);

	// The server only asks for a code when the account has 2FA on, so the field
	// follows the same condition rather than being always visible and optional.
	const needsCode = $derived(session.user?.totpEnabled === true);

	function reset() {
		password = '';
		totpCode = '';
		error = undefined;
	}

	export function close() {
		open = false;
		reset();
	}

	async function submit() {
		busy = true;
		error = undefined;
		try {
			await onconfirm({ password, totpCode: totpCode.trim() || undefined });
			close();
		} catch (e) {
			// 403 is the re-authentication itself failing — a wrong password or a
			// wrong code — and it belongs on the field the user just typed into.
			if (problemStatus(e) === 403) {
				error = t('stepUp.wrong');
			} else {
				// Anything else came from the action behind the dialog. Re-throwing
				// it here would become an unhandled rejection, because nothing
				// awaits an onclick handler; reporting it is the only way it
				// reaches the user at all.
				close();
				toast.error(problemMessage(e) ?? t('common.error'));
			}
		} finally {
			busy = false;
		}
	}
</script>

<Modal {open} {title} onclose={close}>
	<p class="intro">{intro}</p>
	<TextField
		label={t('stepUp.password')}
		type="password"
		autocomplete="current-password"
		bind:value={password}
		error={needsCode ? undefined : error}
	/>
	{#if needsCode}
		<TextField
			label={t('stepUp.code')}
			hint={t('stepUp.codeHint')}
			autocomplete="one-time-code"
			inputmode="numeric"
			bind:value={totpCode}
			{error}
		/>
	{/if}
	{#snippet footer()}
		<PillButton variant="ghost" onclick={close}>{t('common.cancel')}</PillButton>
		<PillButton {variant} loading={busy} disabled={!password} onclick={submit}>
			{confirmLabel}
		</PillButton>
	{/snippet}
</Modal>

<style>
	.intro {
		margin: 0 0 var(--space-4);
		color: var(--text-secondary);
		font-size: var(--text-sm);
		line-height: 1.5;
	}
</style>
