import adapter from '@sveltejs/adapter-static';
import { vitePreprocess } from '@sveltejs/vite-plugin-svelte';

/** @type {import('@sveltejs/kit').Config} */
const config = {
	preprocess: vitePreprocess(),
	kit: {
		// Pure SPA: everything renders client-side, the Go binary serves
		// dist/ via go:embed with index.html as the catch-all fallback.
		adapter: adapter({
			pages: 'dist',
			assets: 'dist',
			fallback: 'index.html',
			precompress: false,
			strict: true
		})
	}
};

export default config;
