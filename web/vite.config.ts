import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig } from 'vitest/config';

// `mode` is 'test' only under vitest, which is how the browser condition below
// stays out of the dev and build pipelines.
export default defineConfig(({ mode }) => ({
	plugins: [sveltekit()],
	// Resolve Svelte to its client build for the component tests. Without the
	// `browser` condition the SvelteKit plugin's SSR resolution wins and
	// `mount()` throws "not available on the server".
	resolve: mode === 'test' ? { conditions: ['browser'] } : {},
	server: {
		// During `npm run dev`, proxy API, upload and probe routes to the Go backend.
		proxy: {
			'/api': 'http://localhost:3001',
			'/uploads': 'http://localhost:3001',
			'/healthz': 'http://localhost:3001',
			'/readyz': 'http://localhost:3001'
		}
	},
	test: {
		// jsdom for everything: the component tests need a DOM, and the pure-model
		// tests do not care either way.
		environment: 'jsdom',
		setupFiles: ['./vitest-setup.ts'],
		include: ['src/**/*.test.ts'],
		globals: true
	}
}));
