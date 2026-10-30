/**
 * ImageDropzone owns no upload of its own — its contract is when it hands a
 * File to the consumer and how it reports the consumer's error back. The busy
 * state is the interesting half: it must refuse input without ever taking the
 * native disabled attribute, or a keyboard user loses focus mid-upload.
 */
import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import ImageDropzone from './ImageDropzone.svelte';

const png = () => new File(['\x89PNG'], 'logo.png', { type: 'image/png' });

/** The visually hidden file input, addressed through its label. */
const fileInput = () => screen.getByLabelText('Logo') as HTMLInputElement;

/** The drop zone: the only element carrying the drag handlers. */
function zone(container: HTMLElement): HTMLElement {
	return container.querySelector('.zone') as HTMLElement;
}

/** A drop event carrying files — jsdom builds no DataTransfer for us. */
async function drop(el: HTMLElement, files: File[]) {
	await fireEvent.drop(el, { dataTransfer: { files } });
}

describe('ImageDropzone', () => {
	it('shows the prompt with no image and the preview with one', () => {
		const { unmount } = render(ImageDropzone, { props: { label: 'Logo' } });
		expect(screen.getByText('Drop an image here')).toBeInTheDocument();
		expect(screen.queryByRole('img')).not.toBeInTheDocument();
		unmount();

		render(ImageDropzone, {
			props: { label: 'Logo', src: '/uploads/logo.png', previewAlt: 'Acme logo' }
		});
		// A logo is content, not decoration, so it must carry a description.
		expect(screen.getByRole('img', { name: 'Acme logo' })).toHaveAttribute(
			'src',
			'/uploads/logo.png'
		);
	});

	it('hands a dropped file to the consumer', async () => {
		const onselect = vi.fn();
		const { container } = render(ImageDropzone, { props: { label: 'Logo', onselect } });
		const file = png();
		await drop(zone(container), [file]);
		expect(onselect).toHaveBeenCalledExactlyOnceWith(file);
	});

	it('takes only the first of several dropped files', async () => {
		const onselect = vi.fn();
		const { container } = render(ImageDropzone, { props: { label: 'Logo', onselect } });
		const [first, second] = [png(), png()];
		await drop(zone(container), [first, second]);
		expect(onselect).toHaveBeenCalledExactlyOnceWith(first);
	});

	it('ignores a drop that carries no file', async () => {
		const onselect = vi.fn();
		const { container } = render(ImageDropzone, { props: { label: 'Logo', onselect } });
		await drop(zone(container), []);
		await fireEvent.drop(zone(container), { dataTransfer: undefined });
		expect(onselect).not.toHaveBeenCalled();
	});

	it('marks the zone on dragover and unmarks it on dragleave', async () => {
		const { container } = render(ImageDropzone, { props: { label: 'Logo' } });
		await fireEvent.dragOver(zone(container));
		expect(zone(container)).toHaveClass('dragover');
		await fireEvent.dragLeave(zone(container));
		expect(zone(container)).not.toHaveClass('dragover');
	});

	it('clears the dragover mark after a drop', async () => {
		const { container } = render(ImageDropzone, { props: { label: 'Logo' } });
		await fireEvent.dragOver(zone(container));
		await drop(zone(container), [png()]);
		expect(zone(container)).not.toHaveClass('dragover');
	});

	it('hands a picked file over and resets the input so the same file re-fires', async () => {
		const onselect = vi.fn();
		const user = userEvent.setup();
		render(ImageDropzone, { props: { label: 'Logo', onselect } });
		await user.upload(fileInput(), png());
		expect(onselect).toHaveBeenCalledOnce();
		// Without the reset, re-choosing the same file fires no change event.
		expect(fileInput().value).toBe('');
	});

	it('offers Remove only once there is an image', async () => {
		const onremove = vi.fn();
		const user = userEvent.setup();
		const { unmount } = render(ImageDropzone, { props: { label: 'Logo', onremove } });
		expect(screen.queryByRole('button', { name: 'Remove' })).not.toBeInTheDocument();
		unmount();

		render(ImageDropzone, { props: { label: 'Logo', src: '/uploads/logo.png', onremove } });
		await user.click(screen.getByRole('button', { name: 'Remove' }));
		expect(onremove).toHaveBeenCalledOnce();
	});

	describe('error and hint', () => {
		it('renders the hint when there is no error', () => {
			render(ImageDropzone, { props: { label: 'Logo', hint: 'PNG or JPEG, up to 1 MB.' } });
			expect(screen.getByText('PNG or JPEG, up to 1 MB.')).toBeInTheDocument();
			expect(fileInput()).not.toHaveAttribute('aria-invalid');
		});

		it('replaces the hint with the error and marks the input invalid', () => {
			render(ImageDropzone, {
				props: { label: 'Logo', hint: 'PNG or JPEG, up to 1 MB.', error: 'That file is too large.' }
			});
			expect(screen.getByText('That file is too large.')).toBeInTheDocument();
			expect(screen.queryByText('PNG or JPEG, up to 1 MB.')).not.toBeInTheDocument();
			expect(fileInput()).toHaveAttribute('aria-invalid', 'true');
		});

		it('makes the error reachable from the input via aria-describedby', () => {
			render(ImageDropzone, { props: { label: 'Logo', error: 'That file is too large.' } });
			const refs = (fileInput().getAttribute('aria-describedby') ?? '').split(' ').filter(Boolean);
			const texts = refs.map((ref) => document.getElementById(ref)?.textContent);
			expect(texts).toContain('That file is too large.');
		});

		it('describes nothing when there is neither an error nor a hint', () => {
			render(ImageDropzone, { props: { label: 'Logo' } });
			expect(fileInput()).not.toHaveAttribute('aria-describedby');
		});

		it('keeps showing the error while a new file is being chosen', async () => {
			const onselect = vi.fn();
			const user = userEvent.setup();
			render(ImageDropzone, {
				props: { label: 'Logo', error: 'That file is too large.', onselect }
			});
			await user.upload(fileInput(), png());
			// The consumer owns the error, so retrying must still reach it.
			expect(onselect).toHaveBeenCalledOnce();
			expect(screen.getByText('That file is too large.')).toBeInTheDocument();
		});
	});

	describe('disabled', () => {
		it('refuses a drop', async () => {
			const onselect = vi.fn();
			const { container } = render(ImageDropzone, {
				props: { label: 'Logo', disabled: true, onselect }
			});
			await drop(zone(container), [png()]);
			expect(onselect).not.toHaveBeenCalled();
		});

		it('does not arm the dragover highlight', async () => {
			const { container } = render(ImageDropzone, { props: { label: 'Logo', disabled: true } });
			await fireEvent.dragOver(zone(container));
			expect(zone(container)).not.toHaveClass('dragover');
		});

		it('natively disables the input and the actions', () => {
			render(ImageDropzone, { props: { label: 'Logo', src: '/u/l.png', disabled: true } });
			expect(fileInput()).toBeDisabled();
			expect(screen.getByRole('button', { name: 'Choose file' })).toBeDisabled();
			expect(screen.getByRole('button', { name: 'Remove' })).toBeDisabled();
		});
	});

	describe('busy', () => {
		it('refuses a drop while an upload is in flight', async () => {
			const onselect = vi.fn();
			const { container } = render(ImageDropzone, {
				props: { label: 'Logo', busy: true, onselect }
			});
			await drop(zone(container), [png()]);
			expect(onselect).not.toHaveBeenCalled();
		});

		it('does not arm the dragover highlight', async () => {
			const { container } = render(ImageDropzone, { props: { label: 'Logo', busy: true } });
			await fireEvent.dragOver(zone(container));
			expect(zone(container)).not.toHaveClass('dragover');
		});

		it('swallows the Remove action', async () => {
			const onremove = vi.fn();
			const user = userEvent.setup();
			render(ImageDropzone, {
				props: { label: 'Logo', src: '/u/l.png', busy: true, onremove }
			});
			await user.click(screen.getByRole('button', { name: 'Remove' }));
			expect(onremove).not.toHaveBeenCalled();
		});

		it('uses aria-disabled rather than the native attribute, so focus survives', () => {
			render(ImageDropzone, { props: { label: 'Logo', src: '/u/l.png', busy: true } });
			for (const name of ['Choose file', 'Remove']) {
				const btn = screen.getByRole('button', { name });
				expect(btn).toHaveAttribute('aria-disabled', 'true');
				expect(btn).not.toBeDisabled();
			}
			expect(fileInput()).toHaveAttribute('aria-disabled', 'true');
			expect(fileInput()).toHaveAttribute('aria-busy', 'true');
			expect(fileInput()).not.toBeDisabled();

			// Still focusable: that is the whole point of not using `disabled`.
			fileInput().focus();
			expect(fileInput()).toHaveFocus();
		});

		it('keeps the picker shut without stealing focus from the input', async () => {
			render(ImageDropzone, { props: { label: 'Logo', busy: true } });
			const opened = vi.fn();
			fileInput().addEventListener('click', opened);
			const clicked = await fireEvent.click(fileInput());
			// preventDefault() reports back as a cancelled event.
			expect(clicked).toBe(false);
			expect(opened).toHaveBeenCalledOnce();
		});
	});
});
