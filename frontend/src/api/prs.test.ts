import { afterEach, describe, expect, it, vi } from 'vitest';

const apiPostMock = vi.fn();
vi.mock('./client', () => ({
  apiPost: (path: string, body: unknown) => apiPostMock(path, body),
}));

import { generateReview, triggerReview } from './prs';

describe('triggerReview with a profile', () => {
  afterEach(() => apiPostMock.mockClear());

  it('requests config.profile through the versioned run API', async () => {
    apiPostMock.mockResolvedValue({ run_id: 'run-1', status: 'queued' });
    const result = await triggerReview({ owner: 'acme', repo: 'example', number: 7, publish: false, profile: 'lite' });
    expect(apiPostMock).toHaveBeenCalledWith('/api/v1/review-runs', {
      target: { owner: 'acme', repo: 'example', pull_request: 7 },
      publish: false,
      config: { profile: 'lite' },
    });
    expect(result).toEqual({ status: 'queued' });
  });

  it('publishes by default when a profile is requested without a publish choice', async () => {
    apiPostMock.mockResolvedValue({ run_id: 'run-1', status: 'queued' });
    await triggerReview({ owner: 'acme', repo: 'example', number: 7, profile: 'lite_plus' });
    const last = apiPostMock.mock.calls[apiPostMock.mock.calls.length - 1];
    expect(last[1]).toMatchObject({ publish: true, config: { profile: 'lite_plus' } });
  });
});

describe('generateReview', () => {
  it('marks dashboard submissions with source=form so the server claims the PR', async () => {
    apiPostMock.mockResolvedValue({ status: 'success' });
    await generateReview({ owner: 'acme', repo: 'example', number: 7 });
    expect(apiPostMock).toHaveBeenCalledWith('/api/prs/generate-review', {
      owner: 'acme',
      repo: 'example',
      number: 7,
      source: 'form',
    });
  });

  it('triggerReview stays unmarked', async () => {
    apiPostMock.mockResolvedValue({ status: 'success' });
    await triggerReview({ owner: 'acme', repo: 'example', number: 7 });
    expect(apiPostMock).toHaveBeenCalledWith('/api/prs/trigger-review', {
      owner: 'acme',
      repo: 'example',
      number: 7,
    });
  });

  it('omits the publish key entirely when it is undefined', async () => {
    apiPostMock.mockResolvedValue({ status: 'success' });
    await triggerReview({ owner: 'acme', repo: 'example', number: 7, publish: undefined });
    await generateReview({ owner: 'acme', repo: 'example', number: 7, publish: undefined });
    for (const [, body] of apiPostMock.mock.calls) {
      expect(Object.keys(body as object)).not.toContain('publish');
    }
  });

  it('forwards publish=false to both endpoints', async () => {
    apiPostMock.mockResolvedValue({ status: 'success' });
    await triggerReview({ owner: 'acme', repo: 'example', number: 7, publish: false });
    expect(apiPostMock).toHaveBeenCalledWith('/api/prs/trigger-review', {
      owner: 'acme',
      repo: 'example',
      number: 7,
      publish: false,
    });
    await generateReview({ owner: 'acme', repo: 'example', number: 7, publish: false });
    expect(apiPostMock).toHaveBeenCalledWith('/api/prs/generate-review', {
      owner: 'acme',
      repo: 'example',
      number: 7,
      source: 'form',
      publish: false,
    });
  });

  it('forwards publish=true explicitly when requested', async () => {
    apiPostMock.mockResolvedValue({ status: 'success' });
    await triggerReview({ owner: 'acme', repo: 'example', number: 7, publish: true });
    expect(apiPostMock).toHaveBeenCalledWith('/api/prs/trigger-review', {
      owner: 'acme',
      repo: 'example',
      number: 7,
      publish: true,
    });
  });
});
