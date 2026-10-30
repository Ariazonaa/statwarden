/**
 * ColorField's contract is what it accepts and what it refuses. The committed
 * value is observable without a binding harness: `commit` writes it back into
 * the hex box (syncHex) and the Reset control only renders while a value is
 * set, so the DOM is an honest witness for the bound prop.
 */
import { describe, expect, it } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import ColorField from './ColorField.svelte';

/** The hex text box, addressed by the label the component mirrors onto it. */
const hexBox = () => screen.getByRole('textbox', { name: 'Accent' }) as HTMLInputElement;

/** The native colour input, which has no accessible role. */
function swatch(container: HTMLElement): HTMLInputElement {
	return container.querySelector('input[type="color"]') as HTMLInputElement;
}

/** Commit a typed value the way a user leaving the field would. */
async function typeHex(v: string) {
	await fireEvent.change(hexBox(), { target: { value: v } });
}

const INVALID = 'Enter a hex colour like #4f46e5.';

describe('ColorField', () => {
	it('renders the current value in the hex box and the swatch', () => {
		const { container } = render(ColorField, { props: { label: 'Accent', value: '#0ea5e9' } });
		expect(hexBox().value).toBe('#0ea5e9');
		expect(swatch(container).value).toBe('#0ea5e9');
	});

	it('falls the swatch back to the product accent when nothing is set', () => {
		const { container } = render(ColorField, { props: { label: 'Accent', value: '' } });
		expect(swatch(container).value).toBe('#4f46e5');
	});

	it('lowercases a typed hex', async () => {
		render(ColorField, { props: { label: 'Accent', value: '' } });
		await typeHex('#0EA5E9');
		expect(hexBox().value).toBe('#0ea5e9');
		expect(screen.queryByText(INVALID)).not.toBeInTheDocument();
	});

	it('expands #rgb shorthand the way the server would', async () => {
		// normalizeHexColor in internal/api/logos.go accepts shorthand; refusing it
		// here would reject a value the API takes.
		render(ColorField, { props: { label: 'Accent', value: '' } });
		await typeHex('#F80');
		expect(hexBox().value).toBe('#ff8800');
	});

	it('trims surrounding whitespace before validating', async () => {
		render(ColorField, { props: { label: 'Accent', value: '' } });
		await typeHex('  #10b981  ');
		expect(hexBox().value).toBe('#10b981');
		expect(screen.queryByText(INVALID)).not.toBeInTheDocument();
	});

	it.each(['nope', '#12', '#12345', '#1234567', '1234ab', '#ggghhh'])(
		'refuses %s with an inline error instead of dropping it silently',
		async (bad) => {
			render(ColorField, { props: { label: 'Accent', value: '#0ea5e9' } });
			await typeHex(bad);
			expect(screen.getByText(INVALID)).toBeInTheDocument();
			// The previously committed value survives a rejection.
			expect(hexBox().value).toBe('#0ea5e9');
			expect(hexBox()).toHaveAttribute('aria-invalid', 'true');
		}
	);

	it('clears the inline error once a valid value is typed', async () => {
		render(ColorField, { props: { label: 'Accent', value: '' } });
		await typeHex('nope');
		expect(screen.getByText(INVALID)).toBeInTheDocument();
		await typeHex('#10b981');
		expect(screen.queryByText(INVALID)).not.toBeInTheDocument();
		expect(hexBox()).not.toHaveAttribute('aria-invalid');
	});

	it('treats an emptied box as clearing the value, not as invalid', async () => {
		render(ColorField, { props: { label: 'Accent', value: '#0ea5e9' } });
		await typeHex('');
		expect(screen.queryByText(INVALID)).not.toBeInTheDocument();
		expect(hexBox().value).toBe('');
		expect(screen.queryByRole('button', { name: 'Reset' })).not.toBeInTheDocument();
	});

	it('re-syncs the box when a retyped value normalises to what is already stored', async () => {
		// `value` does not change, so the {value} attribute does not re-render —
		// without the forced sync the typed text stays on screen disagreeing with
		// what was committed.
		render(ColorField, { props: { label: 'Accent', value: '#ff8800' } });
		await typeHex('#F80');
		expect(hexBox().value).toBe('#ff8800');
	});

	it('lets a consumer error win over the internal validation message', async () => {
		render(ColorField, {
			props: { label: 'Accent', value: '', error: 'Reserved for the dark theme.' }
		});
		await typeHex('nope');
		expect(screen.getByText('Reserved for the dark theme.')).toBeInTheDocument();
		expect(screen.queryByText(INVALID)).not.toBeInTheDocument();
	});

	it('commits a preset swatch', async () => {
		const user = userEvent.setup();
		render(ColorField, { props: { label: 'Accent', value: '', presets: ['#10b981'] } });
		await user.click(screen.getByRole('button', { name: '#10b981' }));
		expect(hexBox().value).toBe('#10b981');
	});

	it('offers Reset only while a value is set, and clearing removes it again', async () => {
		const user = userEvent.setup();
		render(ColorField, { props: { label: 'Accent', value: '#10b981' } });
		await user.click(screen.getByRole('button', { name: 'Reset' }));
		expect(hexBox().value).toBe('');
		expect(screen.queryByRole('button', { name: 'Reset' })).not.toBeInTheDocument();
	});

	it('commits what the native colour picker emits', async () => {
		const { container } = render(ColorField, { props: { label: 'Accent', value: '' } });
		await fireEvent.input(swatch(container), { target: { value: '#8b5cf6' } });
		expect(hexBox().value).toBe('#8b5cf6');
	});

	it('points aria-describedby at the hint, then adds the error once one appears', async () => {
		const { container } = render(ColorField, {
			props: { label: 'Accent', value: '', hint: 'Used on the public page.' }
		});
		/** The elements aria-describedby actually resolves to. */
		const described = () =>
			(swatch(container).getAttribute('aria-describedby') ?? '')
				.split(' ')
				.filter(Boolean)
				.map((ref) => document.getElementById(ref));

		expect(described()).toHaveLength(1);
		expect(described()[0]).toHaveTextContent('Used on the public page.');

		await typeHex('nope');
		// The rejection has to be reachable from the input, not just visible.
		expect(described().map((el) => el?.textContent)).toContain(INVALID);
	});

	it('disables every control when disabled', () => {
		const { container } = render(ColorField, {
			props: { label: 'Accent', value: '#10b981', disabled: true, presets: ['#10b981'] }
		});
		expect(hexBox()).toBeDisabled();
		expect(swatch(container)).toBeDisabled();
		expect(screen.getByRole('button', { name: '#10b981' })).toBeDisabled();
		expect(screen.getByRole('button', { name: 'Reset' })).toBeDisabled();
	});
});
