/**
 * A list screen whose first load fails must say so and offer a retry, rather
 * than sitting on its skeleton forever. The tags screen stands in for the
 * others that share LoadFailed (activity, maintenance, notifications, users).
 */
import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/svelte';
import { tick } from 'svelte';

vi.mock('$app/navigation', () => ({ goto: vi.fn() }));

const listTags = vi.fn();
vi.mock('$lib/api', async (importOriginal) => {
	const real = await importOriginal<typeof import('$lib/api')>();
	return { ...real, api: { ...real.api, listTags: () => listTags() } };
});

const { i18n } = await import('$lib/i18n');
const { default: Page } = await import('./+page.svelte');

describe('tags screen first load', () => {
	it('shows a retryable error instead of an endless skeleton', async () => {
		i18n.setLocale('en', { persist: false });
		listTags.mockRejectedValueOnce({ title: 'Bad Gateway', status: 502 });
		render(Page);
		expect(await screen.findByText('Could not load this. Try again.')).toBeInTheDocument();

		listTags.mockResolvedValueOnce([{ id: '1', name: 'prod', color: '#ef4444' }]);
		await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
		await tick();
		expect(await screen.findByText('prod')).toBeInTheDocument();
		expect(screen.queryByText('Could not load this. Try again.')).not.toBeInTheDocument();
	});
});
