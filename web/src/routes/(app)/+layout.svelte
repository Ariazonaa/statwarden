<script lang="ts">
	// Authenticated app shell: route guard, live WS connection, the Kuma-style
	// header (plus a bottom nav on phones), connection-lost banner, command
	// palette and global keyboard shortcuts (n = new monitor, / = search,
	// ? = shortcuts help).
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { session } from '$lib/stores/session.svelte';
	import { monitors } from '$lib/stores/monitors.svelte';
	import { prefs } from '$lib/stores/prefs.svelte';
	import { ws } from '$lib/ws/store.svelte';
	import { navEntries } from '$lib/components/shell/nav';
	import { t } from '$lib/i18n';
	import AppHeader from '$lib/components/shell/AppHeader.svelte';
	import BottomNav from '$lib/components/shell/BottomNav.svelte';
	import ConnectionBanner from '$lib/components/shell/ConnectionBanner.svelte';
	import { CommandPalette, Kbd, Modal, theme, type Command } from '$lib/design-system';

	let { children } = $props();

	let ready = $state(false);
	let paletteOpen = $state(false);
	let helpOpen = $state(false);

	onMount(() => {
		let alive = true;
		(async () => {
			const ok = await session.guard();
			if (!ok || !alive) return;
			ws.connect();
			await monitors.load();
			ready = true;
		})();
		return () => {
			alive = false;
			ws.disconnect();
		};
	});

	const title = $derived.by(() => {
		const path = page.url.pathname;
		if (path.startsWith('/monitors/new')) return t('title.newMonitor');
		if (path.match(/\/monitors\/[^/]+\/edit/)) return t('title.editMonitor');
		if (path.startsWith('/monitors/')) {
			const id = path.split('/')[2];
			return monitors.get(id)?.name ?? t('title.monitor');
		}
		// /profile is reached from the user menu, not the sidebar, so it has no
		// navEntries row to borrow a label from.
		if (path.startsWith('/profile')) return t('title.profile');
		if (path === '/list') return t('shell.list');
		const entry = navEntries.find((e) => path.startsWith(e.href));
		return entry ? t(entry.labelKey) : t('title.dashboard');
	});

	/**
	 * How the page title renders. Under /monitors/ the pane beside the list
	 * renders its own h1 (the monitor's name, "New monitor", "Edit monitor"), so
	 * the shell adds none — a shell title there would sit above BOTH columns and
	 * push the list down. The dashboard and /list keep the h1 for assistive tech
	 * but hide it, as Kuma shows no title over its dashboard. Every other page
	 * shows it.
	 */
	const titleMode = $derived.by<'visible' | 'hidden' | 'none'>(() => {
		const path = page.url.pathname;
		if (path.startsWith('/monitors/')) return 'none';
		if (path === '/dashboard' || path === '/list') return 'hidden';
		return 'visible';
	});

	const commands = $derived.by<Command[]>(() => {
		const nav: Command[] = navEntries
			.filter((e) => !e.adminOnly || session.isAdmin)
			.map((e) => ({
				id: `nav-${e.id}`,
				label: t(e.labelKey),
				group: t('palette.groupNavigate'),
				action: () => goto(e.href)
			}));
		const actions: Command[] = [
			{
				id: 'new-monitor',
				label: t('palette.newMonitor'),
				group: t('palette.groupActions'),
				kbd: 'N',
				action: () => goto('/monitors/new')
			},
			{
				id: 'toggle-density',
				label: t('palette.toggleDensity'),
				group: t('palette.groupActions'),
				action: () => prefs.toggleDensity()
			},
			{
				id: 'sign-out',
				label: t('palette.signOut'),
				group: t('palette.groupActions'),
				action: () => session.logout()
			}
		];
		const mons: Command[] = monitors.items.map((m) => ({
			id: `mon-${m.id}`,
			label: m.name,
			group: t('palette.groupMonitors'),
			hint: t(`status.${m.status}`),
			action: () => goto(`/monitors/${m.id}`)
		}));
		return [...actions, ...nav, ...mons];
	});

	function onKeydown(e: KeyboardEvent) {
		const t = e.target as HTMLElement;
		const typing = /^(input|textarea|select)$/i.test(t.tagName) || t.isContentEditable;
		if (typing || e.metaKey || e.ctrlKey || e.altKey) return;
		if (e.key === 'n') {
			e.preventDefault();
			goto('/monitors/new');
		} else if (e.key === '/') {
			e.preventDefault();
			paletteOpen = true;
		} else if (e.key === '?') {
			e.preventDefault();
			helpOpen = true;
		}
	}
</script>

<svelte:window onkeydown={onKeydown} />

<CommandPalette
	{commands}
	bind:open={paletteOpen}
	placeholder={t('palette.placeholder')}
	emptyText={t('palette.empty')}
/>

{#if ready}
	<AppHeader onpalette={() => (paletteOpen = true)} />
	<div class="shell">
		<ConnectionBanner />
		{#if titleMode !== 'none'}
			<h1 class="pagetitle" class:hidden={titleMode === 'hidden'}>{title}</h1>
		{/if}
		{@render children()}
	</div>
	<BottomNav />
{:else}
	<div class="loading" aria-busy="true"><span class="spinner"></span></div>
{/if}

<Modal bind:open={helpOpen} title={t('shortcuts.title')}>
	<ul class="shortcuts">
		<li><Kbd>N</Kbd><span>{t('shortcuts.newMonitor')}</span></li>
		<li><Kbd>/</Kbd><span>{t('shortcuts.openPalette')}</span></li>
		<li>
			<span class="combo"><Kbd>⌘</Kbd><Kbd>K</Kbd></span><span>{t('shortcuts.commandPalette')}</span
			>
		</li>
		<li><Kbd>?</Kbd><span>{t('shortcuts.thisHelp')}</span></li>
	</ul>
</Modal>

<style>
	/* 120rem = a full 1920px screen. The old 90rem cap left a quarter of a
	   Full-HD display as black margin; pages that should stay narrow (forms)
	   cap themselves. */
	.shell {
		max-width: 120rem;
		margin: 0 auto;
		padding: var(--space-6) var(--space-7) var(--space-9);
	}
	.pagetitle {
		margin: 0 0 var(--space-6);
		font-size: var(--text-title-1);
		font-weight: var(--weight-bold);
		letter-spacing: var(--tracking-tight);
	}
	.pagetitle.hidden {
		position: absolute;
		width: 1px;
		height: 1px;
		padding: 0;
		margin: -1px;
		overflow: hidden;
		clip: rect(0, 0, 0, 0);
		white-space: nowrap;
		border: 0;
	}

	.loading {
		min-height: 100dvh;
		display: grid;
		place-items: center;
	}
	.spinner {
		width: 2rem;
		height: 2rem;
		border-radius: 50%;
		border: 3px solid var(--hairline-strong);
		border-top-color: var(--color-accent);
		animation: spin 0.8s linear infinite;
	}
	@keyframes spin {
		to {
			transform: rotate(360deg);
		}
	}

	.shortcuts {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}
	.shortcuts li {
		display: flex;
		align-items: center;
		gap: var(--space-4);
		font-size: var(--text-subheadline);
		color: var(--color-text-secondary);
	}
	.combo {
		display: inline-flex;
		gap: var(--space-2);
	}

	@media (max-width: 1023px) {
		.shell {
			padding: var(--space-5) var(--space-5) var(--space-9);
		}
	}
	/* Room for the fixed BottomNav, so the last row is never hidden under it. */
	@media (max-width: 767px) {
		.shell {
			padding: var(--space-4) var(--space-4) calc(5.5rem + env(safe-area-inset-bottom));
		}
	}
	@media (prefers-reduced-motion: reduce) {
		.spinner {
			animation-duration: 2s;
		}
	}
</style>
