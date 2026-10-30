<script lang="ts" module>
	export interface SettingsSection {
		id: string;
		label: string;
		/** Inline SVG path data (24×24 viewBox, stroked). */
		icon: string;
	}
</script>

<script lang="ts">
	// The Uptime Kuma settings frame: one box, a section menu on the left, the
	// chosen section on the right. Used by /settings and /profile so both read
	// as one place instead of a long stack of unrelated cards.
	//
	// The section lives in the URL (`?s=<id>`), not in component state, so a
	// section can be linked, reloaded and reached with the back button — the
	// same reason Kuma gives every settings page a route of its own.
	//
	// Inside the pane the cards a page already renders lose their own box and
	// become plain sections divided by a hairline, which is what turns a stack
	// of cards into one settings page. They keep their headings and actions.
	import type { Snippet } from 'svelte';
	import { page } from '$app/state';
	import { t } from '$lib/i18n';

	let {
		sections,
		children
	}: {
		sections: SettingsSection[];
		/** Rendered with the active section id; render only that section. */
		children: Snippet<[string]>;
	} = $props();

	const current = $derived.by(() => {
		const wanted = page.url.searchParams.get('s');
		return sections.find((s) => s.id === wanted)?.id ?? sections[0]?.id ?? '';
	});
	const currentLabel = $derived(sections.find((s) => s.id === current)?.label ?? '');

	// On a phone the menu is a horizontal strip; keep the active entry in view
	// rather than leaving it clipped at the edge after a reload or a deep link.
	let nav = $state<HTMLElement | undefined>(undefined);
	$effect(() => {
		void current;
		nav
			?.querySelector('[aria-current="page"]')
			?.scrollIntoView({ block: 'nearest', inline: 'nearest' });
	});
</script>

<div class="sshell">
	<nav class="snav" bind:this={nav} aria-label={t('settingsShell.sections')}>
		{#each sections as s (s.id)}
			<a
				class="sitem ds-focusable"
				href="?s={s.id}"
				aria-current={s.id === current ? 'page' : undefined}
				data-sveltekit-noscroll
			>
				<svg viewBox="0 0 24 24" aria-hidden="true">
					<path
						d={s.icon}
						fill="none"
						stroke="currentColor"
						stroke-width="1.8"
						stroke-linecap="round"
						stroke-linejoin="round"
					/>
				</svg>
				<span>{s.label}</span>
			</a>
		{/each}
	</nav>
	<section class="spane" aria-labelledby="sshell-title">
		<h2 id="sshell-title" class="stitle">{currentLabel}</h2>
		<div class="sbody">
			{@render children(current)}
		</div>
	</section>
</div>

<style>
	.sshell {
		display: grid;
		grid-template-columns: 15rem minmax(0, 1fr);
		min-height: 32rem;
		border: 1px solid var(--hairline);
		border-radius: var(--radius-squircle-md);
		background: var(--glass-bg-thin);
		overflow: hidden;
	}

	.snav {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
		padding: var(--space-5) var(--space-4);
		border-right: 1px solid var(--hairline);
	}
	.sitem {
		display: flex;
		align-items: center;
		gap: var(--space-4);
		min-height: 2.75rem;
		padding: 0 var(--space-4);
		border-radius: var(--radius-squircle-sm);
		color: var(--color-text-secondary);
		font-size: var(--text-subheadline);
		font-weight: var(--weight-medium);
		text-decoration: none;
		white-space: nowrap;
	}
	.sitem svg {
		width: 1.15rem;
		height: 1.15rem;
		flex-shrink: 0;
	}
	.sitem:hover {
		color: var(--color-text-primary);
		background: var(--hairline);
	}
	.sitem[aria-current='page'] {
		color: var(--color-text-primary);
		background: var(--glass-bg-thick);
		box-shadow: inset 3px 0 0 var(--color-accent);
		font-weight: var(--weight-semibold);
	}

	.spane {
		min-width: 0;
		padding: var(--space-7) var(--space-8) var(--space-8);
	}
	.stitle {
		margin: 0 0 var(--space-5);
		font-size: var(--text-title-2);
		font-weight: var(--weight-bold);
		letter-spacing: var(--tracking-tight);
	}
	.sbody {
		max-width: 52rem;
	}

	/* The page's cards become sections of this one box: no fill, no border, no
	   shadow, a hairline between them. Anchored to .sbody so nothing leaks. */
	.sbody :global(.panel) {
		background: transparent;
		border: 0;
		box-shadow: none;
		border-radius: 0;
		padding-left: 0;
		padding-right: 0;
	}
	.sbody :global(.panel + .panel) {
		border-top: 1px solid var(--hairline);
	}
	.sbody :global(.panel:first-child) {
		padding-top: 0;
	}
	/* Kuma puts the save button under the form, flush left. */
	.sbody :global(.save) {
		justify-content: flex-start;
	}

	@media (max-width: 899px) {
		.sshell {
			grid-template-columns: minmax(0, 1fr);
			min-height: 0;
		}
		.snav {
			flex-direction: row;
			overflow-x: auto;
			scrollbar-width: none;
			padding: var(--space-3);
			border-right: 0;
			border-bottom: 1px solid var(--hairline);
		}
		.sitem[aria-current='page'] {
			box-shadow: inset 0 -3px 0 var(--color-accent);
		}
		.spane {
			padding: var(--space-6) var(--space-5);
		}
	}
</style>
