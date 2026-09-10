import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { APIError, apiDelete, apiGet, apiPost } from './client';

const fetchMock = vi.fn();

const jsonResponse = (body: unknown, init?: ResponseInit) =>
  new Response(JSON.stringify(body), { status: 200, ...init });

describe('api client', () => {
  beforeEach(() => {
    fetchMock.mockReset();
    vi.stubGlobal('fetch', fetchMock);
  });
  afterEach(() => vi.unstubAllGlobals());

  it('apiGet returns the parsed body on success', async () => {
    fetchMock.mockResolvedValue(jsonResponse({ ok: true }));
    await expect(apiGet('/api/settings')).resolves.toEqual({ ok: true });
    expect(fetchMock).toHaveBeenCalledWith('/api/settings', expect.objectContaining({ headers: expect.any(Object) }));
  });

  it('apiPost sends the JSON body and returns the parsed response', async () => {
    fetchMock.mockResolvedValue(jsonResponse({ saved: 1 }));
    await expect(apiPost('/api/settings', { a: 1 })).resolves.toEqual({ saved: 1 });
    expect(fetchMock).toHaveBeenCalledWith('/api/settings', expect.objectContaining({ method: 'POST', body: '{"a":1}' }));
  });

  it('puts the trimmed response body text and the status on the thrown error', async () => {
    fetchMock.mockResolvedValue(
      new Response('publish_enabled_authors: "al ice" is not a valid login\n', { status: 400, statusText: 'Bad Request' })
    );
    const err = await apiPost('/api/settings', {}).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(APIError);
    expect((err as APIError).message).toBe('publish_enabled_authors: "al ice" is not a valid login');
    expect((err as APIError).status).toBe(400);
    expect((err as APIError).statusText).toBe('Bad Request');
  });

  it('surfaces a 403 body from every verb', async () => {
    for (const call of [
      () => apiGet('/api/settings'),
      () => apiPost('/api/settings', {}),
      () => apiDelete('/api/prs', {}),
    ]) {
      fetchMock.mockResolvedValue(new Response('admin required\n', { status: 403, statusText: 'Forbidden' }));
      const err = await call().catch((e: unknown) => e);
      expect((err as APIError).message).toBe('admin required');
      expect((err as APIError).status).toBe(403);
    }
  });

  it('falls back to the status text when the error body is empty', async () => {
    fetchMock.mockResolvedValue(new Response('', { status: 500, statusText: 'Internal Server Error' }));
    const err = await apiGet('/api/settings').catch((e: unknown) => e);
    expect((err as APIError).message).toBe('API error: Internal Server Error');
    expect((err as APIError).status).toBe(500);
  });
});
