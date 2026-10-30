<script lang="ts">
	// Account self-service: profile picture, sign-in address, password, 2FA and
	// linked SSO identities. Deliberately NOT admin-gated — /settings is, so
	// without this screen a viewer has no way to change their own password. Every
	// endpoint behind it resolves the user from the session, never from a path
	// parameter.
	import { onMount } from 'svelte';
	import {
		api,
		problemMessage,
		problemStatus,
		type OidcIdentity,
		type OidcProvider,
		type StepUpConfirmation,
		type TotpEnrollment
	} from '$lib/api';
	import RelativeTime from '$lib/components/RelativeTime.svelte';
	import QrCode from '$lib/components/QrCode.svelte';
	import StepUpDialog from '$lib/components/StepUpDialog.svelte';
	import SettingsShell, { type SettingsSection } from '$lib/components/shell/SettingsShell.svelte';
	import { session } from '$lib/stores/session.svelte';
	import { t } from '$lib/i18n';
	import {
		Avatar,
		GlassCard,
		ImageDropzone,
		Modal,
		PillButton,
		Skeleton,
		TextField,
		toast
	} from '$lib/design-system';

	/**
	 * The menu. "Linked sign-ins" only exists while an SSO provider is
	 * configured — the same condition its card had — so a plain password
	 * install does not show an empty section.
	 */
	const sections = $derived.by<SettingsSection[]>(() => {
		const out: SettingsSection[] = [
			{
				id: 'account',
				label: t('profile.account'),
				icon: 'M12 12a4 4 0 1 0 0-8 4 4 0 0 0 0 8Zm-7 9a7 7 0 0 1 14 0'
			},
			{
				id: 'security',
				label: t('settingsShell.security'),
				icon: 'M12 3 5 6v5c0 4.5 3 8.5 7 10 4-1.5 7-5.5 7-10V6l-7-3Zm-3 9 2 2 4-4'
			}
		];
		if (providers && providers.length > 0) {
			out.push({
				id: 'identities',
				label: t('profile.identities'),
				icon: 'M10 14a4 4 0 0 0 5.7 0l3-3a4 4 0 0 0-5.7-5.7l-1 1M14 10a4 4 0 0 0-5.7 0l-3 3a4 4 0 0 0 5.7 5.7l1-1'
			});
		}
		return out;
	});

	// --- avatar ---
	let avatarBusy = $state(false);
	let confirmAvatarRemove = $state(false);

	async function uploadAvatar(file: File) {
		avatarBusy = true;
		try {
			session.setUser(await api.uploadAvatar(file));
			toast.success(t('profile.avatarUploaded'));
		} catch (e) {
			// 413 and 415 are the server's own two rejections (size cap, and the
			// sniffed-type allowlist that also catches an SVG posing as a PNG).
			// Anything else — a 503 when the uploads directory is missing or not
			// writable, a 500 on a failed write — carries a server explanation that
			// is far more actionable than our generic string, so surface the problem
			// detail verbatim before falling back. Swallowing it (or masking it with
			// "Could not upload the picture") is exactly what made this bug opaque.
			const status = problemStatus(e);
			toast.error(
				status === 413
					? t('profile.avatarTooLarge')
					: status === 415
						? t('profile.avatarBadType')
						: (problemMessage(e) ?? t('profile.avatarError'))
			);
		} finally {
			avatarBusy = false;
		}
	}

	async function removeAvatar() {
		confirmAvatarRemove = false;
		avatarBusy = true;
		try {
			await api.deleteAvatar();
			if (session.user) session.setUser({ ...session.user, avatarUrl: undefined });
			toast.success(t('profile.avatarRemoved'));
		} catch {
			toast.error(t('profile.avatarError'));
		} finally {
			avatarBusy = false;
		}
	}

	// --- email ---
	// Changing the address needs the current password: it is the channel a
	// password reset is mailed to, so the server re-authenticates the change
	// rather than trusting the session alone (auth_handlers.go handleChangeEmail).
	let email = $state(session.user?.email ?? '');
	let emailPassword = $state('');
	let emailSaving = $state(false);
	let confirmEmail = $state(false);

	// The store loads asynchronously, so seed the field once it arrives — but
	// only while it is still pristine, or a slow /me would clobber typing.
	let emailTouched = $state(false);
	$effect(() => {
		if (!emailTouched && session.user) email = session.user.email;
	});

	function askEmail() {
		const next = email.trim();
		if (!next || !next.includes('@')) {
			toast.error(t('profile.emailInvalid'));
			return;
		}
		if (next === session.user?.email) {
			toast.info(t('profile.emailUnchanged'));
			return;
		}
		if (!emailPassword) {
			toast.error(t('profile.emailPasswordRequired'));
			return;
		}
		confirmEmail = true;
	}

	async function saveEmail() {
		confirmEmail = false;
		emailSaving = true;
		try {
			session.setUser(
				await api.changeEmail({ email: email.trim(), currentPassword: emailPassword })
			);
			emailPassword = '';
			emailTouched = false;
			toast.success(t('profile.emailChanged'));
		} catch (e) {
			// 403 is specifically "current password is wrong" and 409 is an SSO
			// account with no local password to check — both need their own words,
			// the same way savePassword spells out its 403.
			const status = problemStatus(e);
			toast.error(
				status === 403
					? t('profile.emailWrongPassword')
					: status === 409
						? t('profile.emailNoPassword')
						: (problemMessage(e) ?? t('profile.emailError'))
			);
		} finally {
			emailSaving = false;
		}
	}

	// --- password ---
	let currentPassword = $state('');
	let newPassword = $state('');
	let confirmPassword = $state('');
	let passwordSaving = $state(false);

	async function savePassword() {
		if (newPassword.length < 8) {
			toast.error(t('profile.passwordTooShort'));
			return;
		}
		if (newPassword !== confirmPassword) {
			toast.error(t('profile.passwordMismatch'));
			return;
		}
		passwordSaving = true;
		try {
			await api.changePassword({ currentPassword, newPassword });
			currentPassword = '';
			newPassword = '';
			confirmPassword = '';
			// The server revokes every other session as part of the change, so say
			// so: the current session survives and nothing else is visible.
			toast.success(t('profile.passwordChanged'));
		} catch (e) {
			// 403 is specifically "current password is wrong"; a 400 carries the
			// server's own policy message, which is more useful than ours.
			const status = problemStatus(e);
			toast.error(
				status === 403
					? t('profile.passwordWrongCurrent')
					: (problemMessage(e) ?? t('profile.passwordError'))
			);
		} finally {
			passwordSaving = false;
		}
	}

	// --- two-factor (TOTP) ---
	// Enrolment is two legs with an authenticator app in between: enroll mints a
	// pending secret (2FA still off), verify proves a current code and flips it
	// on, answering the one-time backup codes we then show once.
	let enrollment = $state<TotpEnrollment | undefined>(undefined);
	let enrollOpen = $state(false);
	let enrollStarting = $state(false);
	let twofaCode = $state('');
	let twofaVerifying = $state(false);
	let twofaCodeError = $state<string | undefined>(undefined);

	let backupCodes = $state<string[] | undefined>(undefined);
	let backupCodesOpen = $state(false);

	let disableOpen = $state(false);
	let disabling = $state(false);

	let regenOpen = $state(false);
	let regenerating = $state(false);

	// Enrolling attaches an authenticator to the account, so the server
	// re-authenticates first: a borrowed session must not be able to add a
	// second factor the owner does not hold — which, since disabling 2FA needs
	// the password, would lock the owner out with their own account.
	let enrollConfirmOpen = $state(false);

	async function startTotpEnroll(confirm: StepUpConfirmation) {
		enrollStarting = true;
		try {
			enrollment = await api.enrollTotp(confirm);
			twofaCode = '';
			twofaCodeError = undefined;
			enrollOpen = true;
		} catch (e) {
			// A 403 is the re-authentication itself failing, and it belongs on
			// the dialog's own error line where the fields are. Catching it here
			// closed the dialog and replaced "that did not match" with a generic
			// toast, so a mistyped password lost everything already entered.
			if (problemStatus(e) === 403) throw e;
			toast.error(problemMessage(e) ?? t('profile.twofaEnrollError'));
		} finally {
			enrollStarting = false;
		}
	}

	async function verifyTotpEnroll() {
		const code = twofaCode.trim();
		// Cheap client-side gate on shape; the server is the real judge and its
		// 400 lands on the same error line below.
		if (!/^\d{6}$/.test(code)) {
			twofaCodeError = t('profile.twofaCodeInvalid');
			return;
		}
		twofaVerifying = true;
		twofaCodeError = undefined;
		try {
			const codes = await api.verifyTotp(code);
			if (session.user) session.setUser({ ...session.user, totpEnabled: true });
			enrollOpen = false;
			enrollment = undefined;
			backupCodes = codes;
			backupCodesOpen = true;
			toast.success(t('profile.twofaEnabledToast'));
		} catch (e) {
			twofaCodeError =
				problemStatus(e) === 400
					? t('profile.twofaCodeInvalid')
					: (problemMessage(e) ?? t('profile.twofaEnrollError'));
		} finally {
			twofaVerifying = false;
		}
	}

	async function disableTotp(confirm: StepUpConfirmation) {
		disabling = true;
		try {
			await api.disableTotp(confirm);
			if (session.user) session.setUser({ ...session.user, totpEnabled: false });
			toast.success(t('profile.twofaDisabledToast'));
		} catch (e) {
			if (problemStatus(e) === 403) throw e; // shown on the dialog, see startTotpEnroll
			toast.error(problemMessage(e) ?? t('profile.twofaDisableError'));
		} finally {
			disabling = false;
		}
	}

	async function regenerateCodes(confirm: StepUpConfirmation) {
		regenerating = true;
		try {
			const codes = await api.regenerateBackupCodes(confirm);
			regenOpen = false;
			backupCodes = codes;
			backupCodesOpen = true;
			toast.success(t('profile.backupCodesRegenerated'));
		} catch (e) {
			if (problemStatus(e) === 403) throw e; // shown on the dialog, see startTotpEnroll
			toast.error(problemMessage(e) ?? t('profile.backupCodesError'));
		} finally {
			regenerating = false;
		}
	}

	async function copyBackupCodes() {
		try {
			await navigator.clipboard.writeText((backupCodes ?? []).join('\n'));
			toast.success(t('profile.backupCodesCopied'));
		} catch {
			// A clipboard the browser refuses (insecure context, denied permission)
			// is not an error worth a toast — the codes are on screen to copy by hand.
		}
	}

	// --- single sign-on identities (OIDC) ---
	// The list/unlink routes are only mounted when the operator configured OIDC;
	// on an instance without SSO they 404, which we treat as "no linked accounts".
	// The whole section is hidden unless at least one provider is configured.
	let providers = $state<OidcProvider[] | undefined>(undefined);
	let identities = $state<OidcIdentity[] | undefined>(undefined);
	let linkingProvider = $state<string | undefined>(undefined);
	let confirmUnlink = $state<OidcIdentity | undefined>(undefined);
	let unlinking = $state(false);

	function providerLabel(providerId: string): string {
		return providers?.find((p) => p.id === providerId)?.displayName ?? providerId;
	}

	// Linking adds a second way in that the owner never sees credentials for and
	// that no password change revokes, so it re-authenticates too.
	let linkConfirmFor = $state<string | undefined>(undefined);

	async function linkProvider(providerId: string, confirm: StepUpConfirmation) {
		linkingProvider = providerId;
		try {
			const { authorizeUrl } = await api.linkOidc(providerId, confirm);
			// Top-level navigation: the OIDC handshake needs the browser at the
			// provider, not an XHR. We do not clear linkingProvider — the page is
			// leaving.
			window.location.href = authorizeUrl;
		} catch (e) {
			linkingProvider = undefined;
			if (problemStatus(e) === 403) throw e; // shown on the dialog, see startTotpEnroll
			toast.error(problemMessage(e) ?? t('profile.identityLinkError'));
		}
	}

	async function unlinkIdentity(identity: OidcIdentity) {
		unlinking = true;
		try {
			await api.unlinkIdentity(identity.id);
			identities = (identities ?? []).filter((x) => x.id !== identity.id);
			confirmUnlink = undefined;
			toast.success(t('profile.identityUnlinked'));
		} catch (e) {
			// 409 is the server refusing to strip the last sign-in method from a
			// passwordless account — a distinct, actionable message.
			toast.error(
				problemStatus(e) === 409
					? t('profile.identityUnlinkLast')
					: (problemMessage(e) ?? t('profile.identityUnlinkError'))
			);
		} finally {
			unlinking = false;
		}
	}

	onMount(async () => {
		// Providers and identities are independent; a 404 from either (SSO off, or a
		// pre-SSO server) collapses to an empty list rather than a broken section.
		const [pv, ids] = await Promise.all([
			api.oidcProviders().catch(() => [] as OidcProvider[]),
			api.listIdentities().catch(() => [] as OidcIdentity[])
		]);
		providers = pv;
		identities = ids;
	});
</script>

<svelte:head><title>{t('profile.pageTitle')}</title></svelte:head>

<SettingsShell {sections}>
	{#snippet children(current: string)}
		{#if current === 'account'}
			<!-- Who this is, as Kuma's settings open with the account it belongs to. -->
			<div class="who">
				<Avatar name={session.user?.username ?? 'User'} src={session.user?.avatarUrl} size="lg" />
				<div>
					<strong>{session.user?.username}</strong>
					<span class="role">{session.user?.role}</span>
				</div>
			</div>
			<p class="lead">{t('profile.lead')}</p>
			<GlassCard subtitle={t('profile.accountSub')}>
				<TextField
					label={t('profile.usernameField')}
					value={session.user?.username ?? ''}
					autocomplete="username"
					readonly
				/>
			</GlassCard>
			<GlassCard title={t('profile.email')} subtitle={t('profile.emailSub')}>
				<div class="form">
					<TextField
						label={t('profile.emailField')}
						type="email"
						autocomplete="email"
						bind:value={email}
						oninput={() => (emailTouched = true)}
					/>
					<TextField
						label={t('profile.currentPassword')}
						type="password"
						autocomplete="current-password"
						hint={t('profile.emailPasswordRequired')}
						bind:value={emailPassword}
					/>
				</div>
				<div class="save">
					<PillButton loading={emailSaving} disabled={!emailPassword} onclick={askEmail}>
						{t('common.saveChanges')}
					</PillButton>
				</div>
			</GlassCard>
			<GlassCard title={t('profile.avatar')} subtitle={t('profile.avatarSub')}>
				<ImageDropzone
					label={t('profile.avatarField')}
					hint={t('profile.avatarHint')}
					src={session.user?.avatarUrl}
					busy={avatarBusy}
					promptText={t('profile.avatarDropPrompt')}
					chooseLabel={t('profile.avatarChoose')}
					removeLabel={t('profile.avatarRemove')}
					previewAlt={t('profile.avatarPreviewAlt')}
					onselect={uploadAvatar}
					onremove={() => (confirmAvatarRemove = true)}
				/>
			</GlassCard>
		{:else if current === 'security'}
			<GlassCard title={t('profile.password')} subtitle={t('profile.passwordSub')}>
				<div class="form">
					<TextField
						label={t('profile.currentPassword')}
						type="password"
						autocomplete="current-password"
						bind:value={currentPassword}
					/>
					<div class="grid">
						<TextField
							label={t('profile.newPassword')}
							type="password"
							autocomplete="new-password"
							bind:value={newPassword}
						/>
						<TextField
							label={t('profile.confirmPassword')}
							type="password"
							autocomplete="new-password"
							error={confirmPassword && newPassword !== confirmPassword
								? t('profile.passwordMismatch')
								: undefined}
							bind:value={confirmPassword}
						/>
					</div>
				</div>
				<div class="save">
					<PillButton
						loading={passwordSaving}
						disabled={!currentPassword || !newPassword || !confirmPassword}
						onclick={savePassword}>{t('profile.changePassword')}</PillButton
					>
				</div>
			</GlassCard>
			<GlassCard title={t('profile.twofa')} subtitle={t('profile.twofaSub')}>
				{#if session.user?.totpEnabled}
					<p class="status-on">{t('profile.twofaOn')}</p>
					<div class="actions-row">
						<PillButton size="sm" onclick={() => (regenOpen = true)}>
							{t('profile.backupCodesRegenerate')}
						</PillButton>
						<PillButton variant="destructive" size="sm" onclick={() => (disableOpen = true)}>
							{t('profile.twofaDisable')}
						</PillButton>
					</div>
				{:else}
					<p class="muted">{t('profile.twofaOff')}</p>
					<div class="save">
						<PillButton loading={enrollStarting} onclick={() => (enrollConfirmOpen = true)}>
							{t('profile.twofaEnable')}
						</PillButton>
					</div>
				{/if}
			</GlassCard>
		{:else if current === 'identities' && providers && providers.length > 0}
			<GlassCard subtitle={t('profile.identitiesSub')}>
				{#if !identities}
					<Skeleton lines={2} />
				{:else if identities.length === 0}
					<p class="muted">{t('profile.identitiesNone')}</p>
				{:else}
					<ul class="rows">
						{#each identities as idn (idn.id)}
							<li>
								<div>
									<strong>{idn.displayName || providerLabel(idn.provider)}</strong>
									<span class="sub">
										{providerLabel(idn.provider)} · {idn.subject}{#if idn.email}
											· {idn.email}{/if}
									</span>
									<span class="sub">
										{t('profile.identityLinkedOn')}
										<RelativeTime value={idn.createdAt} />
										·
										{#if idn.lastLoginAt}
											{t('profile.identityLastLogin')} <RelativeTime value={idn.lastLoginAt} />
										{:else}
											{t('profile.identityNeverUsed')}
										{/if}
									</span>
								</div>
								<PillButton variant="ghost" size="sm" onclick={() => (confirmUnlink = idn)}>
									{t('profile.identityUnlink')}
								</PillButton>
							</li>
						{/each}
					</ul>
				{/if}
				<div class="link-row">
					{#each providers as pv (pv.id)}
						<PillButton
							size="sm"
							loading={linkingProvider === pv.id}
							onclick={() => (linkConfirmFor = pv.id)}
						>
							{t('profile.identityLink', { provider: pv.displayName })}
						</PillButton>
					{/each}
				</div>
			</GlassCard>
		{/if}
	{/snippet}
</SettingsShell>

<Modal
	open={confirmAvatarRemove}
	title={t('profile.avatarRemoveTitle')}
	onclose={() => (confirmAvatarRemove = false)}
>
	<p style="margin:0">{t('profile.avatarRemoveBody')}</p>
	{#snippet footer()}
		<PillButton variant="ghost" onclick={() => (confirmAvatarRemove = false)}>
			{t('common.cancel')}
		</PillButton>
		<PillButton variant="destructive" onclick={removeAvatar}>{t('common.remove')}</PillButton>
	{/snippet}
</Modal>

<Modal
	open={confirmEmail}
	title={t('profile.emailConfirmTitle')}
	onclose={() => (confirmEmail = false)}
>
	<p style="margin:0">{t('profile.emailConfirmBody', { email: email.trim() })}</p>
	{#snippet footer()}
		<PillButton variant="ghost" onclick={() => (confirmEmail = false)}>
			{t('common.cancel')}
		</PillButton>
		<PillButton onclick={saveEmail}>{t('common.saveChanges')}</PillButton>
	{/snippet}
</Modal>

<Modal open={enrollOpen} title={t('profile.twofaEnrollTitle')} onclose={() => (enrollOpen = false)}>
	<p class="modalintro">{t('profile.twofaEnrollScan')}</p>
	{#if enrollment}
		<div class="qr-wrap">
			<QrCode value={enrollment.otpauthUri} alt={t('profile.twofaEnrollScan')} />
		</div>
		<div class="secret-block">
			<span class="field-label">{t('profile.twofaSecret')}</span>
			<code class="secret">{enrollment.secret}</code>
			<a class="otpauth" href={enrollment.otpauthUri}>{enrollment.otpauthUri}</a>
		</div>
	{/if}
	<TextField
		label={t('profile.twofaCode')}
		placeholder={t('profile.twofaCodePlaceholder')}
		hint={t('profile.twofaCodeHint')}
		error={twofaCodeError}
		inputmode="numeric"
		autocomplete="one-time-code"
		maxlength={6}
		bind:value={twofaCode}
	/>
	{#snippet footer()}
		<PillButton variant="ghost" onclick={() => (enrollOpen = false)}>
			{t('common.cancel')}
		</PillButton>
		<PillButton loading={twofaVerifying} onclick={verifyTotpEnroll}>
			{t('profile.twofaVerify')}
		</PillButton>
	{/snippet}
</Modal>

<Modal
	open={backupCodesOpen}
	title={t('profile.backupCodes')}
	onclose={() => (backupCodesOpen = false)}
>
	<p class="modalintro">{t('profile.backupCodesIntro')}</p>
	<p class="warn">{t('profile.backupCodesShownOnce')}</p>
	{#if backupCodes}
		<ul class="codes">
			{#each backupCodes as code (code)}
				<li>{code}</li>
			{/each}
		</ul>
	{/if}
	{#snippet footer()}
		<PillButton variant="ghost" onclick={copyBackupCodes}>
			{t('profile.backupCodesCopy')}
		</PillButton>
		<PillButton onclick={() => (backupCodesOpen = false)}>
			{t('profile.backupCodesDone')}
		</PillButton>
	{/snippet}
</Modal>

<StepUpDialog
	bind:open={disableOpen}
	title={t('profile.twofaDisableTitle')}
	intro={t('profile.twofaDisableBody')}
	confirmLabel={t('profile.twofaDisable')}
	variant="destructive"
	onconfirm={disableTotp}
/>

<StepUpDialog
	bind:open={regenOpen}
	title={t('profile.backupCodesRegenerateTitle')}
	intro={t('profile.backupCodesRegenerateBody')}
	confirmLabel={t('profile.backupCodesRegenerate')}
	onconfirm={regenerateCodes}
/>

<StepUpDialog
	bind:open={enrollConfirmOpen}
	title={t('profile.twofaEnable')}
	intro={t('profile.twofaEnrollConfirmBody')}
	confirmLabel={t('common.continue')}
	onconfirm={startTotpEnroll}
/>

<StepUpDialog
	open={linkConfirmFor != null}
	title={t('profile.identityLinkTitle')}
	intro={t('profile.identityLinkConfirmBody')}
	confirmLabel={t('common.continue')}
	onconfirm={async (c) => {
		const pid = linkConfirmFor;
		linkConfirmFor = undefined;
		if (pid) await linkProvider(pid, c);
	}}
/>

<Modal
	open={confirmUnlink != null}
	title={t('profile.identityUnlinkTitle')}
	onclose={() => (confirmUnlink = undefined)}
>
	<p style="margin:0">
		{t('profile.identityUnlinkBody', {
			provider: confirmUnlink ? providerLabel(confirmUnlink.provider) : ''
		})}
	</p>
	{#if identities?.length === 1}
		<p class="warn">{t('profile.identityUnlinkLast')}</p>
	{/if}
	{#snippet footer()}
		<PillButton variant="ghost" onclick={() => (confirmUnlink = undefined)}>
			{t('common.cancel')}
		</PillButton>
		<PillButton
			variant="destructive"
			loading={unlinking}
			onclick={() => confirmUnlink && unlinkIdentity(confirmUnlink)}
		>
			{t('profile.identityUnlink')}
		</PillButton>
	{/snippet}
</Modal>

<style>
	.who {
		display: flex;
		align-items: center;
		gap: var(--space-5);
		margin-bottom: var(--space-4);
	}
	.who strong {
		display: block;
		font-size: var(--text-title-3);
		font-weight: var(--weight-bold);
	}
	.who .role {
		font-size: var(--text-footnote);
		color: var(--color-text-secondary);
		text-transform: capitalize;
	}
	.lead {
		margin: 0 0 var(--space-5);
		color: var(--color-text-secondary);
		font-size: var(--text-subheadline);
	}
	.form {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}
	.grid {
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: var(--space-4);
	}
	.save {
		margin-top: var(--space-5);
		display: flex;
		justify-content: flex-end;
	}
	.actions-row {
		margin-top: var(--space-4);
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-3);
		justify-content: flex-end;
	}
	.link-row {
		margin-top: var(--space-5);
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-3);
	}
	.rows {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}
	.rows li {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-4);
		padding: var(--space-3) 0;
		border-bottom: 1px solid var(--hairline);
	}
	.rows li:last-child {
		border-bottom: 0;
	}
	.sub {
		display: block;
		font-size: var(--text-caption-1);
		color: var(--color-text-tertiary);
		margin-top: 2px;
	}
	.muted {
		margin: 0;
		color: var(--color-text-tertiary);
		font-size: var(--text-subheadline);
	}
	.status-on {
		margin: 0;
		color: var(--color-text-primary);
		font-size: var(--text-subheadline);
		font-weight: var(--weight-medium);
	}
	.modalintro {
		margin: 0 0 var(--space-4);
		font-size: var(--text-subheadline);
		color: var(--color-text-secondary);
	}
	.warn {
		margin: var(--space-4) 0 0;
		font-size: var(--text-caption-1);
		color: var(--color-danger);
		font-weight: var(--weight-medium);
	}
	.qr-wrap {
		display: flex;
		justify-content: center;
		margin-bottom: var(--space-4);
	}
	.secret-block {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		margin-bottom: var(--space-4);
		padding: var(--space-4);
		border: 1px solid var(--hairline);
		border-radius: var(--radius-squircle-sm);
		background: var(--glass-bg-thin);
	}
	.field-label {
		font-size: var(--text-footnote);
		font-weight: var(--weight-medium);
		color: var(--color-text-secondary);
	}
	.secret {
		font-family: var(--font-mono, monospace);
		font-size: var(--text-body);
		letter-spacing: 0.12em;
		color: var(--color-text-primary);
		user-select: all;
		word-break: break-all;
	}
	.otpauth {
		font-size: var(--text-caption-1);
		color: var(--color-text-tertiary);
		word-break: break-all;
	}
	.codes {
		list-style: none;
		margin: 0;
		padding: var(--space-4);
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: var(--space-2) var(--space-4);
		border: 1px solid var(--hairline);
		border-radius: var(--radius-squircle-sm);
		background: var(--glass-bg-thin);
	}
	.codes li {
		font-family: var(--font-mono, monospace);
		font-size: var(--text-subheadline);
		letter-spacing: 0.08em;
		color: var(--color-text-primary);
		user-select: all;
	}
	@media (max-width: 640px) {
		.grid {
			grid-template-columns: 1fr;
		}
		.codes {
			grid-template-columns: 1fr;
		}
	}
</style>
