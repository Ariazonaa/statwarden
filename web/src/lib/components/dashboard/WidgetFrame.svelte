<script lang="ts">
	/**
	 * The chrome around one dashboard widget: a heading, and — only while the
	 * dashboard is in edit mode — the controls that move it, resize it and take it
	 * away.
	 *
	 * The controls exist twice over on purpose. Dragging is the fast path, but it
	 * is unusable from a keyboard and unreliable on a touch screen, so every
	 * arrangement this widget supports is also reachable through ordinary buttons.
	 * That is not a fallback bolted on afterwards; it is why the layout module is
	 * a set of plain functions over the layout array rather than a drop handler,
	 * and why the up/down buttons drive the same reorder the drop does.
	 */
	import type { Snippet } from 'svelte';
	import { t } from '$lib/i18n';
	import type { WidgetType, WidgetWidth } from '$lib/dashboard/layout';

	interface Props {
		type: WidgetType;
		title: string;
		width: WidgetWidth;
		editing: boolean;
		/** Position in the layout, for the disabled state of the move buttons. */
		first: boolean;
		last: boolean;
		dragging?: boolean;
		onmove: (delta: -1 | 1) => void;
		oncycleWidth: () => void;
		onremove: () => void;
		ondragstart?: () => void;
		ondrop?: () => void;
		ondragend?: () => void;
		children: Snippet;
	}

	let {
		type,
		title,
		width,
		editing,
		first,
		last,
		dragging = false,
		onmove,
		oncycleWidth,
		onremove,
		ondragstart,
		ondrop,
		ondragend,
		children
	}: Props = $props();

	const widthLabel = $derived(
		width === 'third'
			? t('widget.widthThird')
			: width === 'half'
				? t('widget.widthHalf')
				: t('widget.widthFull')
	);
</script>

<section
	class="widget ds-glass"
	class:editing
	class:dragging
	data-widget={type}
	draggable={editing}
	ondragstart={(e) => {
		// Two guards, and both are load-bearing, because dragstart BUBBLES.
		//
		// The section is only draggable in edit mode, but a link, an image or a
		// text selection inside it is draggable at all times, and its dragstart
		// arrives here. Without the checks this handler hijacked those drags:
		// setData replaced the link's URL with a widget type, and ondragstart
		// faded the whole widget to 50% for a drag it has nothing to do with.
		// currentTarget is the section, and for a drag of the section itself the
		// target IS the section — a nested source is anything else.
		if (!editing || e.target !== e.currentTarget) return;
		// setData is not decoration. Firefox CANCELS a drag whose dragstart
		// handler sets no drag data, so without this the board could be rearranged
		// in Chrome and Safari and simply refused to pick anything up in Firefox.
		// The payload itself is unused — the board tracks the dragged widget in its
		// own state — so it carries the widget type purely to be non-empty and
		// legible if it ever lands somewhere that reads it.
		e.dataTransfer?.setData('text/plain', type);
		if (e.dataTransfer) e.dataTransfer.effectAllowed = 'move';
		ondragstart?.();
	}}
	ondragover={(e) => {
		if (!editing) return;
		// preventDefault is what marks this element as a valid drop target;
		// without it the drop never fires at all.
		e.preventDefault();
		if (e.dataTransfer) e.dataTransfer.dropEffect = 'move';
	}}
	ondrop={(e) => {
		if (!editing) return;
		e.preventDefault();
		ondrop?.();
	}}
	ondragend={() => ondragend?.()}
	aria-label={title}
>
	<header class="whead">
		<h2>{title}</h2>
		<!-- draggable=false: the section around these is draggable in edit mode, and
		     a press-and-move on a button inside a draggable ancestor starts that
		     ancestor's drag in several browsers — so the remove button would drag
		     the widget instead of pressing. -->
		<div class="wactions" draggable="false">
			{#if editing}
				<button
					type="button"
					class="wbtn"
					disabled={first}
					aria-label={t('widget.moveUp', { name: title })}
					onclick={() => onmove(-1)}>↑</button
				>
				<button
					type="button"
					class="wbtn"
					disabled={last}
					aria-label={t('widget.moveDown', { name: title })}
					onclick={() => onmove(1)}>↓</button
				>
				<button
					type="button"
					class="wbtn wide"
					aria-label={t('widget.cycleWidth', { name: title })}
					onclick={oncycleWidth}>{widthLabel}</button
				>
				<button
					type="button"
					class="wbtn danger"
					aria-label={t('widget.remove', { name: title })}
					onclick={onremove}>✕</button
				>
			{/if}
		</div>
	</header>
	<div class="wbody">
		{@render children()}
	</div>
</section>

<style>
	.widget {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
		padding: var(--space-5);
		border-radius: var(--radius-squircle-lg);
		min-width: 0;
	}
	/* The dashed outline is the only thing that says "this is now draggable".
	   Without it, edit mode looks identical to normal and the grab cursor is the
	   sole hint — which a touch user never sees at all. */
	.widget.editing {
		outline: 1px dashed var(--color-accent);
		outline-offset: -1px;
		cursor: grab;
	}
	.widget.dragging {
		opacity: 0.5;
	}
	.whead {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		min-height: 1.75rem;
	}
	.whead h2 {
		margin: 0;
		font-size: var(--text-subheadline);
		font-weight: var(--weight-semibold);
		color: var(--color-text-secondary);
		letter-spacing: var(--tracking-normal);
	}
	.wactions {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		margin-left: auto;
	}
	.wbtn {
		display: inline-grid;
		place-items: center;
		min-width: 1.75rem;
		height: 1.75rem;
		padding: 0 var(--space-2);
		border-radius: var(--radius-pill);
		border: 1px solid var(--glass-border);
		background: var(--glass-bg-thin);
		color: var(--color-text-secondary);
		font: inherit;
		font-size: var(--text-caption-1);
		cursor: pointer;
	}
	.wbtn.wide {
		min-width: 3.5rem;
	}
	.wbtn:hover:not(:disabled) {
		border-color: var(--glass-border-strong);
		color: var(--color-text-primary);
	}
	.wbtn:disabled {
		opacity: 0.4;
		cursor: default;
	}
	.wbtn.danger:hover {
		color: var(--status-down);
		border-color: var(--status-down);
	}
	.wbody {
		min-width: 0;
	}
</style>
