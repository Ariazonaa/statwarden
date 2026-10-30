import type { MessageKey } from '$lib/i18n';

/** Primary navigation model shared by the sidebar and command palette. */
export interface NavEntry {
	id: string;
	/** i18n key resolved at render time so the nav re-labels on locale switch. */
	labelKey: MessageKey;
	href: string;
	/** Restrict to admins. */
	adminOnly?: boolean;
	/** Inline SVG path data (24×24 viewBox, stroked). */
	icon: string;
}

export const navEntries: NavEntry[] = [
	{
		id: 'dashboard',
		labelKey: 'nav.dashboard',
		href: '/dashboard',
		icon: 'M4 13h6V4H4v9Zm0 7h6v-5H4v5Zm10 0h6V11h-6v9Zm0-16v5h6V4h-6Z'
	},
	{
		id: 'notifications',
		labelKey: 'nav.notifications',
		href: '/notifications',
		icon: 'M12 3a6 6 0 0 0-6 6c0 5-2 7-2 7h16s-2-2-2-7a6 6 0 0 0-6-6ZM10 20a2 2 0 0 0 4 0'
	},
	{
		id: 'tags',
		labelKey: 'nav.tags',
		href: '/tags',
		icon: 'M3 12V5a2 2 0 0 1 2-2h7l9 9-9 9-9-9Zm5-4h.01'
	},
	{
		id: 'maintenance',
		labelKey: 'nav.maintenance',
		href: '/maintenance',
		icon: 'M14.7 6.3a4 4 0 0 0-5.4 5.2l-6 6a1.8 1.8 0 0 0 2.5 2.5l6-6a4 4 0 0 0 5.2-5.4l-3 3-2-2 3-3Z'
	},
	{
		id: 'status-pages',
		labelKey: 'nav.statusPages',
		href: '/status-pages',
		icon: 'M4 5h16M4 12h16M4 19h10'
	},
	// adminOnly: GET /audit-log is requireRole(admin). Without the flag the entry
	// was offered to every role, and the page has no error path — a viewer who
	// clicked it got a 403 the load never caught and sat on skeleton rows.
	{
		id: 'activity',
		labelKey: 'nav.activity',
		href: '/activity',
		adminOnly: true,
		icon: 'M3 12h4l3-8 4 16 3-8h4'
	},
	{
		id: 'users',
		labelKey: 'nav.users',
		href: '/users',
		adminOnly: true,
		icon: 'M16 20v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2M9 10a3 3 0 1 0 0-6 3 3 0 0 0 0 6Zm13 10v-2a4 4 0 0 0-3-3.9M16 4.1a4 4 0 0 1 0 7.8'
	},
	{
		id: 'settings',
		labelKey: 'nav.settings',
		href: '/settings',
		adminOnly: true,
		icon: 'M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6Zm7.4-3a7.4 7.4 0 0 0-.1-1.3l2-1.6-2-3.4-2.4 1a7.3 7.3 0 0 0-2.2-1.3L14 2h-4l-.7 2.1A7.3 7.3 0 0 0 7.1 5.4l-2.4-1-2 3.4 2 1.6a7.4 7.4 0 0 0 0 2.6l-2 1.6 2 3.4 2.4-1a7.3 7.3 0 0 0 2.2 1.3L10 22h4l.7-2.1a7.3 7.3 0 0 0 2.2-1.3l2.4 1 2-3.4-2-1.6c.06-.43.1-.86.1-1.3Z'
	}
];
