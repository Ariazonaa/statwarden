<script lang="ts">
	import '../app.css';
	import { page } from '$app/state';
	import { AppBackground, ToastHost } from '$lib/design-system';

	let { children } = $props();

	// Only the public status pages are meant to be embedded. The server sends
	// frame-ancestors 'none' everywhere else, but a framable /status/* page that
	// navigates client-side — a link, the error page's way back — would carry the
	// signed-in dashboard into a foreign page's iframe without another request
	// for the browser to refuse. So the SPA refuses it as well.
	const framed = window.top !== window.self;
	const allowed = $derived(!framed || page.url.pathname.startsWith('/status/'));
</script>

{#if allowed}
	<AppBackground />

	{@render children()}

	<ToastHost />
{/if}
