/**
 * WebSocket hub client (see docs/architecture.md "WebSocket hub").
 * Envelope `{type, topic, data}`; client sends subscribe/unsubscribe/ping,
 * server sends heartbeat/state_change. Topics: monitor:{id}, dashboard:all,
 * status-page:{slug}.
 *
 * Reactive `connection` state drives the connection-lost banner; reconnect
 * uses exponential backoff with jitter. When the app runs against the mock
 * backend, we bridge the in-memory mockBus instead of opening a real socket,
 * so live updates work identically in development.
 */
import { browser } from '$app/environment';
import { USE_MOCK, wsHeartbeatFromWire, wsStateChangeFromWire } from '$lib/api/client';
import { mockBus } from '$lib/api/mock';

export type WsMessageType =
	'heartbeat' | 'state_change' | 'subscribe' | 'unsubscribe' | 'ping' | 'pong';

export interface WsEnvelope<T = unknown> {
	type: WsMessageType | string;
	topic: string;
	data: T;
}

export type ConnectionState = 'connecting' | 'open' | 'closed' | 'reconnecting';

type Handler = (env: WsEnvelope) => void;

class WsStore {
	connection = $state<ConnectionState>('closed');
	/** Reconnect attempt counter — surfaced for the banner copy. */
	attempt = $state(0);

	#socket: WebSocket | undefined;
	#mockUnsub: (() => void) | undefined;
	#handlers = new Set<Handler>();
	#reconnectHandlers = new Set<() => void>();
	/**
	 * Topic → number of live subscribers, NOT a plain Set.
	 *
	 * `subscribe` hands every caller its own unsubscribe function, and the store
	 * is a singleton shared by the whole app, so two components may legitimately
	 * hold the same topic at once. With a Set the first teardown deleted the
	 * topic outright and sent `unsubscribe` to the hub, silently killing live
	 * updates for everyone still mounted on it — a view that then sits on stale
	 * data with no error anywhere. Counting means the hub is only told once the
	 * last holder is gone.
	 */
	#topics = new Map<string, number>();
	#reconnectTimer: ReturnType<typeof setTimeout> | undefined;
	#pingTimer: ReturnType<typeof setInterval> | undefined;
	#started = false;
	#manualClose = false;

	get online() {
		return this.connection === 'open';
	}

	/** Open the connection (idempotent). Call once from the app shell. */
	connect() {
		if (!browser || this.#started) return;
		this.#started = true;
		this.#manualClose = false;
		this.#open();
	}

	disconnect() {
		this.#manualClose = true;
		this.#started = false;
		clearTimeout(this.#reconnectTimer);
		clearInterval(this.#pingTimer);
		this.#mockUnsub?.();
		this.#mockUnsub = undefined;
		this.#socket?.close();
		this.#socket = undefined;
		this.connection = 'closed';
	}

	/**
	 * Subscribe to a topic. Returns an unsubscribe fn that is safe to call more
	 * than once: the second call is a no-op rather than a decrement that would
	 * drop the topic out from under another holder.
	 */
	subscribe(topic: string): () => void {
		const held = this.#topics.get(topic) ?? 0;
		this.#topics.set(topic, held + 1);
		// Only the first holder has to tell the hub; it is already sending us this
		// topic for the others.
		if (held === 0) this.#send({ type: 'subscribe', topic, data: null });
		let released = false;
		return () => {
			if (released) return;
			released = true;
			this.unsubscribe(topic);
		};
	}

	unsubscribe(topic: string) {
		const held = this.#topics.get(topic) ?? 0;
		if (held <= 1) {
			this.#topics.delete(topic);
			// Only ever told to the hub when nobody is left, and never for a topic
			// we do not hold — an unsubscribe for a topic we never subscribed to is
			// noise the hub answers with an envelope nobody asked for.
			if (held === 1) this.#send({ type: 'unsubscribe', topic, data: null });
			return;
		}
		this.#topics.set(topic, held - 1);
	}

	/** Register a message handler. Returns an unregister fn. */
	on(handler: Handler): () => void {
		this.#handlers.add(handler);
		return () => this.#handlers.delete(handler);
	}

	/**
	 * Register a callback fired after the socket *reconnects* — never on the
	 * first connect. Frames delivered while offline are gone for good, so a
	 * missed state_change would otherwise leave a view wrong forever; callers
	 * use this to reconcile their state through the REST API. Returns an
	 * unregister fn.
	 */
	onReconnect(cb: () => void): () => void {
		this.#reconnectHandlers.add(cb);
		return () => this.#reconnectHandlers.delete(cb);
	}

	#emit(env: WsEnvelope) {
		for (const h of this.#handlers) h(env);
	}

	#emitReconnect() {
		for (const cb of this.#reconnectHandlers) cb();
	}

	#open() {
		if (USE_MOCK) {
			// Mock bridge: no socket, immediate "open", replay mockBus envelopes.
			this.connection = 'open';
			this.attempt = 0;
			this.#mockUnsub = mockBus.subscribe((env) => {
				if (this.#topics.has(env.topic)) this.#emit(env);
			});
			return;
		}

		this.connection = this.attempt === 0 ? 'connecting' : 'reconnecting';
		const proto = location.protocol === 'https:' ? 'wss' : 'ws';
		const sock = new WebSocket(`${proto}://${location.host}/ws`);
		this.#socket = sock;

		sock.addEventListener('open', () => {
			// attempt > 0 means this open follows a drop: a reconnect, not the
			// first connect. Re-subscribing restores the topic flow but not the
			// frames missed while offline, so reconnect listeners reconcile via REST.
			const reconnected = this.attempt > 0;
			this.connection = 'open';
			this.attempt = 0;
			// Re-subscribe to every held topic after a reconnect. Once each,
			// regardless of how many local holders it has — the hub's subscription
			// is per connection, not per component.
			for (const topic of this.#topics.keys()) this.#send({ type: 'subscribe', topic, data: null });
			this.#pingTimer = setInterval(
				() => this.#send({ type: 'ping', topic: '', data: null }),
				25000
			);
			if (reconnected) this.#emitReconnect();
		});

		sock.addEventListener('message', (ev) => {
			try {
				const env = JSON.parse(ev.data) as WsEnvelope;
				if (env.type === 'pong') return;
				// The hub speaks the same snake_case wire dialect as the REST API,
				// so frames are mapped here — subscribers must see the model shape
				// whether they were fed by the socket or by the mock bus.
				if (env.type === 'heartbeat')
					this.#emit({ ...env, data: wsHeartbeatFromWire(env.data as never) });
				else if (env.type === 'state_change')
					this.#emit({ ...env, data: wsStateChangeFromWire(env.data as never) });
				else this.#emit(env);
			} catch {
				/* ignore malformed frames */
			}
		});

		sock.addEventListener('close', () => {
			clearInterval(this.#pingTimer);
			this.#socket = undefined;
			if (this.#manualClose) return;
			this.#scheduleReconnect();
		});

		sock.addEventListener('error', () => sock.close());
	}

	#scheduleReconnect() {
		this.connection = 'reconnecting';
		this.attempt += 1;
		// Exponential backoff capped at 15s, with jitter.
		const backoff = Math.min(15000, 500 * 2 ** Math.min(this.attempt, 5));
		const jitter = Math.random() * 400;
		clearTimeout(this.#reconnectTimer);
		this.#reconnectTimer = setTimeout(() => this.#open(), backoff + jitter);
	}

	#send(env: WsEnvelope) {
		if (USE_MOCK) return;
		if (this.#socket?.readyState === WebSocket.OPEN) {
			this.#socket.send(JSON.stringify(env));
		}
	}
}

export const ws = new WsStore();
