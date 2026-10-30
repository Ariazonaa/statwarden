/**
 * Statwarden Design System — Liquid Glass.
 * Tokens, theme, motion and primitives. See /styleguide for a live preview.
 */

// Tokens
export * from './tokens/tokens.js';

// Theme
export { theme, type ThemePreference } from './theme/theme.svelte.js';

// Motion
export {
	springPresets,
	createSpring,
	safeDuration,
	glassFlip,
	type SpringPreset
} from './motion/motion.svelte.js';

// Primitives
export { default as AppBackground } from './components/AppBackground.svelte';
export { default as GlassPanel } from './components/GlassPanel.svelte';
export { default as GlassCard } from './components/GlassCard.svelte';
export { default as PillButton } from './components/PillButton.svelte';
export { default as PillNav } from './components/PillNav.svelte';
export { default as PillSearchField } from './components/PillSearchField.svelte';
export { default as PillToggle } from './components/PillToggle.svelte';
export { default as SegmentedControl } from './components/SegmentedControl.svelte';
export { default as Squircle } from './components/Squircle.svelte';
export { default as StatusBadge } from './components/StatusBadge.svelte';
export { default as StatusPill } from './components/StatusPill.svelte';
export { default as Modal } from './components/Modal.svelte';
export { default as ToastHost } from './components/ToastHost.svelte';
export { toast } from './components/toast.svelte.js';
export { default as Tabs } from './components/Tabs.svelte';
export { default as Tooltip } from './components/Tooltip.svelte';
export { default as Select } from './components/Select.svelte';
export { default as TextField } from './components/TextField.svelte';
export { default as TextArea } from './components/TextArea.svelte';
export { default as Checkbox } from './components/Checkbox.svelte';
export { default as Radio } from './components/Radio.svelte';
export { default as Avatar } from './components/Avatar.svelte';
export { default as Skeleton } from './components/Skeleton.svelte';
export { default as EmptyState } from './components/EmptyState.svelte';
export { default as Kbd } from './components/Kbd.svelte';
export { default as CommandPalette, type Command } from './components/CommandPalette.svelte';
export { default as ColorField } from './components/ColorField.svelte';
export { default as ImageDropzone } from './components/ImageDropzone.svelte';
export { default as DragGrip } from './components/DragGrip.svelte';

// Charts
export { default as Sparkline } from './charts/Sparkline.svelte';
export { default as LineChart } from './charts/LineChart.svelte';
export { default as HeatmapBars, type HeatmapBar } from './charts/HeatmapBars.svelte';
export { default as HeartbeatBar } from './charts/HeartbeatBar.svelte';
