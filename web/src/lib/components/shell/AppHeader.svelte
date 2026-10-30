<script lang="ts">
	// The app's top bar, laid out like Uptime Kuma's: brand on the left, the
	// three destinations an operator reaches for constantly (status pages, the
	// dashboard, maintenance) as links on the right, and everything else in the
	// profile menu. The command palette trigger sits between them — it is the
	// fastest route to any monitor and the one thing Kuma's header lacks.
	//
	// On a phone the links collapse into the profile menu and BottomNav takes
	// over the primary destinations.
	import { fly } from 'svelte/transition';
	import { page } from '$app/state';
	import { session } from '$lib/stores/session.svelte';
	import { navEntries } from './nav';
	import { t } from '$lib/i18n';
	import { Avatar, Kbd, safeDuration } from '$lib/design-system';

	let { onpalette }: { onpalette: () => void } = $props();

	/** The header links, in Kuma's order. */
	const primaryIds = ['status-pages', 'dashboard', 'maintenance'];
	const primary = $derived(
		primaryIds
			.map((id) => navEntries.find((e) => e.id === id))
			.filter((e): e is (typeof navEntries)[number] => e != null)
	);
	/** Everything else the sidebar used to carry, filtered by role. */
	const secondary = $derived(
		navEntries.filter((e) => !primaryIds.includes(e.id) && (!e.adminOnly || session.isAdmin))
	);

	function isActive(href: string): boolean {
		const path = page.url.pathname;
		if (href === '/dashboard') {
			// The dashboard owns the monitor area: list, detail, new and edit all
			// render inside its layout, so they light the same link.
			return path === href || path === '/list' || path.startsWith('/monitors');
		}
		return path === href || path.startsWith(href + '/');
	}

	let menuOpen = $state(false);

	function onDocPointer(e: PointerEvent) {
		if (menuOpen && !(e.target as HTMLElement).closest('.usermenu')) menuOpen = false;
	}
	function onKeydown(e: KeyboardEvent) {
		if (menuOpen && e.key === 'Escape') menuOpen = false;
	}
</script>

<svelte:document onpointerdown={onDocPointer} onkeydown={onKeydown} />

<header class="appheader">
	<div class="inner">
		<a class="brand ds-focusable" href="/dashboard">
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
			<span class="name">Statwarden</span>
		</a>

		<nav class="links" aria-label={t('nav.primaryLabel')}>
			{#each primary as entry (entry.id)}
				<a
					class="link ds-focusable"
					href={entry.href}
					aria-current={isActive(entry.href) ? 'page' : undefined}
				>
					<svg class="ico" viewBox="0 0 24 24" aria-hidden="true">
						<path
							d={entry.icon}
							fill="none"
							stroke="currentColor"
							stroke-width="1.8"
							stroke-linecap="round"
							stroke-linejoin="round"
						/>
					</svg>
					<span>{t(entry.labelKey)}</span>
				</a>
			{/each}
		</nav>

		<div class="right">
			<button class="search ds-focusable" onclick={onpalette} aria-label={t('topbar.search')}>
				<svg class="lens" viewBox="0 0 16 16" aria-hidden="true"
					><circle
						cx="6.75"
						cy="6.75"
						r="4.75"
						fill="none"
						stroke="currentColor"
						stroke-width="1.8"
					/>
					<path
						d="M10.5 10.5 14 14"
						stroke="currentColor"
						stroke-width="1.8"
						stroke-linecap="round"
					/>
				</svg>
				<span class="search-label" aria-hidden="true">{t('topbar.search')}</span>
				<Kbd>⌘K</Kbd>
			</button>

			<div class="usermenu">
				<button
					class="avatarbtn ds-focusable"
					aria-haspopup="menu"
					aria-expanded={menuOpen}
					aria-label={t('shell.profileMenu')}
					onclick={() => (menuOpen = !menuOpen)}
				>
					<Avatar name={session.user?.username ?? 'User'} size="sm" src={session.user?.avatarUrl} />
					<svg class="chev" viewBox="0 0 24 24" aria-hidden="true"
						><path
							d="m6 9 6 6 6-6"
							fill="none"
							stroke="currentColor"
							stroke-width="2"
							stroke-linecap="round"
						/></svg
					>
				</button>
				{#if menuOpen}
					<div class="dropdown" role="menu" transition:fly={{ y: -8, duration: safeDuration(180) }}>
						<div class="who">
							<strong>{session.user?.username}</strong>
							<span class="role">{session.user?.role}</span>
						</div>
						<!-- The header links, repeated for phones, where the header has
						     no room for them and BottomNav carries only the dashboard. -->
						{#each primary.filter((e) => e.id !== 'dashboard') as entry (entry.id)}
							<a
								class="mi phone-only ds-focusable"
								role="menuitem"
								href={entry.href}
								onclick={() => (menuOpen = false)}>{t(entry.labelKey)}</a
							>
						{/each}
						<!-- /profile first: a viewer has no settings entry, so this is the
						     only way they reach their own account. -->
						<a
							class="mi ds-focusable"
							role="menuitem"
							href="/profile"
							onclick={() => (menuOpen = false)}>{t('topbar.profile')}</a
						>
						{#each secondary as entry (entry.id)}
							<a
								class="mi ds-focusable"
								role="menuitem"
								href={entry.href}
								aria-current={isActive(entry.href) ? 'page' : undefined}
								onclick={() => (menuOpen = false)}>{t(entry.labelKey)}</a
							>
						{/each}
						<button
							class="mi danger ds-focusable"
							role="menuitem"
							onclick={() => {
								menuOpen = false;
								session.logout();
							}}>{t('topbar.signOut')}</button
						>
					</div>
				{/if}
			</div>
		</div>
	</div>
</header>

<style>
	.appheader {
		position: sticky;
		top: 0;
		z-index: 60;
		background: var(--glass-bg-thin);
		border-bottom: 1px solid var(--hairline);
	}
	.inner {
		display: flex;
		align-items: center;
		gap: var(--space-5);
		max-width: 120rem;
		margin: 0 auto;
		padding: var(--space-3) var(--space-7);
	}

	.brand {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		margin-right: auto;
		padding: var(--space-2);
		border-radius: var(--radius-squircle-sm);
		text-decoration: none;
		color: var(--color-text-primary);
	}
	.mark {
		display: inline-grid;
		place-items: center;
		width: 2rem;
		height: 2rem;
		border-radius: var(--radius-squircle-sm);
		color: var(--color-text-primary);
		background: var(--color-accent-tint);
		border: 1px solid var(--hairline-strong);
		flex-shrink: 0;
	}
	.mark svg {
		width: 1.25rem;
		height: 1.25rem;
	}
	.name {
		font-size: var(--text-title-3);
		font-weight: var(--weight-bold);
		letter-spacing: var(--tracking-tight);
	}

	.links {
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}
	.link {
		display: inline-flex;
		align-items: center;
		gap: var(--space-3);
		min-height: 2.75rem;
		padding: 0 var(--space-5);
		border-radius: var(--radius-pill);
		text-decoration: none;
		color: var(--color-text-secondary);
		font-size: var(--text-subheadline);
		font-weight: var(--weight-medium);
		white-space: nowrap;
	}
	.link:hover {
		color: var(--color-text-primary);
		background: var(--hairline);
	}
	.link[aria-current='page'] {
		color: var(--color-text-primary);
		background: var(--glass-bg-thick);
		box-shadow: inset 0 0 0 1px var(--hairline-strong);
	}
	.ico {
		width: 1.1rem;
		height: 1.1rem;
		flex-shrink: 0;
	}

	.right {
		display: flex;
		align-items: center;
		gap: var(--space-3);
	}
	.search {
		display: inline-flex;
		align-items: center;
		gap: var(--space-3);
		min-height: 2.75rem;
		padding: 0 var(--space-3) 0 var(--space-5);
		border: 1px solid var(--hairline);
		border-radius: var(--radius-pill);
		background: var(--glass-bg-regular);
		color: var(--color-text-secondary);
		cursor: pointer;
		font-family: inherit;
		font-size: var(--text-subheadline);
	}
	.search:hover {
		color: var(--color-text-primary);
		border-color: var(--hairline-strong);
	}
	.lens {
		width: 1rem;
		height: 1rem;
	}
	.search-label {
		min-width: 5rem;
		text-align: left;
	}

	.usermenu {
		position: relative;
	}
	.avatarbtn {
		display: inline-flex;
		align-items: center;
		gap: var(--space-2);
		min-height: 2.75rem;
		padding: var(--space-1) var(--space-3) var(--space-1) var(--space-1);
		border: 1px solid var(--hairline);
		border-radius: var(--radius-pill);
		background: transparent;
		color: var(--color-text-secondary);
		cursor: pointer;
	}
	.avatarbtn:hover {
		border-color: var(--hairline-strong);
		color: var(--color-text-primary);
	}
	.chev {
		width: 0.9rem;
		height: 0.9rem;
	}
	.dropdown {
		position: absolute;
		right: 0;
		top: calc(100% + var(--space-3));
		z-index: 70;
		min-width: 13rem;
		padding: var(--space-3);
		border-radius: var(--radius-squircle-md);
		background: var(--glass-bg-thick);
		border: 1px solid var(--hairline-strong);
		box-shadow: var(--shadow-lg, 0 18px 48px rgb(0 0 0 / 0.6));
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}
	.who {
		display: flex;
		flex-direction: column;
		padding: var(--space-2) var(--space-3) var(--space-3);
		border-bottom: 1px solid var(--hairline);
		margin-bottom: var(--space-2);
	}
	.who .role {
		font-size: var(--text-caption-1);
		color: var(--color-text-secondary);
		text-transform: capitalize;
	}
	.mi {
		display: block;
		text-align: left;
		padding: var(--space-3);
		border: 0;
		border-radius: var(--radius-squircle-sm);
		background: transparent;
		font-family: inherit;
		font-size: var(--text-subheadline);
		color: var(--color-text-primary);
		text-decoration: none;
		cursor: pointer;
	}
	.mi:hover,
	.mi[aria-current='page'] {
		background: var(--hairline);
	}
	.mi.danger {
		color: var(--color-danger);
	}
	.phone-only {
		display: none;
	}

	@media (max-width: 1023px) {
		.inner {
			padding: var(--space-3) var(--space-5);
		}
		.search-label,
		.search :global(kbd) {
			display: none;
		}
		.search {
			padding: 0 var(--space-4);
		}
	}
	@media (max-width: 767px) {
		.links {
			display: none;
		}
		.phone-only {
			display: block;
		}
		.inner {
			padding: var(--space-3) var(--space-4);
		}
	}
</style>
