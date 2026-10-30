/**
 * Theme store — dark only.
 *
 * Statwarden ships a single dark, graphite "Liquid Glass" look. There is no light
 * theme and no toggle. This shim keeps the historical `theme` shape so callers
 * (charts, status colours, anything asking `theme.isDark`) keep working, but it
 * always resolves to dark. <html data-theme="dark"> is stamped by the inline
 * script in app.html before first paint.
 */
export type ThemePreference = 'dark';
export type ResolvedTheme = 'dark';

export const theme = {
	get preference(): ThemePreference {
		return 'dark';
	},
	get resolved(): ResolvedTheme {
		return 'dark';
	},
	get isDark(): boolean {
		return true;
	},
	/** No-op: the app is dark only. Kept so existing callers do not break. */
	set(_pref?: ThemePreference): void {},
	/** No-op: there is nothing to toggle. */
	toggle(): void {}
};
