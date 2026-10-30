/**
 * Session / "me" store + auth guard. Loads the current user once, exposes
 * reactive auth state, and drives redirects (unauthenticated → /login,
 * first-run → /setup).
 */
import { goto } from '$app/navigation';
import { api, ApiError, type User } from '$lib/api';

class SessionStore {
	user = $state<User | undefined>(undefined);
	loading = $state(true);
	needsSetup = $state(false);

	get authenticated() {
		return this.user != null;
	}
	get isAdmin() {
		return this.user?.role === 'admin';
	}
	get canEdit() {
		return this.user?.role === 'admin' || this.user?.role === 'user';
	}

	/** Resolve current auth state. Safe to call repeatedly. */
	async load(): Promise<void> {
		this.loading = true;
		try {
			const { needsSetup } = await api.setupState();
			this.needsSetup = needsSetup;
			if (needsSetup) {
				this.user = undefined;
				return;
			}
			this.user = await api.me();
		} catch (err) {
			if (err instanceof ApiError && err.status === 401) {
				this.user = undefined;
			} else {
				// Network/other error: treat as logged out but don't crash.
				this.user = undefined;
			}
		} finally {
			this.loading = false;
		}
	}

	setUser(user: User) {
		this.user = user;
		this.needsSetup = false;
	}

	async logout(): Promise<void> {
		try {
			await api.logout();
		} finally {
			this.user = undefined;
			await goto('/login');
		}
	}

	/** Route guard used by the authenticated layout. */
	async guard(): Promise<boolean> {
		if (this.loading) await this.load();
		if (this.needsSetup) {
			await goto('/setup');
			return false;
		}
		if (!this.authenticated) {
			await goto('/login');
			return false;
		}
		return true;
	}
}

export const session = new SessionStore();
