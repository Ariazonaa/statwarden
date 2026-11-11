import { it, expect } from 'vitest';
import { mockApi } from './mock';

// The monitor form passes Svelte $state arrays (tag and channel ids), which are
// proxies. structuredClone cannot copy a proxy, so a mock that stored them as
// given failed the next read with a DataCloneError and no monitor could be
// created or edited against the mock.
it('stores a plain copy of form input, not the reactive proxies', async () => {
	const created = await mockApi.createMonitor({
		name: 'proxied',
		type: 'http',
		config: { url: 'https://example.com' },
		tagIds: new Proxy<string[]>([], {}),
		notificationIds: new Proxy<string[]>([], {})
	});
	await expect(mockApi.listMonitors()).resolves.toBeTruthy();
	await mockApi.updateMonitor(created.id, {
		name: 'proxied 2',
		tagIds: new Proxy<string[]>([], {})
	});
	await expect(mockApi.getMonitor(created.id)).resolves.toMatchObject({ name: 'proxied 2' });
});
