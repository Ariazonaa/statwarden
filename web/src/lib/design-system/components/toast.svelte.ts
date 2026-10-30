/**
 * Toast store — imperative API for short-lived notification pills.
 * Render <ToastHost /> once in the app shell, then call
 * toast.success('Saved') from anywhere.
 */

export type ToastKind = 'info' | 'success' | 'error';

export interface ToastItem {
	id: number;
	kind: ToastKind;
	message: string;
	duration: number;
}

let nextId = 0;
const items = $state<ToastItem[]>([]);
const timers = new Map<number, ReturnType<typeof setTimeout>>();

function push(kind: ToastKind, message: string, duration = 4200): number {
	const id = ++nextId;
	items.push({ id, kind, message, duration });
	timers.set(
		id,
		setTimeout(() => dismiss(id), duration)
	);
	return id;
}

export function dismiss(id: number): void {
	const idx = items.findIndex((t) => t.id === id);
	if (idx !== -1) items.splice(idx, 1);
	const timer = timers.get(id);
	if (timer) clearTimeout(timer);
	timers.delete(id);
}

export const toast = {
	get items(): readonly ToastItem[] {
		return items;
	},
	show: push,
	info: (message: string, duration?: number) => push('info', message, duration),
	success: (message: string, duration?: number) => push('success', message, duration),
	error: (message: string, duration?: number) => push('error', message, duration),
	dismiss
};
