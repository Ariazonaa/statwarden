/**
 * Live-reconciliation contract for the monitors store: a heartbeat carries the
 * monitor's current status and must be treated as a state source, and a socket
 * reconnect must trigger a REST reload so a state_change missed while offline is
 * repaired.
 *
 * Runs the store against a real (faked) socket by stubbing VITE_USE_MOCK false
 * and re-importing the modules fresh so the ws singleton and USE_MOCK are
 * re-evaluated per test.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Monitor } from '$lib/api';

class FakeWebSocket {
	static instances: FakeWebSocket[] = [];
	static OPEN = 1;
	static CLOSED = 3;
	readyState = 0;
	url: string;
	sent: string[] = [];
	#listeners: Record<string, ((ev?: unknown) => void)[]> = {};

	constructor(url: string) {
		this.url = url;
		FakeWebSocket.instances.push(this);
	}
	addEventListener(type: string, fn: (ev?: unknown) => void) {
		(this.#listeners[type] ??= []).push(fn);
	}
	send(data: string) {
		this.sent.push(data);
	}
	close() {
		this.readyState = FakeWebSocket.CLOSED;
		this.#fire('close');
	}
	fireOpen() {
		this.readyState = FakeWebSocket.OPEN;
		this.#fire('open');
	}
	fireMessage(data: unknown) {
		this.#fire('message', { data: JSON.stringify(data) });
	}
	#fire(type: string, ev?: unknown) {
		for (const fn of this.#listeners[type] ?? []) fn(ev);
	}
}

function seedMonitor(): Monitor {
	return {
		id: '1',
		name: 'API',
		status: 'up',
		latencyMs: 100,
		sparkline: [],
		recentBeats: ['up'],
		tagIds: []
	} as unknown as Monitor;
}

// eslint-disable-next-line @typescript-eslint/no-explicit-any
let ws: any;
// eslint-disable-next-line @typescript-eslint/no-explicit-any
let monitors: any;
// eslint-disable-next-line @typescript-eslint/no-explicit-any
let api: any;

beforeEach(async () => {
	FakeWebSocket.instances = [];
	vi.stubGlobal('WebSocket', FakeWebSocket);
	vi.stubEnv('VITE_USE_MOCK', 'false');
	vi.useFakeTimers();
	vi.resetModules();
	({ ws } = await import('$lib/ws/store.svelte'));
	({ monitors } = await import('./monitors.svelte'));
	({ api } = await import('$lib/api'));
});

afterEach(() => {
	vi.useRealTimers();
	vi.unstubAllGlobals();
	vi.unstubAllEnvs();
});

describe('monitors live reconciliation', () => {
	it('applies heartbeat.status as a state source', () => {
		monitors.items = [seedMonitor()];
		const stop = monitors.activate();
		ws.connect();
		FakeWebSocket.instances[0].fireOpen();

		FakeWebSocket.instances[0].fireMessage({
			type: 'heartbeat',
			topic: 'dashboard:all',
			data: { monitor_id: 1, status: 'down', at: Date.now(), important: false, retries: 0 }
		});

		expect(monitors.get('1').status).toBe('down');
		stop();
	});

	it('appends every heartbeat to the per-check bar, latency or not', () => {
		monitors.items = [seedMonitor()];
		const stop = monitors.activate();
		ws.connect();
		FakeWebSocket.instances[0].fireOpen();

		// A DOWN beat without a latency: the sparkline skips it, the bar must not.
		FakeWebSocket.instances[0].fireMessage({
			type: 'heartbeat',
			topic: 'dashboard:all',
			data: { monitor_id: 1, status: 'down', at: Date.now(), important: true, retries: 0 }
		});

		expect(monitors.get('1').recentBeats).toEqual(['up', 'down']);
		expect(monitors.get('1').sparkline).toEqual([]);
		stop();
	});

	it('reloads monitors from REST after a reconnect', () => {
		const spy = vi.spyOn(api, 'listMonitors').mockResolvedValue([]);
		monitors.items = [seedMonitor()];
		monitors.loaded = true;
		const stop = monitors.activate();
		ws.connect();
		FakeWebSocket.instances[0].fireOpen();

		// Drop then let the backoff reconnect fire.
		FakeWebSocket.instances[0].close();
		expect(spy).not.toHaveBeenCalled();
		vi.advanceTimersByTime(20000);
		FakeWebSocket.instances[1].fireOpen();

		expect(spy).toHaveBeenCalledTimes(1);
		stop();
	});
});
