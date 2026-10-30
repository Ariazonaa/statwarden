<script lang="ts">
	// Renders a scannable QR code from an arbitrary string (here an otpauth://
	// TOTP-enrolment URI) to a data-URL <img>. Generation is client-side and
	// cheap; the effect re-runs whenever `value` changes. On failure the caller's
	// manual-entry fallback (the setup key) still stands, so we render nothing
	// rather than surface an error toast — this is a local render, not a mutation.
	import QRCode from 'qrcode';

	let { value, alt, size = 180 }: { value: string; alt: string; size?: number } = $props();

	let src = $state<string | undefined>(undefined);

	$effect(() => {
		let active = true;
		src = undefined;
		// Fixed black-on-white with a quiet-zone margin so the code scans against
		// the translucent glass background regardless of the active theme. Rendered
		// at 2x the display size for crispness on high-density screens.
		QRCode.toDataURL(value, {
			errorCorrectionLevel: 'M',
			margin: 2,
			width: size * 2,
			color: { dark: '#000000', light: '#ffffff' }
		})
			.then((url) => {
				if (active) src = url;
			})
			.catch(() => {
				// Manual-entry fallback covers this; nothing to surface.
			});
		return () => {
			active = false;
		};
	});
</script>

{#if src}
	<img class="qr" {src} {alt} width={size} height={size} />
{/if}

<style>
	.qr {
		display: block;
		border-radius: var(--radius-squircle-sm);
		background: #ffffff;
	}
</style>
