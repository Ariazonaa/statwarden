<script lang="ts">
	// Internal styleguide — every token group and primitive in both
	// themes. Dev-facing visual QA surface for the design system.
	import {
		AppBackground,
		Avatar,
		Checkbox,
		ColorField,
		CommandPalette,
		DragGrip,
		EmptyState,
		GlassCard,
		GlassPanel,
		HeartbeatBar,
		HeatmapBars,
		ImageDropzone,
		Kbd,
		LineChart,
		Modal,
		PillButton,
		PillNav,
		PillSearchField,
		PillToggle,
		Radio,
		SegmentedControl,
		Select,
		Skeleton,
		Sparkline,
		Squircle,
		StatusBadge,
		StatusPill,
		Tabs,
		TextArea,
		TextField,
		ToastHost,
		Tooltip,
		glassFlip,
		monitorStatuses,
		theme,
		toast,
		type Command,
		type HeatmapBar,
		type MonitorStatus,
		type ThemePreference
	} from '$lib/design-system';
	import LayoutSorter from '$lib/components/statuspage/LayoutSorter.svelte';
	import type { Container } from '$lib/components/statuspage/layout';

	// ---- theme control ------------------------------------------------
	const themeSegments = [
		{ value: 'light', label: 'Light' },
		{ value: 'system', label: 'System' },
		{ value: 'dark', label: 'Dark' }
	];
	let themePref = $state<ThemePreference>(theme.preference);
	$effect(() => {
		theme.set(themePref);
	});

	// ---- demo state ---------------------------------------------------
	let navActive = $state('monitors');
	let segValue = $state('24h');
	let search = $state('');
	let toggleA = $state(true);
	let toggleB = $state(false);
	let modalOpen = $state(false);
	let paletteOpen = $state(false);
	let selectValue = $state('60');
	let checkA = $state(true);
	let checkB = $state(false);
	let radioValue = $state('http');
	let textValue = $state('');
	let areaValue = $state('');
	let tabActive = $state('overview');
	let btnLoading = $state(false);
	let squircleSmoothing = $state('0.6');

	function fakeLoad() {
		btnLoading = true;
		setTimeout(() => (btnLoading = false), 1600);
	}

	// FLIP demo list
	let flipItems = $state([
		{ id: 'api', label: 'API', status: 'up' },
		{ id: 'web', label: 'Website', status: 'up' },
		{ id: 'db', label: 'Database', status: 'up' },
		{ id: 'mail', label: 'Mail relay', status: 'pending' }
	]);
	function shuffleFlip() {
		flipItems = [...flipItems].sort(() => Math.random() - 0.5);
	}

	// Chart demo data
	const sparkData = Array.from({ length: 40 }, (_, i) => 80 + Math.sin(i / 4) * 25 + (i % 7) * 4);
	const now = Math.floor(Date.now() / 1000);
	const lineX = Array.from({ length: 96 }, (_, i) => now - (96 - i) * 900);
	const lineY = lineX.map(
		(_, i) => 120 + Math.sin(i / 8) * 40 + Math.cos(i / 3) * 15 + (i % 11) * 3
	);
	const heatBars: HeatmapBar[] = Array.from({ length: 60 }, (_, i) => {
		const day = new Date(Date.now() - (59 - i) * 86400000);
		const label = day.toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
		if (i === 41) return { label, status: 'down', detail: '92.1 % uptime' };
		if (i === 42) return { label, status: 'maintenance', detail: 'planned window' };
		if (i > 55) return { label, status: 'up', detail: '100 % uptime' };
		return { label, status: 'up', detail: '99.98 % uptime' };
	});

	const commands: Command[] = [
		{
			id: 'new',
			label: 'New monitor…',
			group: 'Monitors',
			kbd: 'N',
			action: () => toast.success('New monitor')
		},
		{
			id: 'pause',
			label: 'Pause all monitors',
			group: 'Monitors',
			action: () => toast.info('Paused')
		},
		{
			id: 'status',
			label: 'Open status page',
			group: 'Navigate',
			action: () => toast.info('Status page')
		},
		{
			id: 'settings',
			label: 'Settings',
			group: 'Navigate',
			hint: 'General',
			action: () => toast.info('Settings')
		},
		{
			id: 'theme',
			label: 'Toggle theme',
			group: 'Appearance',
			kbd: 'T',
			action: () => theme.toggle()
		}
	];

	const spacingSteps = [1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11];
	const typeRamp = [
		{ name: 'large-title', css: 'var(--text-large-title)', weight: 700, sample: 'Large Title' },
		{ name: 'title-1', css: 'var(--text-title-1)', weight: 700, sample: 'Title 1' },
		{ name: 'title-2', css: 'var(--text-title-2)', weight: 600, sample: 'Title 2' },
		{ name: 'title-3', css: 'var(--text-title-3)', weight: 600, sample: 'Title 3' },
		{ name: 'headline', css: 'var(--text-headline)', weight: 600, sample: 'Headline' },
		{
			name: 'body',
			css: 'var(--text-body)',
			weight: 400,
			sample: 'Body — the default reading size'
		},
		{ name: 'callout', css: 'var(--text-callout)', weight: 400, sample: 'Callout' },
		{ name: 'subheadline', css: 'var(--text-subheadline)', weight: 400, sample: 'Subheadline' },
		{ name: 'footnote', css: 'var(--text-footnote)', weight: 400, sample: 'Footnote' },
		{ name: 'caption-1', css: 'var(--text-caption-1)', weight: 400, sample: 'Caption 1' },
		{ name: 'caption-2', css: 'var(--text-caption-2)', weight: 400, sample: 'Caption 2' }
	];
	const semanticSwatches = [
		{ name: 'text-primary', varName: '--color-text-primary' },
		{ name: 'text-secondary', varName: '--color-text-secondary' },
		{ name: 'text-tertiary', varName: '--color-text-tertiary' },
		{ name: 'accent', varName: '--color-accent' },
		{ name: 'danger', varName: '--color-danger' },
		{ name: 'bg-base', varName: '--color-bg-base' }
	];
	const easings = [
		{ name: 'spring', css: 'var(--ease-spring)' },
		{ name: 'spring-soft', css: 'var(--ease-spring-soft)' },
		{ name: 'out-expo', css: 'var(--ease-out-expo)' },
		{ name: 'in-out', css: 'var(--ease-in-out)' }
	];
	let easeDemo = $state(false);

	// Branding demo. A data: URI keeps the populated preview self-contained —
	// the real editor renders a server-issued /uploads/logos/… path.
	const sampleLogo =
		"data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' width='120' height='40'%3E%3Crect width='120' height='40' rx='8' fill='rgb(79,70,229)'/%3E%3Ctext x='60' y='25' font-family='sans-serif' font-size='15' font-weight='700' fill='white' text-anchor='middle'%3EStatwarden%3C/text%3E%3C/svg%3E";
	let accent = $state('#0ea5e9');

	// Layout sorter demo. containers[0] is the ungrouped pseudo-group; the empty
	// group exercises the drop slot and the second group makes group reordering
	// reachable (moveGroup needs two real groups).
	let sorterLayout = $state<Container[]>([
		{
			id: null,
			name: 'Ungrouped',
			monitors: [
				{ id: 'm1', name: 'API' },
				{ id: 'm2', name: 'Website' }
			]
		},
		{ id: 'g1', name: 'Core', monitors: [{ id: 'm3', name: 'Database' }] },
		{ id: 'g2', name: 'Delivery', monitors: [] }
	]);

	// HeartbeatBar sample: steady, one retry, a short outage, recovered.
	const sgBeats: MonitorStatus[] = [
		...Array<MonitorStatus>(30).fill('up'),
		'pending',
		'down',
		'down',
		'down',
		...Array<MonitorStatus>(10).fill('up'),
		'maintenance',
		'maintenance',
		...Array<MonitorStatus>(4).fill('up')
	];
</script>

<svelte:head>
	<title>Statwarden — Styleguide</title>
</svelte:head>

<AppBackground />
<ToastHost />
<CommandPalette {commands} bind:open={paletteOpen} />

<div class="page">
	<header class="topbar ds-glass ds-glass--thick">
		<h1>Liquid Glass <span class="muted">/ styleguide</span></h1>
		<div class="topbar-controls">
			<PillButton variant="ghost" size="sm" onclick={() => (paletteOpen = true)}>
				Command palette&nbsp;<Kbd>⌘K</Kbd>
			</PillButton>
			<SegmentedControl segments={themeSegments} bind:value={themePref} label="Theme" size="sm" />
		</div>
	</header>

	<!-- ============ TOKENS ============ -->
	<h2 class="section">Tokens</h2>

	<div class="grid two">
		<GlassCard title="Semantic colours" subtitle="light + dark from the same names">
			<div class="swatches">
				{#each semanticSwatches as s (s.name)}
					<div class="swatch">
						<span class="chip" style="background: var({s.varName})"></span>
						<code>{s.name}</code>
					</div>
				{/each}
			</div>
		</GlassCard>

		<GlassCard title="Status colours" subtitle="up / down / pending / maintenance / paused">
			<div class="badges">
				{#each monitorStatuses as s (s)}
					<StatusBadge status={s} />
				{/each}
			</div>
			<div class="badges" style="margin-top: var(--space-4)">
				{#each monitorStatuses as s (s)}
					<StatusBadge status={s} size="sm" pulse={false} />
				{/each}
			</div>
		</GlassCard>
	</div>

	<div class="grid two">
		<GlassCard title="Radius scale" subtitle="pill + squircle steps">
			<div class="radius-row">
				<div class="radius-demo" style="border-radius: var(--radius-squircle-sm)">
					<code>sm</code>
				</div>
				<div class="radius-demo" style="border-radius: var(--radius-squircle-md)">
					<code>md</code>
				</div>
				<div class="radius-demo" style="border-radius: var(--radius-squircle-lg)">
					<code>lg</code>
				</div>
				<div class="radius-demo pillr" style="border-radius: var(--radius-pill)">
					<code>pill</code>
				</div>
			</div>
		</GlassCard>

		<GlassCard title="Surface depth · Elevation" subtitle="thin / regular / thick">
			<div class="depth-row">
				<GlassPanel blur="thin" class="depth-demo"><code>thin</code></GlassPanel>
				<GlassPanel blur="regular" class="depth-demo"><code>regular</code></GlassPanel>
				<GlassPanel blur="thick" class="depth-demo"><code>thick</code></GlassPanel>
			</div>
			<p class="hint-text">
				Each step pairs a slightly lighter surface fill with a deeper elevation recipe (layered
				shadow + inset top-edge highlight). No backdrop blur — the cards sit opaque on the
				pure-black field.
			</p>
		</GlassCard>
	</div>

	<div class="grid two">
		<GlassCard title="Typography" subtitle="SF/system stack with Inter fallback">
			<div class="type-ramp">
				{#each typeRamp as t (t.name)}
					<div class="type-row">
						<code>{t.name}</code>
						<span
							style="font-size: {t.css}; font-weight: {t.weight}; letter-spacing: var(--tracking-tight)"
						>
							{t.sample}
						</span>
					</div>
				{/each}
			</div>
		</GlassCard>

		<div class="stack">
			<GlassCard title="Spacing scale">
				<div class="space-ramp">
					{#each spacingSteps as i (i)}
						<div class="space-row">
							<code>space-{i}</code>
							<span class="space-bar" style="width: var(--space-{i})"></span>
						</div>
					{/each}
				</div>
			</GlassCard>

			<GlassCard title="Motion tokens" subtitle="spring-approximating curves">
				<div class="ease-row">
					{#each easings as e (e.name)}
						<div class="ease-demo">
							<span
								class="ease-ball"
								class:go={easeDemo}
								style="transition-timing-function: {e.css}"
							></span>
							<code>{e.name}</code>
						</div>
					{/each}
				</div>
				<PillButton variant="secondary" size="sm" onclick={() => (easeDemo = !easeDemo)}>
					Play
				</PillButton>
			</GlassCard>
		</div>
	</div>

	<!-- ============ PRIMITIVES ============ -->
	<h2 class="section">Primitives</h2>

	<GlassCard title="PillButton" subtitle="primary / secondary / destructive / ghost · loading">
		<div class="row">
			<PillButton>Primary</PillButton>
			<PillButton variant="secondary">Secondary</PillButton>
			<PillButton variant="destructive">Delete monitor</PillButton>
			<PillButton variant="ghost">Ghost</PillButton>
			<PillButton loading={btnLoading} onclick={fakeLoad}>
				{btnLoading ? 'Saving…' : 'Click to load'}
			</PillButton>
			<PillButton disabled>Disabled</PillButton>
		</div>
		<div class="row" style="margin-top: var(--space-4)">
			<PillButton size="sm">Small</PillButton>
			<PillButton size="md" variant="secondary">Medium</PillButton>
			<PillButton size="lg" variant="secondary">Large</PillButton>
		</div>
	</GlassCard>

	<div class="grid two">
		<GlassCard title="PillNav" subtitle="floating navigation">
			<PillNav
				items={[
					{ id: 'monitors', label: 'Monitors' },
					{ id: 'status', label: 'Status pages' },
					{ id: 'alerts', label: 'Alerts' },
					{ id: 'settings', label: 'Settings' }
				]}
				bind:active={navActive}
			/>
		</GlassCard>

		<GlassCard title="SegmentedControl · Tabs">
			<div class="stack">
				<SegmentedControl
					segments={[
						{ value: '24h', label: '24 h' },
						{ value: '7d', label: '7 d' },
						{ value: '30d', label: '30 d' },
						{ value: '90d', label: '90 d' }
					]}
					bind:value={segValue}
					label="Time range"
				/>
				<Tabs
					tabs={[
						{ id: 'overview', label: 'Overview' },
						{ id: 'checks', label: 'Checks' },
						{ id: 'events', label: 'Events' }
					]}
					bind:active={tabActive}
					label="Monitor detail"
				>
					{#snippet panel(active)}
						<p class="hint-text">Active panel: <code>{active}</code></p>
					{/snippet}
				</Tabs>
			</div>
		</GlassCard>
	</div>

	<div class="grid two">
		<GlassCard title="PillSearchField · Select">
			<div class="stack">
				<PillSearchField bind:value={search} shortcutHint="⌘K" placeholder="Search monitors…" />
				<Select
					label="Check interval"
					bind:value={selectValue}
					options={[
						{ value: '30', label: 'Every 30 seconds' },
						{ value: '60', label: 'Every minute' },
						{ value: '300', label: 'Every 5 minutes' },
						{ value: '900', label: 'Every 15 minutes', disabled: true },
						{ value: '3600', label: 'Every hour' }
					]}
				/>
			</div>
		</GlassCard>

		<GlassCard title="PillToggle · Checkbox · Radio">
			<div class="stack">
				<PillToggle label="Notifications" bind:checked={toggleA} />
				<PillToggle label="Public status page" bind:checked={toggleB} />
				<PillToggle label="Disabled" checked disabled />
				<Checkbox label="Retry before alerting" bind:checked={checkA} />
				<Checkbox label="Follow redirects" bind:checked={checkB} />
				<div class="row">
					<Radio name="probe" value="http" label="HTTP" bind:group={radioValue} />
					<Radio name="probe" value="tcp" label="TCP" bind:group={radioValue} />
					<Radio name="probe" value="ping" label="Ping" bind:group={radioValue} />
				</div>
			</div>
		</GlassCard>
	</div>

	<div class="grid two">
		<GlassCard title="TextField · TextArea">
			<div class="stack">
				<TextField
					label="Monitor name"
					placeholder="My API"
					bind:value={textValue}
					hint="Shown on the status page."
				/>
				<TextField
					label="URL"
					placeholder="https://"
					value="not a url"
					error="Enter a valid URL."
				/>
				<TextArea
					label="Description"
					placeholder="Optional notes…"
					bind:value={areaValue}
					rows={3}
				/>
			</div>
		</GlassCard>

		<div class="stack">
			<GlassCard title="Avatar · Kbd · Tooltip">
				<div class="row center">
					<Avatar name="Jordan Lee" size="lg" status="up" />
					<Avatar name="Jane Doe" status="down" />
					<Avatar name="Statwarden Bot" size="sm" />
					<span class="row center" style="gap: var(--space-2)"><Kbd>⌘</Kbd><Kbd>K</Kbd></span>
					<Tooltip text="Checks run every 60 s from this instance.">
						<PillButton variant="secondary" size="sm">Hover or focus me</PillButton>
					</Tooltip>
				</div>
			</GlassCard>

			<GlassCard title="Skeleton shimmer">
				<div class="row center">
					<Skeleton variant="circle" />
					<div style="flex: 1">
						<Skeleton lines={3} />
					</div>
				</div>
				<Skeleton variant="rect" height="3.5rem" class="mt" />
			</GlassCard>
		</div>
	</div>

	<div class="grid two">
		<GlassCard title="Modal / BottomSheet · Toast · CommandPalette">
			<div class="row">
				<PillButton onclick={() => (modalOpen = true)}>Open modal</PillButton>
				<PillButton variant="secondary" onclick={() => toast.success('Monitor created')}>
					Success toast
				</PillButton>
				<PillButton variant="secondary" onclick={() => toast.error('Probe failed: timeout')}>
					Error toast
				</PillButton>
				<PillButton variant="secondary" onclick={() => (paletteOpen = true)}>⌘K palette</PillButton>
			</div>
			<p class="hint-text">The modal becomes a bottom sheet below 768 px viewport width.</p>
		</GlassCard>

		<GlassCard title="EmptyState">
			<EmptyState
				title="No monitors yet"
				description="Add your first monitor and Statwarden starts watching it within seconds."
			>
				{#snippet icon()}
					<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8">
						<path d="M3 12h4l2.5-6 4 12 2.5-6h5" stroke-linecap="round" stroke-linejoin="round" />
					</svg>
				{/snippet}
				{#snippet action()}
					<PillButton size="sm">New monitor</PillButton>
				{/snippet}
			</EmptyState>
		</GlassCard>
	</div>

	<div class="grid two">
		<GlassCard title="ImageDropzone" subtitle="empty / uploaded / busy / error">
			<div class="stack">
				<ImageDropzone
					label="Logo"
					hint="PNG, JPEG or WebP · at most 2 MiB."
					promptText="Drop a logo here"
					previewAlt="Status page logo"
					onselect={(f) => toast.success(`Selected ${f.name}`)}
				/>
				<ImageDropzone
					label="Logo (uploaded)"
					src={sampleLogo}
					previewAlt="Statwarden wordmark"
					hint="Drop a new file to replace it."
					onselect={(f) => toast.success(`Selected ${f.name}`)}
					onremove={() => toast.info('Logo removed')}
				/>
				<ImageDropzone
					label="Logo (uploading)"
					src={sampleLogo}
					previewAlt="Statwarden wordmark"
					busy
				/>
				<ImageDropzone
					label="Logo (rejected)"
					error="SVG logos are not accepted; upload a PNG, JPEG or WebP."
				/>
			</div>
		</GlassCard>

		<GlassCard title="ColorField" subtitle="native swatch · hex input · presets">
			<div class="stack">
				<ColorField
					label="Accent colour"
					bind:value={accent}
					hint="Overrides the --color-accent family on the public status page."
				/>
				<div class="swatch">
					<span class="chip" style="background: {accent || 'var(--color-accent)'}"></span>
					<code>{accent || '(unset — product accent)'}</code>
				</div>
				<ColorField label="Disabled" value="#10b981" disabled />
			</div>
		</GlassCard>
	</div>

	<div class="grid two">
		<GlassCard title="DragGrip" subtitle="idle / grabbed / inert">
			<div class="row center">
				<DragGrip label="Reorder API" />
				<DragGrip label="Reorder Website" grabbed />
				<DragGrip label="Reorder Database" inert />
			</div>
			<p class="hint-text">
				<code>inert</code> renders <code>aria-disabled</code> rather than the native attribute, so a keyboard
				user does not lose focus while a commit is in flight.
			</p>
		</GlassCard>

		<GlassCard title="LayoutSorter" subtitle="nested pointer + keyboard sorting">
			<LayoutSorter
				bind:containers={sorterLayout}
				oncommit={() => toast.success('Layout order committed')}
			/>
			<p class="hint-text">
				Tab to a grip, <Kbd>Space</Kbd> lifts, <Kbd>↑</Kbd><Kbd>↓</Kbd> move — stepping past a group boundary
				crosses into the next container — then <Kbd>Space</Kbd> drops or
				<Kbd>Esc</Kbd> cancels.
			</p>
		</GlassCard>
	</div>

	<GlassCard
		title="Squircle"
		subtitle="exact superellipse clip-path for hero surfaces · degrades to border-radius"
	>
		{#snippet actions()}
			<SegmentedControl
				segments={[
					{ value: '0', label: 'Circle' },
					{ value: '0.6', label: 'iOS' },
					{ value: '1', label: 'Square' }
				]}
				bind:value={squircleSmoothing}
				label="Corner smoothing"
				size="sm"
			/>
		{/snippet}
		<div class="squircle-demo">
			<Squircle radius={44} smoothing={Number(squircleSmoothing)} class="squircle-hero">
				<div class="squircle-hero-inner">
					<span class="squircle-eyebrow">Hero surface</span>
					<strong>Superellipse clip</strong>
				</div>
			</Squircle>
			<p class="hint-text">
				The gradient fill is clipped to a mathematically exact superellipse — smoothing maps to the
				exponent (<code>n = 2</code> circle → <code>n = 5</code> near-square). Where
				<code>clip-path</code> is unsupported it falls back to the CSS <code>border-radius</code> squircle
				approximation.
			</p>
		</div>
	</GlassCard>

	<!-- ============ CHARTS ============ -->
	<h2 class="section">Charts</h2>

	<div class="grid two">
		<GlassCard title="Sparkline" subtitle="inline trend, accent + status colours">
			<div class="stack">
				<Sparkline data={sparkData} label="Response time trend" />
				<Sparkline
					data={sparkData.map((v) => 200 - v)}
					color="down"
					label="Error trend"
					height={28}
				/>
			</div>
		</GlassCard>

		<GlassCard title="HeatmapBars" subtitle="uptime, last 60 days">
			<HeatmapBars bars={heatBars} label="Uptime, last 60 days" />
		</GlassCard>
	</div>

	<div class="grid two">
		<GlassCard title="HeartbeatBar" subtitle="one pill per check, newest right; clips on the left">
			<div class="stack">
				<HeartbeatBar beats={sgBeats} />
				<HeartbeatBar beats={sgBeats} size="sm" slots={30} />
				<HeartbeatBar beats={['up', 'up', 'pending']} size="sm" slots={30} />
			</div>
		</GlassCard>

		<GlassCard title="StatusPill" subtitle="solid status fill: list uptime, detail state">
			<div class="stack">
				<div class="row">
					{#each monitorStatuses as s (s)}
						<StatusPill status={s} label="99,95 %" />
					{/each}
				</div>
				<div class="row">
					<StatusPill status="up" label="Up" size="lg" />
					<StatusPill status="down" label="Down" size="lg" />
				</div>
			</div>
		</GlassCard>
	</div>

	<GlassCard title="LineChart" subtitle="response-time history with crosshair + glass tooltip">
		<LineChart x={lineX} y={lineY} label="Response time" />
	</GlassCard>

	<!-- ============ SYSTEM BEHAVIOUR ============ -->
	<h2 class="section">System behaviour</h2>

	<div class="grid two">
		<GlassCard title="FLIP list reordering" subtitle="animate:glassFlip">
			{#snippet actions()}
				<PillButton variant="secondary" size="sm" onclick={shuffleFlip}>Shuffle</PillButton>
			{/snippet}
			<ul class="flip-list">
				{#each flipItems as item (item.id)}
					<li class="flip-item ds-glass ds-glass--thin" animate:glassFlip>
						<span>{item.label}</span>
						<StatusBadge status={item.status as 'up' | 'pending'} size="sm" />
					</li>
				{/each}
			</ul>
		</GlassCard>

		<GlassCard title="Fallbacks" subtitle="progressive enhancement">
			<ul class="notes">
				<li>
					Surfaces are solid and opaque by design — there is no <code>backdrop-filter</code> to fall back
					from, so they render identically in every browser.
				</li>
				<li>
					<code>prefers-reduced-motion</code>: springs and the skeleton shimmer are disabled
					globally.
				</li>
				<li>
					Focus rings use a two-layer halo that stays visible on the flat black field — try tabbing
					through this page.
				</li>
				<li>Theme switching cross-fades and persists; “System” follows the OS live.</li>
			</ul>
		</GlassCard>
	</div>
</div>

<Modal bind:open={modalOpen} title="Pause monitor?">
	<p style="margin: 0 0 var(--space-3)">
		Paused monitors stop probing and alerting until resumed. Existing history is kept.
	</p>
	<TextField label="Reason (optional)" placeholder="Planned maintenance…" />
	{#snippet footer()}
		<PillButton variant="ghost" onclick={() => (modalOpen = false)}>Cancel</PillButton>
		<PillButton onclick={() => (modalOpen = false)}>Pause</PillButton>
	{/snippet}
</Modal>

<style>
	.page {
		max-width: 72rem;
		margin: 0 auto;
		padding: var(--space-7) var(--space-6) var(--space-11);
		display: flex;
		flex-direction: column;
		gap: var(--space-6);
	}

	.topbar {
		position: sticky;
		top: var(--space-4);
		z-index: 40;
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-4);
		flex-wrap: wrap;
		padding: var(--space-4) var(--space-6);
		border-radius: var(--radius-squircle-lg);
	}
	.topbar h1 {
		margin: 0;
		font-size: var(--text-title-3);
		font-weight: var(--weight-bold);
		letter-spacing: var(--tracking-tight);
	}
	.muted {
		color: var(--color-text-tertiary);
		font-weight: var(--weight-regular);
	}
	.topbar-controls {
		display: flex;
		align-items: center;
		gap: var(--space-4);
		flex-wrap: wrap;
	}

	.section {
		margin: var(--space-6) 0 0;
		font-size: var(--text-title-2);
		font-weight: var(--weight-bold);
		letter-spacing: var(--tracking-tight);
	}

	.grid.two {
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: var(--space-6);
		align-items: start;
	}
	@media (max-width: 1024px) {
		.grid.two {
			grid-template-columns: 1fr;
		}
	}

	.stack {
		display: flex;
		flex-direction: column;
		gap: var(--space-5);
	}
	.row {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-4);
	}
	.row.center {
		align-items: center;
	}

	code {
		font-family: var(--font-mono);
		font-size: var(--text-caption-1);
		color: var(--color-text-secondary);
	}

	.hint-text {
		margin: var(--space-4) 0 0;
		font-size: var(--text-footnote);
		color: var(--color-text-secondary);
	}

	/* Tokens */
	.swatches {
		display: grid;
		grid-template-columns: repeat(auto-fill, minmax(9rem, 1fr));
		gap: var(--space-4);
	}
	.swatch {
		display: flex;
		align-items: center;
		gap: var(--space-3);
	}
	.chip {
		width: 1.5rem;
		height: 1.5rem;
		border-radius: 0.5rem;
		border: 1px solid var(--hairline);
		box-shadow: inset 0 1px 0 var(--glass-highlight);
		flex-shrink: 0;
	}

	.badges {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-3);
	}

	.radius-row,
	.depth-row {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-5);
		align-items: center;
	}
	.radius-demo {
		display: grid;
		place-items: center;
		width: 5rem;
		height: 5rem;
		background: var(--color-accent-tint);
		border: 1px solid var(--hairline);
	}
	.radius-demo.pillr {
		width: 7rem;
		height: 3rem;
	}
	:global(.depth-demo) {
		display: grid;
		place-items: center;
		width: 7rem;
		height: 5rem;
	}

	.type-ramp {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}
	.type-row {
		display: grid;
		grid-template-columns: 6.5rem 1fr;
		align-items: baseline;
		gap: var(--space-4);
	}
	.type-row span {
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
	}

	.space-ramp {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	.space-row {
		display: grid;
		grid-template-columns: 5rem 1fr;
		align-items: center;
		gap: var(--space-4);
	}
	.space-bar {
		height: 0.625rem;
		border-radius: var(--radius-pill);
		background: var(--color-accent);
		opacity: 0.75;
	}

	.ease-row {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
		margin-bottom: var(--space-5);
	}
	.ease-demo {
		display: grid;
		grid-template-columns: 1fr 6rem;
		align-items: center;
		gap: var(--space-4);
	}
	.ease-ball {
		width: 1rem;
		height: 1rem;
		border-radius: 50%;
		background: var(--color-accent);
		transition-property: transform;
		transition-duration: var(--duration-xl);
		will-change: transform;
	}
	.ease-ball.go {
		transform: translateX(min(14rem, 40vw));
	}
	@media (prefers-reduced-motion: reduce) {
		.ease-ball {
			transition: none;
		}
	}

	:global(.mt) {
		margin-top: var(--space-4);
	}

	/* FLIP demo */
	.flip-list {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}
	.flip-item {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: var(--space-3) var(--space-5);
		border-radius: var(--radius-squircle-sm);
		font-size: var(--text-subheadline);
		font-weight: var(--weight-medium);
	}

	/* Squircle demo */
	.squircle-demo {
		display: flex;
		flex-direction: column;
		gap: var(--space-5);
	}
	:global(.squircle-hero) {
		height: 9rem;
		display: grid;
		place-items: center;
		text-align: center;
		background-image: var(--color-accent-gradient);
		color: var(--color-text-on-accent);
	}
	.squircle-hero-inner {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	.squircle-eyebrow {
		font-size: var(--text-caption-1);
		text-transform: uppercase;
		letter-spacing: var(--tracking-wide);
		opacity: 0.85;
	}
	.squircle-hero-inner strong {
		font-size: var(--text-title-3);
		font-weight: var(--weight-bold);
	}

	.notes {
		margin: 0;
		padding-left: 1.1em;
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
		font-size: var(--text-subheadline);
		line-height: var(--leading-relaxed);
		color: var(--color-text-secondary);
	}
</style>
