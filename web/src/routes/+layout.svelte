<script lang="ts">
	import '../app.css';
	import { AppBackground, ToastHost } from '$lib/design-system';

	let { children } = $props();
</script>

<AppBackground />

{@render children()}

<ToastHost />
