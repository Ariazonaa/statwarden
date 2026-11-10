/**
 * Reconnect-reconciliation contract for the WebSocket store.
 *
 * The store runs against a real socket only when USE_MOCK is false, so the
 * suite stubs VITE_USE_MOCK and swaps in a scriptable fake WebSocket, then
 * re-imports the module fresh so USE_MOCK is re-evaluated. `browser` is already
 * true under the test's browser resolve condition.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

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

// eslint-disable-next-line @typescript-eslint/no-explicit-any
let ws: any;

beforeEach(async () => {
	FakeWebSocket.instances = [];
	vi.stubGlobal('WebSocket', FakeWebSocket);
	vi.stubEnv('VITE_USE_MOCK', 'false');
	vi.useFakeTimers();
	vi.resetModules();
	({ ws } = await import('./store.svelte'));
});

afterEach(() => {
	vi.useRealTimers();
	vi.unstubAllGlobals();
	vi.unstubAllEnvs();
});

describe('ws reconnect reconciliation', () => {
	it('does not fire onReconnect on the first connect', () => {
		const cb = vi.fn();
		ws.onReconnect(cb);
		ws.connect();
		FakeWebSocket.instances[0].fireOpen();
		expect(ws.online).toBe(true);
		expect(cb).not.toHaveBeenCalled();
	});

	it('fires onReconnect once after a drop and re-subscribes the topics', () => {
		const cb = vi.fn();
		ws.onReconnect(cb);
		ws.connect();
		FakeWebSocket.instances[0].fireOpen();
		ws.subscribe('dashboard:all');

		// Drop the socket: the store schedules a backoff reconnect.
		FakeWebSocket.instances[0].close();
		expect(cb).not.toHaveBeenCalled();

		// Let the backoff timer elapse so the store dials a second socket.
		vi.advanceTimersByTime(20000);
		const second = FakeWebSocket.instances[1];
		expect(second).toBeDefined();

		second.fireOpen();
		expect(cb).toHaveBeenCalledTimes(1);
		// The re-opened socket must re-subscribe to the retained topic.
		expect(second.sent.some((s) => s.includes('"subscribe"') && s.includes('dashboard:all'))).toBe(
			true
		);
	});

	it('keeps a topic subscribed while a second holder is still on it', () => {
		ws.connect();
		const sock = FakeWebSocket.instances[0];
		sock.fireOpen();

		const offA = ws.subscribe('monitor:1');
		const offB = ws.subscribe('monitor:1');
		// The hub is told once, not once per holder.
		expect(sock.sent.filter((s: string) => s.includes('"subscribe"')).length).toBe(1);

		// The first holder going away must NOT cut the second one off.
		offA();
		expect(sock.sent.some((s: string) => s.includes('"unsubscribe"'))).toBe(false);

		offB();
		expect(sock.sent.filter((s: string) => s.includes('"unsubscribe"')).length).toBe(1);
	});

	it('ignores a repeated release from the same subscriber', () => {
		ws.connect();
		const sock = FakeWebSocket.instances[0];
		sock.fireOpen();

		const offA = ws.subscribe('monitor:2');
		ws.subscribe('monitor:2');
		offA();
		// A double release would otherwise decrement the other holder's count away.
		offA();
		expect(sock.sent.some((s: string) => s.includes('"unsubscribe"'))).toBe(false);
	});

	it('re-subscribes a held topic exactly once after a reconnect', () => {
		ws.connect();
		FakeWebSocket.instances[0].fireOpen();
		ws.subscribe('monitor:3');
		ws.subscribe('monitor:3');

		FakeWebSocket.instances[0].close();
		vi.advanceTimersByTime(20000);
		const second = FakeWebSocket.instances[1];
		second.fireOpen();

		const resubs = second.sent.filter(
			(s: string) => s.includes('"subscribe"') && s.includes('monitor:3')
		);
		expect(resubs.length).toBe(1);
		// Iterating the topic map must yield topic NAMES, not [key, value] pairs.
		expect(JSON.parse(resubs[0]).topic).toBe('monitor:3');
	});

	it('stops firing onReconnect after the listener unregisters', () => {
		const cb = vi.fn();
		const off = ws.onReconnect(cb);
		off();
		ws.connect();
		FakeWebSocket.instances[0].fireOpen();
		FakeWebSocket.instances[0].close();
		vi.advanceTimersByTime(20000);
		FakeWebSocket.instances[1].fireOpen();
		expect(cb).not.toHaveBeenCalled();
	});
});

describe('ws session loss', () => {
	async function watchUnauthorized() {
		// Same module instance the freshly imported store uses (resetModules ran
		// in beforeEach, and the store pulled client in with it).
		const { onUnauthorized } = await import('$lib/api/client');
		const expired = vi.fn();
		onUnauthorized(expired);
		return expired;
	}

	it('reports the hub closing an open socket over an expired session', async () => {
		const expired = await watchUnauthorized();
		ws.connect();
		const sock = FakeWebSocket.instances[0];
		sock.fireOpen();
		sock.fireMessage({ type: 'error', message: 'session expired' });
		expect(expired).toHaveBeenCalledTimes(1);
	});

	it('reports a private subscription refused on an anonymous reconnect', async () => {
		const expired = await watchUnauthorized();
		ws.connect();
		const sock = FakeWebSocket.instances[0];
		sock.fireOpen();
		ws.subscribe('dashboard:all');
		sock.fireMessage({ type: 'error', message: 'authentication required for this topic' });
		expect(expired).toHaveBeenCalledTimes(1);
	});

	it('leaves unrelated hub errors alone', async () => {
		const expired = await watchUnauthorized();
		ws.connect();
		const sock = FakeWebSocket.instances[0];
		sock.fireOpen();
		sock.fireMessage({ type: 'error', message: 'unknown topic' });
		sock.fireMessage({ type: 'error', message: 'subscription limit reached' });
		expect(expired).not.toHaveBeenCalled();
	});
});

describe('ws reconnect after a manual disconnect', () => {
	/** A socket whose close event arrives later, as a real one's does. */
	function deferClose(sock: FakeWebSocket): () => void {
		let fire: () => void = () => {};
		const original = sock.close.bind(sock);
		sock.close = () => {
			sock.readyState = FakeWebSocket.CLOSED;
			fire = original;
		};
		return () => fire();
	}

	it('keeps the new socket when the old one finishes closing late', () => {
		ws.connect();
		const first = FakeWebSocket.instances[0];
		first.fireOpen();
		const finishClose = deferClose(first);

		// Leaving the app shell and coming straight back: the browser delivers
		// the first socket's close event only after the second one exists.
		ws.disconnect();
		ws.connect();
		const second = FakeWebSocket.instances[1];
		second.fireOpen();
		finishClose();

		// The stale close must not drop the live socket or dial a third one.
		vi.advanceTimersByTime(20000);
		expect(FakeWebSocket.instances).toHaveLength(2);
		expect(ws.online).toBe(true);
		ws.subscribe('dashboard:all');
		expect(second.sent.some((s) => s.includes('dashboard:all'))).toBe(true);
	});
});
