<script lang="ts">
	// Image drop zone with inline preview: drag-and-drop or click to choose,
	// showing the current image and a remove action. Purely presentational —
	// the consumer owns the upload call and the busy state.
	import GlassPanel from './GlassPanel.svelte';
	import PillButton from './PillButton.svelte';

	interface Props {
		/** Current image URL; empty renders the prompt instead of a preview. */
		src?: string;
		label: string;
		hint?: string;
		error?: string;
		accept?: string;
		/** True while an upload is in flight. Uses aria-disabled, never the native
		 *  attribute, so a keyboard user mid-upload keeps focus. */
		busy?: boolean;
		disabled?: boolean;
		promptText?: string;
		/** Announced by the live region while an upload is in flight. */
		busyText?: string;
		chooseLabel?: string;
		removeLabel?: string;
		/** Description of the current image — a logo is content, not decoration. */
		previewAlt?: string;
		onselect?: (file: File) => void;
		onremove?: () => void;
		class?: string;
	}

	let {
		src,
		label,
		hint,
		error,
		accept = 'image/png,image/jpeg,image/webp',
		busy = false,
		disabled = false,
		promptText = 'Drop an image here',
		busyText = 'Uploading…',
		chooseLabel = 'Choose file',
		removeLabel = 'Remove',
		previewAlt = '',
		onselect,
		onremove,
		class: className = ''
	}: Props = $props();

	const id = $props.id();
	const describedBy = $derived(
		[error ? `${id}-error` : null, hint ? `${id}-hint` : null].filter(Boolean).join(' ') ||
			undefined
	);

	let dragover = $state(false);
	let input = $state<HTMLInputElement>();

	function pick(list: FileList | null) {
		const f = list?.[0];
		if (f && !disabled && !busy) onselect?.(f);
	}
</script>

<div class="field {className}">
	<label for={id}>{label}</label>
	<GlassPanel
		blur="thin"
		radius="md"
		class={`zone${dragover ? ' dragover' : ''}${busy ? ' busy' : ''}`}
		ondragover={(e) => {
			e.preventDefault();
			if (!disabled && !busy) dragover = true;
		}}
		ondragleave={() => (dragover = false)}
		ondrop={(e) => {
			e.preventDefault();
			dragover = false;
			pick(e.dataTransfer?.files ?? null);
		}}
	>
		{#if src}
			<img class="preview" {src} alt={previewAlt} />
		{:else}
			<span class="icon" aria-hidden="true">
				<svg
					viewBox="0 0 24 24"
					fill="none"
					stroke="currentColor"
					stroke-width="1.7"
					stroke-linecap="round"
					stroke-linejoin="round"
				>
					<rect x="3" y="4" width="18" height="16" rx="3" />
					<circle cx="8.5" cy="9.5" r="1.6" />
					<path d="m4 17 4.5-4.5a2 2 0 0 1 2.8 0L16 17" />
					<path d="m14 15 1.8-1.8a2 2 0 0 1 2.8 0L20 14.6" />
				</svg>
			</span>
			<span class="prompt">{promptText}</span>
		{/if}
		<div class="actions">
			<PillButton
				size="sm"
				variant="secondary"
				{disabled}
				aria-disabled={busy || undefined}
				onclick={() => {
					if (!busy) input?.click();
				}}>{chooseLabel}</PillButton
			>
			{#if src}
				<PillButton
					size="sm"
					variant="ghost"
					{disabled}
					aria-disabled={busy || undefined}
					onclick={() => {
						if (!busy) onremove?.();
					}}>{removeLabel}</PillButton
				>
			{/if}
		</div>
		<input
			class="sr-only"
			bind:this={input}
			{id}
			type="file"
			{accept}
			{disabled}
			aria-describedby={describedBy}
			aria-invalid={error ? 'true' : undefined}
			aria-disabled={busy || undefined}
			aria-busy={busy || undefined}
			onclick={(e) => {
				// No-op while busy: the picker stays shut but the input keeps focus.
				if (busy) e.preventDefault();
			}}
			onchange={(e) => {
				pick(e.currentTarget.files);
				e.currentTarget.value = '';
			}}
		/>
	</GlassPanel>
	<p class="sr-only" aria-live="polite">{busy ? busyText : ''}</p>
	{#if error}
		<p class="msg error" id="{id}-error">{error}</p>
	{:else if hint}
		<p class="msg hint" id="{id}-hint">{hint}</p>
	{/if}
</div>

<style>
	.field {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}

	label {
		font-size: var(--text-footnote);
		font-weight: var(--weight-medium);
		color: var(--color-text-secondary);
	}

	/* The zone classes land on GlassPanel's own element, so the scoped-CSS
	   compiler cannot see them and would strip these rules as unused. */
	:global(.zone) {
		display: flex;
		flex-direction: column;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-6);
		text-align: center;
		transition:
			border-color var(--duration-sm) var(--ease-out-expo),
			background var(--duration-sm) var(--ease-out-expo);
	}
	:global(.zone.dragover) {
		border-color: var(--glass-border-strong);
		background: var(--color-accent-tint);
	}
	:global(.zone.busy) {
		opacity: 0.7;
	}

	:global(.zone .preview) {
		max-height: 4rem;
		max-width: 100%;
		border-radius: var(--radius-squircle-sm);
	}

	:global(.zone .icon) {
		display: grid;
		place-items: center;
		width: 3.5rem;
		height: 3.5rem;
		border-radius: var(--radius-squircle-sm);
		color: var(--color-accent);
		background: var(--color-accent-tint);
		box-shadow: inset 0 1px 0 var(--glass-highlight);
	}
	:global(.zone .icon svg) {
		width: 1.75rem;
		height: 1.75rem;
	}

	:global(.zone .prompt) {
		font-size: var(--text-footnote);
		color: var(--color-text-secondary);
	}

	:global(.zone .actions) {
		display: flex;
		align-items: center;
		gap: var(--space-3);
	}
	/* Busy actions are aria-disabled rather than :disabled, so mirror the
	   native dimming here. */
	:global(.zone .actions [aria-disabled='true']) {
		cursor: not-allowed;
		opacity: 0.55;
	}
	:global(.zone .actions [aria-disabled='true']:hover),
	:global(.zone .actions [aria-disabled='true']:active) {
		transform: none;
	}

	.msg {
		margin: 0;
		font-size: var(--text-caption-1);
	}
	.hint {
		color: var(--color-text-tertiary);
	}
	.error {
		color: var(--color-danger);
		font-weight: var(--weight-medium);
	}

	/* Visually hidden but still focusable and label-reachable. */
	.sr-only {
		position: absolute;
		width: 1px;
		height: 1px;
		margin: -1px;
		padding: 0;
		overflow: hidden;
		white-space: nowrap;
		clip-path: inset(50%);
		border: 0;
	}

	@media (prefers-reduced-motion: reduce) {
		:global(.zone) {
			transition: none;
		}
	}
</style>
