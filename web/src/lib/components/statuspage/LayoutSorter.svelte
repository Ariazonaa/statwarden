<script lang="ts">
	// Nested sortable for the status-page layout. The pointer path reorders groups
	// among themselves, reorders monitors inside a group, and moves a monitor
	// between groups by dropping it anywhere over the target group (header, rows or
	// padding — the whole box is one drop band). Pointer events (so touch works,
	// unlike HTML5 DnD). Drop feedback is live: the lifted item rides the pointer
	// while a dashed placeholder marks its drop slot and the receiving group is
	// ringed. A full keyboard equivalent mirrors all of it — arrows move an item,
	// and stepping past the end of a group crosses into the next one, which is how
	// cross-group moves stay reachable without a pointer. Every move is announced
	// in a live region.
	import { flushSync, tick, untrack } from 'svelte';
	import { DragGrip, createSpring, glassFlip, safeDuration, duration } from '$lib/design-system';
	import { t, type MessageKey } from '$lib/i18n';
	import type { StatusPageLayoutOrder } from '$lib/api';
	import { type Container, toOrder, moveGroup, moveMonitor } from './layout';

	interface Props {
		/** [0] is always the ungrouped pseudo-group (id: null); it never moves. */
		containers: Container[];
		/** A commit is in flight: grips go aria-disabled but keep focus. */
		busy?: boolean;
		oncommit: (order: StatusPageLayoutOrder) => void;
	}

	let { containers = $bindable(), busy = false, oncommit }: Props = $props();

	type Kind = 'group' | 'monitor';
	type Position = { kind: Kind; ci: number; mi: number };

	const instrId = $props.id();
	const headId = (ci: number) => `${instrId}-head-${ci}`;

	// "Follows pointer/drag almost 1:1" — the one preset written for this.
	const lift = createSpring({ y: 0 }, 'stiff');

	let root = $state<HTMLDivElement>();
	let announcement = $state('');

	let dragging = $state<Position | null>(null);
	let grabbed = $state<(Position & { name: string; key: string }) | null>(null);
	/** Set once a commit is sent; the effect below compares against it to notice a
	 *  rollback. $state so the effect re-runs when a commit starts. */
	let sent = $state<string | null>(null);

	// Plain locals: drag bookkeeping must not drive rendering.
	let origin: Position | null = null;
	let snapshot: Container[] | null = null;
	let keySnapshot: Container[] | null = null;
	let captureEl: HTMLElement | null = null;
	let pointerId: number | null = null;
	let grabY = 0;
	let baseTop = 0;

	const signature = (cs: Container[]) => JSON.stringify(toOrder(cs));

	function commit() {
		sent = signature(containers);
		oncommit(toOrder(containers));
	}

	// A rollback is invisible on screen readers otherwise: the list simply snaps
	// back. `busy` going false is the parent's signal that the request settled, so
	// that is when we compare what landed against what we sent.
	$effect(() => {
		if (busy || sent === null) return;
		if (untrack(() => signature(containers)) !== sent) announcement = t('layout.reverted');
		sent = null;
	});

	function announceMonitor(key: MessageKey, ci: number, mi: number) {
		const c = containers[ci];
		announcement = t(key, {
			name: c.monitors[mi]?.name ?? '',
			index: mi + 1,
			total: c.monitors.length,
			container: c.name
		});
	}

	function announceGroup(key: MessageKey, ci: number) {
		// containers[0] is the ungrouped block, so a group's display index is ci.
		announcement = t(key, {
			name: containers[ci]?.name ?? '',
			index: ci,
			total: containers.length - 1
		});
	}

	/* ---------- geometry ---------- */

	// All measurements use offsetTop/offsetHeight rather than getBoundingClientRect
	// because both `animate:glassFlip` and the live drag apply transforms, which
	// move the client rect but never the layout box. `.sorter` is the only
	// positioned ancestor, so offsetTop is already relative to it.
	function localY(clientY: number): number {
		if (!root) return clientY;
		return clientY - root.getBoundingClientRect().top + root.scrollTop;
	}

	function itemAt(pos: Position): HTMLElement | null {
		const sel =
			pos.kind === 'group'
				? `[data-sortable-kind="group"][data-ci="${pos.ci}"]`
				: `[data-sortable-kind="monitor"][data-ci="${pos.ci}"][data-mi="${pos.mi}"]`;
		return root?.querySelector<HTMLElement>(sel) ?? null;
	}

	/** Insertion point among the groups, or null when there is nothing to hit. */
	function hitGroup(y: number): number | null {
		const els = root?.querySelectorAll<HTMLElement>('[data-sortable-kind="group"]') ?? [];
		let p: number | null = null;
		for (const el of els) {
			const ci = Number(el.dataset.ci);
			if (ci === 0) continue; // the ungrouped block is pinned to the top
			if (y > el.offsetTop + el.offsetHeight / 2) p = ci + 1;
			else if (p === null) p = ci;
		}
		return p;
	}

	/** Target container plus insertion point within it, in pre-move indices.
	 *
	 *  Walk groups and their rows in document order. Reaching a group's box seeds
	 *  the target at that group's top; the group's own rows then refine the index
	 *  as the pointer passes their midpoints, and the next group's box reclaims the
	 *  target for itself. That turns every group's full vertical extent — header,
	 *  rows and padding — into one continuous drop band with no dead zones, which
	 *  is how a monitor reaches another group (drop anywhere over it) and the very
	 *  top of a group (drop over its header). */
	function hitMonitor(y: number): { ci: number; p: number } | null {
		const els =
			root?.querySelectorAll<HTMLElement>(
				'[data-sortable-kind="group"],[data-sortable-kind="monitor"],[data-sortable-kind="slot"]'
			) ?? [];
		let hit: { ci: number; p: number } | null = null;
		for (const el of els) {
			const ci = Number(el.dataset.ci);
			const kind = el.dataset.sortableKind;
			if (kind === 'group' || kind === 'slot') {
				// A group's box (or an empty group's slot) claims the top of that
				// container; its own rows below refine the index from there.
				if (y > el.offsetTop) hit = { ci, p: 0 };
				continue;
			}
			const mi = Number(el.dataset.mi);
			if (y > el.offsetTop + el.offsetHeight / 2) hit = { ci, p: mi + 1 };
			else if (hit === null) hit = { ci, p: mi };
		}
		return hit;
	}

	/* ---------- pointer path ---------- */

	function onGripDown(e: PointerEvent, kind: Kind, ci: number, mi: number) {
		if (busy || grabbed) return;
		if (e.pointerType === 'mouse' && e.button !== 0) return;
		captureEl = e.currentTarget as HTMLElement;
		captureEl.setPointerCapture(e.pointerId);
		pointerId = e.pointerId;
		origin = { kind, ci, mi };
		grabY = localY(e.clientY);
		baseTop = itemAt(origin)?.offsetTop ?? 0;
	}

	function onGripMove(e: PointerEvent) {
		if (pointerId !== e.pointerId || !origin) return;
		const y = localY(e.clientY);
		if (!dragging) {
			// Threshold, so a plain click or a keyboard-style tap on the grip is not
			// swallowed by the drag.
			if (Math.abs(y - grabY) <= 4) return;
			snapshot = $state.snapshot(containers) as Container[];
			dragging = { ...origin };
		}
		reorderTo(y);
		// Re-derive the offset from the item's *current* slot, so the row stays under
		// the pointer after it has been reordered out from under itself.
		const cur = itemAt(dragging)?.offsetTop ?? baseTop;
		lift.target = { y: y - grabY - (cur - baseTop) };
	}

	function reorderTo(y: number) {
		const d = dragging;
		if (!d) return;
		if (d.kind === 'group') {
			const p = hitGroup(y);
			if (p === null) return;
			// moveGroup splices after the removal, so an insertion point below the
			// item's own slot shifts down by one.
			const to = p > d.ci ? p - 1 : p;
			// The primitives clamp and return the input on a no-op, so identity is the
			// authority on whether the item actually moved.
			const next = moveGroup(containers, d.ci, to);
			if (next === containers) return;
			flushSync(() => (containers = next));
			dragging = { ...d, ci: to };
			return;
		}
		const hit = hitMonitor(y);
		if (!hit) return;
		const toI = hit.ci === d.ci && hit.p > d.mi ? hit.p - 1 : hit.p;
		const next = moveMonitor(containers, d.ci, d.mi, hit.ci, toI);
		if (next === containers) return;
		// flushSync so the offsetTop reads above and below see the new layout; Svelte
		// would otherwise not have rendered until the next microtask.
		flushSync(() => (containers = next));
		dragging = { ...d, ci: hit.ci, mi: toI };
	}

	function releasePointer() {
		if (captureEl && pointerId !== null && captureEl.hasPointerCapture(pointerId)) {
			captureEl.releasePointerCapture(pointerId);
		}
		captureEl = null;
		pointerId = null;
		origin = null;
		lift.set({ y: 0 }, { instant: true });
	}

	function onGripUp(e: PointerEvent) {
		if (pointerId !== e.pointerId) return;
		const dropped = dragging;
		const snap = snapshot;
		dragging = null;
		snapshot = null;
		releasePointer();
		if (!dropped || !snap) return; // a tap, never a drag
		// A drag that ends where it started must not fire a PATCH.
		if (signature(containers) === signature(snap)) return;
		if (dropped.kind === 'group') announceGroup('layout.dropped', dropped.ci);
		else announceMonitor('layout.dropped', dropped.ci, dropped.mi);
		commit();
	}

	function abortDrag() {
		if (!dragging || !snapshot) return;
		const name =
			dragging.kind === 'group'
				? (containers[dragging.ci]?.name ?? '')
				: (containers[dragging.ci]?.monitors[dragging.mi]?.name ?? '');
		containers = snapshot;
		dragging = null;
		snapshot = null;
		releasePointer();
		announcement = t('layout.cancelled', { name });
	}

	/* ---------- keyboard path ---------- */

	function focusGrip(key: string) {
		// tick(), not queueMicrotask: the grip only exists at its new slot once
		// Svelte has flushed, and a keyed move can drop focus in the process.
		tick().then(() => root?.querySelector<HTMLElement>(`[data-sortable-id="${key}"]`)?.focus());
	}

	function gripKey(kind: Kind, ci: number, mi: number): string {
		return kind === 'group'
			? `g:${containers[ci]?.id ?? ''}`
			: `m:${containers[ci]?.monitors[mi]?.id ?? ''}`;
	}

	function grab(kind: Kind, ci: number, mi: number) {
		keySnapshot = $state.snapshot(containers) as Container[];
		const name =
			kind === 'group' ? (containers[ci]?.name ?? '') : (containers[ci]?.monitors[mi]?.name ?? '');
		grabbed = { kind, ci, mi, name, key: gripKey(kind, ci, mi) };
		if (kind === 'group') announceGroup('layout.grabbedGroup', ci);
		else announceMonitor('layout.grabbedMonitor', ci, mi);
	}

	function drop() {
		const g = grabbed;
		const snap = keySnapshot;
		if (!g || !snap) return;
		grabbed = null;
		keySnapshot = null;
		if (g.kind === 'group') announceGroup('layout.dropped', g.ci);
		else announceMonitor('layout.dropped', g.ci, g.mi);
		if (signature(containers) !== signature(snap)) commit();
	}

	function cancelGrab() {
		const g = grabbed;
		const snap = keySnapshot;
		if (!g || !snap) return;
		containers = snap;
		grabbed = null;
		keySnapshot = null;
		announcement = t('layout.cancelled', { name: g.name });
		focusGrip(g.key);
	}

	/** Commit a keyboard move, but only if the primitive actually reordered —
	 *  it returns its input unchanged on a clamped no-op. */
	function settle(
		g: Position & { name: string; key: string },
		next: Container[],
		ci: number,
		mi: number
	) {
		if (next === containers) return;
		containers = next;
		grabbed = { ...g, ci, mi };
		if (g.kind === 'group') announceGroup('layout.movedGroup', ci);
		else announceMonitor('layout.movedMonitor', ci, mi);
		focusGrip(g.key);
	}

	function step(delta: -1 | 1) {
		const g = grabbed;
		if (!g) return;
		if (g.kind === 'group') {
			const to = g.ci + delta;
			if (to < 1 || to > containers.length - 1) return;
			settle(g, moveGroup(containers, g.ci, to), to, g.mi);
			return;
		}
		const rows = containers[g.ci].monitors.length;
		let toC = g.ci;
		let toI = g.mi + delta;
		if (toI < 0) {
			// Stepping off the top of a group crosses into the one above, appended at
			// its end — this is the keyboard's only route between containers.
			if (g.ci === 0) return;
			toC = g.ci - 1;
			toI = containers[toC].monitors.length;
		} else if (toI > rows - 1) {
			if (g.ci === containers.length - 1) return;
			toC = g.ci + 1;
			toI = 0;
		}
		settle(g, moveMonitor(containers, g.ci, g.mi, toC, toI), toC, toI);
	}

	function jump(edge: 'start' | 'end') {
		const g = grabbed;
		if (!g) return;
		if (g.kind === 'group') {
			const to = edge === 'start' ? 1 : containers.length - 1;
			if (to === g.ci) return;
			settle(g, moveGroup(containers, g.ci, to), to, g.mi);
			return;
		}
		const toI = edge === 'start' ? 0 : containers[g.ci].monitors.length - 1;
		if (toI === g.mi) return;
		settle(g, moveMonitor(containers, g.ci, g.mi, g.ci, toI), g.ci, toI);
	}

	function onGripKey(e: KeyboardEvent, kind: Kind, ci: number, mi: number) {
		if (dragging) return;
		// Escape stays live while busy so a grabbed row is never trapped by an
		// in-flight commit.
		if (e.key === 'Escape' && grabbed) {
			e.preventDefault();
			cancelGrab();
			return;
		}
		if (busy) return;
		if (e.key === ' ' || e.key === 'Enter') {
			e.preventDefault();
			if (grabbed) drop();
			else grab(kind, ci, mi);
			return;
		}
		if (!grabbed) return;
		// preventDefault throughout, or the surrounding modal scrolls instead.
		if (e.key === 'ArrowUp') {
			e.preventDefault();
			step(-1);
		} else if (e.key === 'ArrowDown') {
			e.preventDefault();
			step(1);
		} else if (e.key === 'Home') {
			e.preventDefault();
			jump('start');
		} else if (e.key === 'End') {
			e.preventDefault();
			jump('end');
		}
	}

	function onGripBlur() {
		if (!grabbed) return;
		// A move re-focuses the grip at its new slot, which fires a blur first. Wait
		// a turn and only cancel if focus really left.
		const key = grabbed.key;
		tick().then(() => {
			if (!grabbed || grabbed.key !== key) return;
			const el = root?.querySelector<HTMLElement>(`[data-sortable-id="${key}"]`);
			if (el && document.activeElement === el) return;
			cancelGrab();
		});
	}

	function isDragging(kind: Kind, ci: number, mi: number): boolean {
		return (
			!!dragging &&
			dragging.kind === kind &&
			dragging.ci === ci &&
			(kind === 'group' || dragging.mi === mi)
		);
	}

	function transformFor(kind: Kind, ci: number, mi: number): string | undefined {
		// The lift-scale sells the "picked up" feel; it rides on .lift, which no
		// measurement reads (all geometry is the <li>'s offsetTop), so it is purely
		// cosmetic and cannot skew the drop math.
		return isDragging(kind, ci, mi) ? `translateY(${lift.current.y}px) scale(1.015)` : undefined;
	}
</script>

<svelte:window
	onkeydown={(e) => {
		if (e.key === 'Escape' && dragging) {
			e.preventDefault();
			abortDrag();
		}
	}}
/>

<div class="sorter" bind:this={root}>
	<p id={instrId} class="sr-only">{t('layout.instructions')}</p>

	<ul class="groups">
		{#each containers as c, ci (c.id ?? '__ungrouped')}
			<li
				class="group"
				class:dragging={isDragging('group', ci, 0)}
				class:receiving={!!dragging && dragging.kind === 'monitor' && dragging.ci === ci}
				data-sortable-kind="group"
				data-ci={ci}
				animate:glassFlip={{ duration: safeDuration(duration.sm) }}
			>
				<!-- The FLIP animation transforms this <li>; the drag transform lives on
				     the inner .lift, so the two never write the same property. -->
				<div class="lift" style:transform={transformFor('group', ci, 0)}>
					<div class="head">
						{#if ci > 0}
							<DragGrip
								label={t('layout.dragGroup', { name: c.name })}
								grabbed={grabbed?.kind === 'group' && grabbed.ci === ci}
								inert={busy}
								aria-describedby={instrId}
								data-sortable-id={`g:${c.id}`}
								onpointerdown={(e) => onGripDown(e, 'group', ci, 0)}
								onpointermove={onGripMove}
								onpointerup={onGripUp}
								onpointercancel={abortDrag}
								onkeydown={(e) => onGripKey(e, 'group', ci, 0)}
								onblur={onGripBlur}
							/>
						{/if}
						<h4 id={headId(ci)}>{c.name}</h4>
					</div>

					<ul class="monitors" aria-labelledby={headId(ci)}>
						{#each c.monitors as m, mi (m.id)}
							<li
								class="row"
								class:dragging={isDragging('monitor', ci, mi)}
								data-sortable-kind="monitor"
								data-ci={ci}
								data-mi={mi}
								animate:glassFlip={{ duration: safeDuration(duration.sm) }}
							>
								<div class="lift" style:transform={transformFor('monitor', ci, mi)}>
									<DragGrip
										label={t('layout.dragMonitor', { name: m.name })}
										grabbed={grabbed?.kind === 'monitor' && grabbed.ci === ci && grabbed.mi === mi}
										inert={busy}
										aria-describedby={instrId}
										data-sortable-id={`m:${m.id}`}
										onpointerdown={(e) => onGripDown(e, 'monitor', ci, mi)}
										onpointermove={onGripMove}
										onpointerup={onGripUp}
										onpointercancel={abortDrag}
										onkeydown={(e) => onGripKey(e, 'monitor', ci, mi)}
										onblur={onGripBlur}
									/>
									<span class="name">{m.name}</span>
								</div>
							</li>
						{:else}
							<!-- Without a rendered row an empty group has no drop target. -->
							<li
								class="slot"
								class:active={!!dragging && dragging.kind === 'monitor'}
								data-sortable-kind="slot"
								data-ci={ci}
								data-mi="0"
							>
								{t('layout.emptyGroup')}
							</li>
						{/each}
					</ul>
				</div>
			</li>
		{/each}
	</ul>

	<div class="sr-only" aria-live="polite" aria-atomic="true">{announcement}</div>
</div>

<style>
	/* The only positioned element in the subtree: every offsetTop measured during a
	   drag resolves against it. Do not add `position` to the rows or groups. */
	.sorter {
		position: relative;
	}

	.groups,
	.monitors {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}

	.groups {
		gap: var(--space-4);
	}

	.group > .lift {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		padding: var(--space-3);
		border: 1px solid var(--glass-border);
		border-radius: var(--radius-squircle-md);
		background: var(--glass-bg-thin);
	}

	.head {
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}

	.head h4 {
		margin: 0;
		font-size: var(--text-footnote);
		font-weight: var(--weight-semibold);
		letter-spacing: var(--tracking-wide);
		text-transform: uppercase;
		color: var(--color-text-secondary);
	}

	.row > .lift {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-2);
		border: 1px solid var(--glass-border);
		border-radius: var(--radius-squircle-sm);
		background: var(--glass-bg-regular);
		box-shadow: var(--elevation-glass-1);
	}

	.name {
		font-size: var(--text-callout);
		color: var(--color-text-primary);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	/* position/z-index go on the <li>, never on .lift — .lift wraps the measured
	   descendants and would otherwise become their offsetParent. */
	.dragging {
		position: relative;
		z-index: 1;
	}

	.dragging > .lift {
		opacity: 0.9;
		box-shadow: var(--elevation-glass-3);
		border-color: var(--glass-border-strong);
	}

	/* The lifted content rides away on .lift while the <li> stays in flow, so a
	   dashed placeholder painted on the <li> marks exactly where the item drops. */
	.row.dragging {
		background: var(--color-accent-tint);
		border: 1px dashed var(--color-accent);
		border-radius: var(--radius-squircle-sm);
	}
	.group.dragging {
		background: var(--color-accent-tint);
		border: 1px dashed var(--color-accent);
		border-radius: var(--radius-squircle-md);
	}

	/* The group a monitor is currently over — the drop-target indicator that makes
	   a cross-group move legible. */
	.group.receiving > .lift {
		border-color: var(--color-accent);
		box-shadow: 0 0 0 1px var(--color-accent-tint);
	}

	.slot {
		padding: var(--space-3);
		border: 1px dashed var(--glass-border-strong);
		border-radius: var(--radius-squircle-sm);
		font-size: var(--text-footnote);
		color: var(--color-text-tertiary);
		text-align: center;
		transition:
			border-color var(--duration-sm) ease,
			color var(--duration-sm) ease,
			background-color var(--duration-sm) ease;
	}

	/* An empty group invites a drop while any monitor is in flight. */
	.slot.active {
		border-color: var(--color-accent);
		color: var(--color-accent);
		background: var(--color-accent-tint);
	}

	/* Visually hidden, still announced. */
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
		.row > .lift,
		.group > .lift {
			transition: none;
		}
	}
</style>
