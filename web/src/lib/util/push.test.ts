import { describe, it, expect } from 'vitest';
import { pushUrl, pushCurl, rememberPushToken, takePushToken } from './push';
import { mockApi } from '$lib/api/mock';

describe('push URL helpers', () => {
	it('builds the heartbeat URL from the origin the browser is using', () => {
		expect(pushUrl('abc123', 'https://status.example.com')).toBe(
			'https://status.example.com/api/v1/push/abc123'
		);
		expect(pushUrl('abc123', 'https://status.example.com/')).toBe(
			'https://status.example.com/api/v1/push/abc123'
		);
	});

	it('gives a cron-ready example that POSTs, since the route is POST only', () => {
		expect(pushCurl('https://status.example.com/api/v1/push/abc')).toBe(
			'curl -fsS -m 10 -X POST "https://status.example.com/api/v1/push/abc?status=up&msg=OK"'
		);
	});

	// The token is stored only as a hash, so the one chance to show it is right
	// after creation. It is handed to the detail page in memory, exactly once.
	it('hands a fresh token to the detail page exactly once, for that monitor only', () => {
		rememberPushToken('m1', 'tok-1');
		expect(takePushToken('m2')).toBeUndefined();
		expect(takePushToken('m1')).toBe('tok-1');
		expect(takePushToken('m1')).toBeUndefined();
	});
});

describe('mock push tokens', () => {
	it('creates a push monitor with a token and can mint a new one', async () => {
		const m = await mockApi.createMonitor({ name: 'backup job', type: 'push', config: {} });
		expect(m.pushToken).toMatch(/^[0-9a-f]{32}$/);
		const r = await mockApi.regeneratePushToken(m.id);
		expect(r.pushToken).toMatch(/^[0-9a-f]{32}$/);
		expect(r.pushToken).not.toBe(m.pushToken);
		expect(r.pushUrl.endsWith('/api/v1/push/' + r.pushToken)).toBe(true);
	});
});
