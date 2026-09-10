import { act, cleanup, renderHook, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ReactNode } from 'react';
import type { Settings } from '@/api/settings';
import { useSettings, useSettingsEditor, useUpdateSettings } from './useSettings';

const fetchMock = vi.fn();

const serverSettings: Settings = {
  auto_review_requested_prs: true,
  review_n_requests: 3,
  publish_enabled_authors: 'alice',
  publish_inline_cap: 5,
  publish_inline_min_severity: 'medium',
  publish_show_unverified: false,
  publish_reply_mode: 'off',
  publish_reply_enabled_at: '',
  admin_logins: 'alice',
  admin_logins_fixed: ['owner'],
};

const jsonResponse = (body: unknown) => new Response(JSON.stringify(body), { status: 200 });

const deferred = <T,>() => {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => (resolve = r));
  return { promise, resolve };
};

const makeClient = () =>
  new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });

const wrapperFor = (client: QueryClient) =>
  function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
  };

describe('useUpdateSettings', () => {
  beforeEach(() => {
    fetchMock.mockReset();
    vi.stubGlobal('fetch', fetchMock);
    vi.spyOn(console, 'error').mockImplementation(() => {});
  });
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it('writes the server response into the settings cache only after it resolves', async () => {
    const client = makeClient();
    client.setQueryData<Settings>(['settings'], serverSettings);
    const pending = deferred<Response>();
    fetchMock.mockReturnValue(pending.promise);

    const { result } = renderHook(() => useUpdateSettings(), { wrapper: wrapperFor(client) });
    act(() => result.current.mutate({ review_n_requests: 7 }));
    await waitFor(() => expect(result.current.isPending).toBe(true));
    expect(client.getQueryData<Settings>(['settings'])).toEqual(serverSettings);

    const fromServer = { ...serverSettings, review_n_requests: 7, publish_reply_enabled_at: '2026-09-10T12:00:00Z' };
    pending.resolve(jsonResponse(fromServer));
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(client.getQueryData<Settings>(['settings'])).toEqual(fromServer);
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/settings',
      expect.objectContaining({ method: 'POST', body: '{"review_n_requests":7}' })
    );
  });

  it('leaves the cache untouched and exposes the server text when the write fails', async () => {
    const client = makeClient();
    client.setQueryData<Settings>(['settings'], serverSettings);
    fetchMock.mockResolvedValue(new Response('admin required\n', { status: 403, statusText: 'Forbidden' }));

    const { result } = renderHook(() => useUpdateSettings(), { wrapper: wrapperFor(client) });
    act(() => result.current.mutate({ review_n_requests: 7 }));
    await waitFor(() => expect(result.current.isError).toBe(true));
    expect(result.current.error?.message).toBe('admin required');
    expect(client.getQueryData<Settings>(['settings'])).toEqual(serverSettings);
  });
});

describe('useSettingsEditor', () => {
  beforeEach(() => {
    fetchMock.mockReset();
    vi.stubGlobal('fetch', fetchMock);
  });
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it('refetches on mount even when the cached settings are fresh', async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
    client.setQueryData<Settings>(['settings'], serverSettings);
    const fromServer = { ...serverSettings, review_n_requests: 9 };
    fetchMock.mockResolvedValue(jsonResponse(fromServer));

    const { result } = renderHook(() => useSettingsEditor(), { wrapper: wrapperFor(client) });
    await waitFor(() => expect(result.current.data?.review_n_requests).toBe(9));
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it('useSettings keeps serving fresh cached data without a refetch', async () => {
    const client = makeClient();
    client.setQueryData<Settings>(['settings'], serverSettings);
    fetchMock.mockResolvedValue(jsonResponse({ ...serverSettings, review_n_requests: 9 }));

    const { result } = renderHook(() => useSettings(), { wrapper: wrapperFor(client) });
    expect(result.current.data?.review_n_requests).toBe(3);
    await act(async () => {});
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
