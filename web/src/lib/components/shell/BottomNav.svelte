<script lang="ts">
	// Phone-only bottom navigation, as Uptime Kuma has it: dashboard, the
	// monitor list (a page of its own on a phone, where there is no room to
	// keep it beside the content), add, and settings. Hidden from 768px up,
	// where the header links and the persistent list cover the same ground.
	import { page } from '$app/state';
	import { session } from '$lib/stores/session.svelte';
	import { t, type MessageKey } from '$lib/i18n';

	interface Item {
		href: string;
		labelKey: MessageKey;
		icon: string;
		match: (path: string) => boolean;
	}

	const items = $derived.by<Item[]>(() => {
		const out: Item[] = [
			{
				href: '/dashboard',
				labelKey: 'nav.dashboard',
				icon: 'M4 13h6V4H4v9Zm0 7h6v-5H4v5Zm10 0h6V11h-6v9Zm0-16v5h6V4h-6Z',
				match: (p) => p === '/dashboard'
			},
			{
				href: '/list',
				labelKey: 'shell.list',
				icon: 'M8 6h12M8 12h12M8 18h12M4 6h.01M4 12h.01M4 18h.01',
				match: (p) => p === '/list' || (p.startsWith('/monitors/') && p !== '/monitors/new')
			}
		];
		if (session.canEdit) {
			out.push({
				href: '/monitors/new',
				labelKey: 'shell.add',
				icon: 'M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18Zm0 5v8m-4-4h8',
				match: (p) => p === '/monitors/new'
			});
		}
		// A viewer has no settings page; their own account is the equivalent.
		out.push(
			session.isAdmin
				? {
						href: '/settings',
						labelKey: 'nav.settings',
						icon: 'M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6Zm7.4-3a7.4 7.4 0 0 0-.1-1.2l2-1.6-2-3.4-2.4 1a7.5 7.5 0 0 0-2-1.2L14.5 3h-4l-.4 2.6a7.5 7.5 0 0 0-2 1.2l-2.4-1-2 3.4 2 1.6a7.4 7.4 0 0 0 0 2.4l-2 1.6 2 3.4 2.4-1a7.5 7.5 0 0 0 2 1.2l.4 2.6h4l.4-2.6a7.5 7.5 0 0 0 2-1.2l2.4 1 2-3.4-2-1.6c.1-.4.1-.8.1-1.2Z',
						match: (p) => p.startsWith('/settings')
					}
				: {
						href: '/profile',
						labelKey: 'topbar.profile',
						icon: 'M12 12a4 4 0 1 0 0-8 4 4 0 0 0 0 8Zm-7 9a7 7 0 0 1 14 0',
						match: (p) => p.startsWith('/profile')
					}
		);
		return out;
	});
</script>

<nav class="bottomnav" aria-label={t('shell.bottomNav')}>
	{#each items as item (item.href)}
		<a
			class="item ds-focusable"
			href={item.href}
			aria-current={item.match(page.url.pathname) ? 'page' : undefined}
		>
			<svg viewBox="0 0 24 24" aria-hidden="true">
				<path
					d={item.icon}
					fill="none"
					stroke="currentColor"
					stroke-width="1.8"
					stroke-linecap="round"
					stroke-linejoin="round"
				/>
			</svg>
			<span>{t(item.labelKey)}</span>
		</a>
	{/each}
</nav>

<style>
	.bottomnav {
		display: none;
	}
	@media (max-width: 767px) {
		.bottomnav {
			position: fixed;
			left: 0;
			right: 0;
			bottom: 0;
			z-index: 60;
			display: grid;
			grid-auto-flow: column;
			grid-auto-columns: minmax(0, 1fr);
			padding: var(--space-2) var(--space-2) calc(var(--space-3) + env(safe-area-inset-bottom));
			background: var(--glass-bg-thin);
			border-top: 1px solid var(--hairline);
		}
	}
	.item {
		display: flex;
		flex-direction: column;
		align-items: center;
		gap: var(--space-1);
		min-height: 3rem;
		justify-content: center;
		border-radius: var(--radius-squircle-sm);
		text-decoration: none;
		color: var(--color-text-secondary);
		font-size: var(--text-caption-2);
		font-weight: var(--weight-medium);
	}
	.item svg {
		width: 1.4rem;
		height: 1.4rem;
	}
	.item[aria-current='page'] {
		color: var(--color-accent);
		font-weight: var(--weight-semibold);
	}
</style>
