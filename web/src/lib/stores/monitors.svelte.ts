/**
 * Monitors store — the single source of truth for the dashboard list,
 * kept live by the WebSocket hub. On `heartbeat` it updates latency +
 * sparkline; on `state_change` it updates status (which re-sorts the list
 * and triggers a toast). Subscribes to `dashboard:all` while mounted.
 */
import {
	api,
	problemMessage,
	type Heartbeat,
	type Monitor,
	type MonitorStatus,
	type StateEvent
} from '$lib/api';
import { ws, type WsEnvelope } from '$lib/ws/store.svelte';
import { toast } from '$lib/design-system';

interface HeartbeatData extends Heartbeat {}
interface StateChangeData {
	monitorId: string;
	from: MonitorStatus | null;
	to: MonitorStatus;
	at: string;
}

class MonitorsStore {
	items = $state<Monitor[]>([]);
	loaded = $state(false);
	error = $state<string | undefined>(undefined);

	#offMessage: (() => void) | undefined;
	#offTopic: (() => void) | undefined;
	#offReconnect: (() => void) | undefined;
	#mounts = 0;

	get byStatus(): Record<MonitorStatus, number> {
		const counts: Record<MonitorStatus, number> = {
			up: 0,
			down: 0,
			pending: 0,
			maintenance: 0,
			paused: 0
		};
		for (const m of this.items) counts[m.status] += 1;
		return counts;
	}

	get(id: string): Monitor | undefined {
		return this.items.find((m) => m.id === id);
	}

	async load(force = false): Promise<void> {
		if (this.loaded && !force) return;
		// Clear any prior failure up front so a retry shows the loading state
		// rather than a stale error, and a background refresh that succeeds can
		// never leave `error` set behind a rendered list.
		this.error = undefined;
		try {
			this.items = await api.listMonitors();
			this.loaded = true;
		} catch (e) {
			this.error = problemMessage(e) ?? 'Could not load monitors.';
		}
	}

	/**
	 * Force a fresh fetch of the list, bypassing the `loaded` cache. Use after a
	 * bulk mutation the WebSocket hub does not replay — e.g. a config or Uptime
	 * Kuma import that inserts monitors server-side — so the overview reflects
	 * the new rows without a manual page reload.
	 */
	reload(): Promise<void> {
		return this.load(true);
	}

	/** Called by the dashboard on mount; wires WS live updates once. */
	activate(): () => void {
		this.#mounts += 1;
		if (this.#mounts === 1) {
			this.#offTopic = ws.subscribe('dashboard:all');
			this.#offMessage = ws.on((env) => this.#handle(env));
			// After a reconnect the socket only replays topics, not the frames
			// missed while offline, so a state_change dropped mid-outage would
			// stick forever. Reload the list from REST to resync.
			this.#offReconnect = ws.onReconnect(() => this.load(true));
		}
		return () => {
			this.#mounts -= 1;
			if (this.#mounts === 0) {
				this.#offMessage?.();
				this.#offTopic?.();
				this.#offReconnect?.();
				this.#offMessage = this.#offTopic = this.#offReconnect = undefined;
			}
		};
	}

	#handle(env: WsEnvelope) {
		if (env.topic !== 'dashboard:all') return;
		if (env.type === 'heartbeat') {
			const hb = env.data as HeartbeatData;
			const m = this.get(hb.monitorId);
			if (!m) return;
			if (hb.latencyMs != null) {
				m.latencyMs = hb.latencyMs;
				m.sparkline = [...m.sparkline.slice(-39), hb.latencyMs];
			}
			m.lastCheckedAt = hb.checkedAt;
			// The per-check bar takes every beat, latency or not: a DOWN check
			// usually measured nothing, and it is the one the bar is there to show.
			if (hb.status) m.recentBeats = [...m.recentBeats.slice(-49), hb.status];
			// Every heartbeat carries the monitor's current status, so it doubles
			// as a state source: if a state_change was missed (e.g. dropped over a
			// reconnect) the next heartbeat silently corrects the badge. The toast
			// stays on state_change, which is the transition event.
			if (hb.status) m.status = hb.status;
		} else if (env.type === 'state_change') {
			const sc = env.data as StateChangeData;
			const m = this.get(sc.monitorId);
			if (!m) return;
			m.status = sc.to;
			if (sc.to === 'down') toast.error(`${m.name} is down`);
			else if (sc.from === 'down' && sc.to === 'up') toast.success(`${m.name} recovered`);
		}
	}

	// ---- optimistic mutations used by cards / detail / forms ----

	replace(m: Monitor) {
		const i = this.items.findIndex((x) => x.id === m.id);
		if (i >= 0) this.items[i] = m;
		else this.items.unshift(m);
	}

	remove(id: string) {
		const i = this.items.findIndex((x) => x.id === id);
		if (i >= 0) this.items.splice(i, 1);
	}

	async pause(id: string) {
		this.replace(await api.pauseMonitor(id));
	}
	async resume(id: string) {
		this.replace(await api.resumeMonitor(id));
	}
	async checkNow(id: string) {
		this.replace(await api.checkNow(id));
	}
	async delete(id: string) {
		await api.deleteMonitor(id);
		this.remove(id);
	}
}

export const monitors = new MonitorsStore();
export type { StateEvent };
