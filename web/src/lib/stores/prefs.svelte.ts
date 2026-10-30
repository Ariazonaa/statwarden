/**
 * Persisted UI preferences: card density, pinned/favourite monitors, a manual
 * drag-and-drop ordering, and the dashboard's widget arrangement. All backed by
 * localStorage so choices survive reloads.
 *
 * See $lib/dashboard/layout for why the dashboard arrangement lives here rather
 * than on the server.
 */
import { browser } from '$app/environment';
import { defaultLayout, sanitizeLayout, type Widget } from '$lib/dashboard/layout';

export type Density = 'comfortable' | 'compact';

const KEY = 'statwarden:prefs';
/**
 * Where the same preferences lived while the project was called Uptura. Read
 * once when the new key is empty, so a rename does not reset everyone's
 * favourites, order and dashboard; the next write lands under KEY.
 */
const LEGACY_KEY = 'uptura:prefs';

interface PrefsShape {
	density: Density;
	favorites: string[];
	order: string[];
	/**
	 * Absent means "never arranged" and yields the default layout; an empty array
	 * means "deliberately emptied" and is kept. The two must stay distinguishable
	 * or the remove button silently stops working on the last widget.
	 */
	dashboard?: Widget[];
	/**
	 * The dashboard value exactly as it was found in storage, before sanitising.
	 *
	 * Kept so an untouched arrangement can be written back UNCHANGED. See
	 * PrefsStore.#persist.
	 */
	rawDashboard?: unknown;
}

function read(): PrefsShape {
	const fallback: PrefsShape = { density: 'comfortable', favorites: [], order: [] };
	if (!browser) return fallback;
	try {
		const raw = localStorage.getItem(KEY) ?? localStorage.getItem(LEGACY_KEY);
		if (!raw) return fallback;
		const parsed = JSON.parse(raw);
		return {
			...fallback,
			...parsed,
			// Never trusted verbatim: this value was written by whatever version of
			// the app the browser last loaded, which may be newer than this one.
			dashboard: sanitizeLayout(parsed?.dashboard),
			rawDashboard: parsed?.dashboard
		};
	} catch {
		return fallback;
	}
}

/** Two layouts are the same when their widgets match in order, type and width. */
function sameLayout(a: Widget[], b: Widget[]): boolean {
	if (a === b) return true;
	if (a.length !== b.length) return false;
	return a.every((w, i) => w.type === b[i].type && w.width === b[i].width);
}

class PrefsStore {
	density = $state<Density>('comfortable');
	favorites = $state<string[]>([]);
	order = $state<string[]>([]);
	dashboard = $state<Widget[]>([]);

	/**
	 * The stored dashboard value, verbatim, until this session actually changes
	 * the arrangement. See #persist.
	 */
	#storedDashboard: unknown;
	#dashboardTouched = false;

	constructor() {
		const p = read();
		this.density = p.density;
		this.favorites = p.favorites;
		this.order = p.order;
		this.dashboard = p.dashboard ?? defaultLayout();
		this.#storedDashboard = p.rawDashboard;
	}

	#persist() {
		if (!browser) return;
		try {
			localStorage.setItem(
				KEY,
				JSON.stringify({
					density: this.density,
					favorites: this.favorites,
					order: this.order,
					// The SANITISED layout is written only once this session has
					// actually rearranged something.
					//
					// sanitizeLayout drops widget types this build does not know
					// about, which is right for rendering and wrong for storing. One
					// browser opening two versions of Statwarden — a tab left on the old
					// one, a rollback, a stale service worker — is enough: the older
					// build reads the newer layout, drops what it cannot render, and
					// then the next unrelated preference change (toggling density,
					// starring a monitor) writes the truncated version back over the
					// real one. The user never went near the dashboard and their
					// arrangement is gone for good. Untouched means untouched.
					dashboard: this.#dashboardTouched ? this.dashboard : this.#storedDashboard
				})
			);
		} catch {
			/* storage unavailable */
		}
	}

	/** Replaces the dashboard arrangement. An empty array is a valid layout. */
	setDashboard(widgets: Widget[]) {
		// A no-op does not count as touching it.
		//
		// reorderWidget returns the SAME array instance when a widget is dropped
		// onto itself, and the drop handler passes that straight here. Marking
		// the layout touched then persisted the sanitized version over a newer
		// build's arrangement — the precise data loss #dashboardTouched exists to
		// prevent — for a gesture that changed nothing at all.
		if (sameLayout(this.dashboard, widgets)) return;
		this.dashboard = widgets;
		this.#dashboardTouched = true;
		this.#persist();
	}

	/** Restores the arrangement a fresh install starts with. */
	resetDashboard() {
		this.setDashboard(defaultLayout());
	}

	setDensity(d: Density) {
		this.density = d;
		this.#persist();
	}
	toggleDensity() {
		this.setDensity(this.density === 'comfortable' ? 'compact' : 'comfortable');
	}

	isFavorite(id: string) {
		return this.favorites.includes(id);
	}
	toggleFavorite(id: string) {
		this.favorites = this.isFavorite(id)
			? this.favorites.filter((x) => x !== id)
			: [...this.favorites, id];
		this.#persist();
	}

	/** Persist a full manual ordering (list of monitor ids). */
	setOrder(ids: string[]) {
		this.order = ids;
		this.#persist();
	}

	/** Sort a list by the saved manual order; unknown ids keep source order. */
	applyOrder<T extends { id: string }>(items: T[]): T[] {
		if (this.order.length === 0) return items;
		const rank = new Map(this.order.map((id, i) => [id, i]));
		return [...items].sort((a, b) => {
			const ra = rank.get(a.id) ?? Number.MAX_SAFE_INTEGER;
			const rb = rank.get(b.id) ?? Number.MAX_SAFE_INTEGER;
			return ra - rb;
		});
	}
}

export const prefs = new PrefsStore();
