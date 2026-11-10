/**
 * A session that ends mid-use must sign the tab out, not leave a dashboard that
 * still looks signed in while every request behind it fails.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { User } from '$lib/api';

const goto = vi.fn();
vi.mock('$app/navigation', () => ({ goto: (...a: unknown[]) => goto(...a) }));

const { session } = await import('./session.svelte');
const { reportUnauthorized } = await import('$lib/api/client');

const ada: User = {
	id: '1',
	username: 'ada',
	email: 'ada@example.test',
	role: 'admin',
	totpEnabled: false,
	disabled: false,
	createdAt: '2026-01-01T00:00:00Z'
};

beforeEach(() => {
	goto.mockClear();
	session.user = undefined;
});

describe('session expiry', () => {
	it('clears the user and goes to the login screen when the server rejects the session', () => {
		session.setUser(ada);
		reportUnauthorized();
		expect(session.user).toBeUndefined();
		expect(goto).toHaveBeenCalledWith('/login');
	});

	it('does nothing for a visitor who was never signed in', () => {
		// A public page probing /me while logged out is not an expiry; bouncing
		// it to /login would break every status page.
		reportUnauthorized();
		expect(goto).not.toHaveBeenCalled();
	});
});
