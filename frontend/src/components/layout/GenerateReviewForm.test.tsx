import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { GenerateReviewForm } from './GenerateReviewForm';
import { APIError } from '@/api/client';

const generateReviewMock = vi.fn();
vi.mock('@/api/prs', () => ({
  generateReview: (params: unknown) => generateReviewMock(params),
}));

const trackMock = vi.fn();
vi.mock('@/hooks/useTelemetry', () => ({
  useTelemetry: () => ({ track: trackMock }),
}));

const submit = (url: string) => {
  fireEvent.change(screen.getByLabelText('PR URL to review'), { target: { value: url } });
  fireEvent.click(screen.getByRole('button', { name: 'Review' }));
};

describe('GenerateReviewForm', () => {
  beforeEach(() => {
    generateReviewMock.mockReset();
    trackMock.mockReset();
  });
  afterEach(() => cleanup());

  it('submits a parsed PR URL to generate-review and reports success', async () => {
    generateReviewMock.mockResolvedValue({ status: 'success' });
    render(<GenerateReviewForm />);
    submit('https://github.com/acme/example/pull/123');

    await waitFor(() =>
      expect(screen.getByRole('status').textContent).toContain('Review started for acme/example#123')
    );
    expect(generateReviewMock).toHaveBeenCalledWith({ owner: 'acme', repo: 'example', number: 123, publish: true });
    expect(trackMock).toHaveBeenCalledWith('generate_review_by_url', {
      pr_owner: 'acme',
      pr_repo: 'example',
      pr_number: 123,
      publish: true,
    });
    // Input clears on success so the next paste starts fresh.
    expect((screen.getByLabelText('PR URL to review') as HTMLInputElement).value).toBe('');
  });

  it('offers a "post to PR" checkbox that is checked by default', () => {
    render(<GenerateReviewForm />);
    const box = screen.getByRole('checkbox', { name: 'Post to PR as the Prism bot' }) as HTMLInputElement;
    expect(box.checked).toBe(true);
  });

  it('sends publish=false when the checkbox is unchecked', async () => {
    generateReviewMock.mockResolvedValue({ status: 'success' });
    render(<GenerateReviewForm />);
    fireEvent.click(screen.getByRole('checkbox', { name: 'Post to PR as the Prism bot' }));
    submit('https://github.com/acme/example/pull/123');

    await waitFor(() => expect(generateReviewMock).toHaveBeenCalledTimes(1));
    expect(generateReviewMock).toHaveBeenCalledWith({ owner: 'acme', repo: 'example', number: 123, publish: false });
    expect(trackMock).toHaveBeenCalledWith('generate_review_by_url', expect.objectContaining({ publish: false }));
  });

  it('rejects unparseable input without calling the API', async () => {
    render(<GenerateReviewForm />);
    submit('not a pr url');

    await waitFor(() => expect(screen.getByRole('status').textContent).toContain('Not a PR URL'));
    expect(generateReviewMock).not.toHaveBeenCalled();
  });

  it('treats 409 (already in flight) as a benign outcome', async () => {
    generateReviewMock.mockRejectedValue(new APIError('conflict', 409, 'Conflict'));
    render(<GenerateReviewForm />);
    submit('acme/example#7');

    await waitFor(() =>
      expect(screen.getByRole('status').textContent).toContain('already in progress for acme/example#7')
    );
  });

  it('reports 404 as PR not found', async () => {
    generateReviewMock.mockRejectedValue(new APIError('not found', 404, 'Not Found'));
    render(<GenerateReviewForm />);
    submit('acme/example#999');

    await waitFor(() =>
      expect(screen.getByRole('status').textContent).toContain('PR not found: acme/example#999')
    );
  });
});
