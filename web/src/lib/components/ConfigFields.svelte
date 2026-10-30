<script lang="ts">
	// Renders one monitor-type or notification-type config schema from
	// $lib/api/registry.ts. Both the monitor form and the notification channel
	// editor mount this, so a widget the registry needs (selects, toggles, the
	// list/map/JSON shapes) exists in exactly one place and cannot be present in
	// one form and missing from the other.
	//
	// `config` holds FORM state, not wire state: shaped fields (arrays, maps,
	// JSON) live here as the text the user is editing. `configFromForm` converts
	// on submit — nothing here should post `config` directly.
	import { isFieldVisible, type FieldSpec } from '$lib/api';
	import { t } from '$lib/i18n';
	import { Select, TextArea, TextField, PillToggle } from '$lib/design-system';

	let {
		fields,
		config = $bindable(),
		errors = {}
	}: {
		fields: FieldSpec[];
		config: Record<string, unknown>;
		errors?: Record<string, string>;
	} = $props();

	// Seed defaults for fields the current type has not filled in yet. Runs on
	// every type switch, which is what makes a `showIf` on a defaulted select
	// (SNMP's version, gRPC's tls) resolve on first render.
	$effect(() => {
		for (const f of fields) {
			if (config[f.key] === undefined && f.default !== undefined) config[f.key] = f.default;
		}
	});

	const visible = (f: FieldSpec) => isFieldVisible(f, config);

	const options = (f: FieldSpec) =>
		(f.options ?? []).map((o) => ({ value: o.value, label: o.labelKey ? t(o.labelKey) : o.label }));

	const text = (f: FieldSpec) => String(config[f.key] ?? '');
</script>

{#each fields as f (f.key)}
	{#if visible(f)}
		{#if f.type === 'boolean'}
			<div class="fullrow">
				<PillToggle
					label={t(f.label)}
					checked={Boolean(config[f.key])}
					onchange={(v) => (config[f.key] = v)}
				/>
			</div>
		{:else if f.type === 'select'}
			<Select
				label={t(f.label)}
				value={text(f)}
				options={options(f)}
				onchange={(v) => (config[f.key] = v)}
			/>
		{:else if f.type === 'textarea'}
			<div class="fullrow">
				<TextArea
					label={t(f.label)}
					value={text(f)}
					placeholder={f.placeholder}
					hint={f.hint ? t(f.hint) : undefined}
					error={errors[`cfg_${f.key}`]}
					oninput={(e) => (config[f.key] = (e.currentTarget as HTMLTextAreaElement).value)}
				/>
			</div>
		{:else}
			<TextField
				label={t(f.label)}
				type={f.type === 'number' ? 'number' : f.type === 'password' ? 'password' : 'text'}
				value={text(f)}
				placeholder={f.placeholder}
				hint={f.hint ? t(f.hint) : undefined}
				error={errors[`cfg_${f.key}`]}
				min={f.min}
				max={f.max}
				oninput={(e) => {
					const v = (e.currentTarget as HTMLInputElement).value;
					// Keep '' as '' rather than coercing to 0: configFromForm drops
					// empty strings, which is how an optional number stays absent.
					config[f.key] = f.type === 'number' && v !== '' ? Number(v) : v;
				}}
			/>
		{/if}
	{/if}
{/each}

<style>
	.fullrow {
		grid-column: 1 / -1;
	}
</style>
