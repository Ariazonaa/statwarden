<script lang="ts">
	// One outgoing webhook feed: its target URL and its HMAC signing secret.
	//
	// The secret is write-only end to end — the server never returns it, only
	// whether one is set — so it can be seen exactly once: right after it is
	// generated here and before it is saved. That is why generating does not save
	// on its own; the operator copies the value into the receiver first.
	import { GlassCard, PillButton, TextField, toast } from '$lib/design-system';
	import { t } from '$lib/i18n';

	interface Props {
		title: string;
		subtitle?: string;
		urlLabel: string;
		urlHint: string;
		urlPlaceholder?: string;
		url: string;
		secretSet: boolean;
		/** Persists the URL; rejects with a problem the caller has already reported. */
		onsaveurl: (url: string) => Promise<void>;
		/** Persists the secret; '' switches signing off. */
		onsetsecret: (secret: string) => Promise<void>;
	}

	let {
		title,
		subtitle,
		urlLabel,
		urlHint,
		urlPlaceholder = 'https://',
		url = $bindable(),
		secretSet,
		onsaveurl,
		onsetsecret
	}: Props = $props();

	let savingUrl = $state(false);
	let savingSecret = $state(false);
	// Non-null while a new secret is being prepared (generated or pasted).
	let draft = $state<string | null>(null);

	const secretValid = $derived(
		draft !== null && draft.length >= 16 && draft.length <= 256 && !/[\s\x00-\x1f\x7f]/.test(draft)
	);

	/** 32 random bytes, base64url, behind a recognisable prefix: 49 characters. */
	function generate() {
		const bytes = crypto.getRandomValues(new Uint8Array(32));
		const b64 = btoa(String.fromCharCode(...bytes))
			.replace(/\+/g, '-')
			.replace(/\//g, '_')
			.replace(/=+$/, '');
		draft = `whsec_${b64}`;
	}

	async function copy() {
		if (!draft) return;
		try {
			await navigator.clipboard.writeText(draft);
			toast.success(t('settings.webhookCopied'));
		} catch {
			// Clipboard access can be refused (insecure origin, permissions); the
			// value is on screen and selectable either way.
			toast.error(t('settings.webhookCopyFailed'));
		}
	}

	async function saveUrl() {
		savingUrl = true;
		try {
			await onsaveurl(url.trim());
		} finally {
			savingUrl = false;
		}
	}

	async function saveSecret(value: string) {
		savingSecret = true;
		try {
			await onsetsecret(value);
			draft = null;
			toast.success(
				t(value === '' ? 'settings.webhookSecretRemoved' : 'settings.webhookSecretSaved')
			);
		} catch {
			// The caller reports the problem; keep the draft so nothing is lost.
		} finally {
			savingSecret = false;
		}
	}
</script>

<GlassCard {title} {subtitle}>
	<div class="field">
		<TextField label={urlLabel} bind:value={url} placeholder={urlPlaceholder} hint={urlHint} />
	</div>
	<div class="row end">
		<PillButton loading={savingUrl} onclick={saveUrl}>{t('settings.saveChanges')}</PillButton>
	</div>

	<div class="signing">
		<div class="state">
			<span class="label">{t('settings.webhookSigning')}</span>
			<span class="badge" class:on={secretSet}>
				{secretSet ? t('settings.webhookSigned') : t('settings.webhookUnsigned')}
			</span>
		</div>
		<p class="hint">{t('settings.webhookSignedHint')}</p>

		{#if draft !== null}
			<div class="draft">
				<TextField
					label={t('settings.webhookSecret')}
					bind:value={draft}
					hint={t('settings.webhookSecretHint')}
					error={draft.length > 0 && !secretValid ? t('settings.webhookSecretInvalid') : undefined}
					autocomplete="off"
					spellcheck={false}
				/>
				<div class="row">
					<PillButton variant="secondary" size="sm" onclick={copy}
						>{t('settings.webhookCopy')}</PillButton
					>
					<span class="spacer"></span>
					<PillButton variant="ghost" size="sm" onclick={() => (draft = null)}>
						{t('common.cancel')}
					</PillButton>
					<PillButton
						size="sm"
						loading={savingSecret}
						disabled={!secretValid}
						onclick={() => draft && saveSecret(draft)}
					>
						{t('settings.webhookSaveSecret')}
					</PillButton>
				</div>
			</div>
		{:else}
			<div class="row">
				<PillButton variant="secondary" size="sm" onclick={generate}>
					{secretSet ? t('settings.webhookRotate') : t('settings.webhookGenerate')}
				</PillButton>
				{#if secretSet}
					<PillButton
						variant="ghost"
						size="sm"
						loading={savingSecret}
						onclick={() => saveSecret('')}
					>
						{t('settings.webhookRemoveSecret')}
					</PillButton>
				{/if}
			</div>
		{/if}
	</div>
</GlassCard>

<style>
	.field {
		display: grid;
		gap: var(--space-2);
	}
	.row {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: var(--space-3);
	}
	.row.end {
		justify-content: flex-end;
		margin-top: var(--space-4);
	}
	.spacer {
		flex: 1;
	}
	.signing {
		margin-top: var(--space-5);
		padding-top: var(--space-5);
		border-top: 1px solid var(--hairline);
		display: grid;
		gap: var(--space-3);
	}
	.state {
		display: flex;
		align-items: center;
		gap: var(--space-3);
	}
	.label {
		font-size: var(--text-subheadline);
		font-weight: var(--weight-semibold);
	}
	.badge {
		font-size: var(--text-caption-2);
		padding: 1px var(--space-3);
		border-radius: var(--radius-pill);
		background: var(--status-paused-tint);
		color: var(--status-paused-text);
	}
	.badge.on {
		background: var(--status-up-tint);
		color: var(--status-up-text);
	}
	.hint {
		margin: 0;
		font-size: var(--text-caption-1);
		color: var(--color-text-tertiary);
		line-height: 1.45;
	}
	.draft {
		display: grid;
		gap: var(--space-3);
	}
</style>
