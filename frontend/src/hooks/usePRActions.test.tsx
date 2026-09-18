import { act, cleanup, renderHook, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ReactNode } from 'react';
import type { PR } from '@/types/pr';
import { useSubmitQuickAction } from './usePRActions';

const fetchMock = vi.fn();

const makePR = (partial: Partial<PR> = {}): PR => ({
  owner: 'acme',
  repo: 'example',
  number: 1,
  commit_sha: 'abc123',
  last_reviewed_at: null,
  review_html_path: '',
  github_url: '',
  review_url: '',
  status: 'completed',
  title: 'Example',
  author: 'bob',
  generating_since: null,
  approval_count: 2,
  my_review_status: '',
  draft: false,
  ci_state: 'success',
  ci_failed_checks: [],
  created_at: null,
  is_mine: false,
  via_teams: [],
  critical_count: 0,
  medium_count: 0,
  low_count: 0,
  notes: '',
  ...partial,
});

const params = (action: 'approve' | 'request_changes' | 'comment') => ({
  owner: 'acme',
  repo: 'example',
  number: 1,
  action,
  body: action === 'approve' ? '' : 'please fix',
  expected_head_sha: 'abc123',
  request_id: 'req-00000001',
});

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

const rowOf = (client: QueryClient) => client.getQueryData<PR[]>(['prs'])![0];

describe('useSubmitQuickAction', () => {
  beforeEach(() => {
    fetchMock.mockReset();
    vi.stubGlobal('fetch', fetchMock);
  });
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it('optimistically sets my_review_status and bumps approval_count on approve', async () => {
    const client = makeClient();
    client.setQueryData<PR[]>(['prs'], [makePR(), makePR({ number: 2 })]);
    const pending = deferred<Response>();
    fetchMock.mockReturnValue(pending.promise);

    const { result } = renderHook(() => useSubmitQuickAction(), { wrapper: wrapperFor(client) });
    act(() => result.current.mutate(params('approve')));
    await waitFor(() => expect(rowOf(client).my_review_status).toBe('APPROVED'));
    expect(rowOf(client).approval_count).toBe(3);
    expect(client.getQueryData<PR[]>(['prs'])![1].my_review_status).toBe('');
    expect(fetchMock).toHaveBeenCalledWith('/api/prs/quick-action', expect.objectContaining({ method: 'POST' }));
    expect(JSON.parse(fetchMock.mock.calls[0][1].body)).toEqual({
      owner: 'acme', repo: 'example', number: 1, action: 'approve', expected_head_sha: 'abc123', request_id: 'req-00000001',
    });

    pending.resolve(new Response(JSON.stringify({ status: 'success', state: 'APPROVED' }), { status: 200 }));
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
  });

  it('does not bump approval_count when already approved', async () => {
    const client = makeClient();
    client.setQueryData<PR[]>(['prs'], [makePR({ my_review_status: 'APPROVED' })]);
    fetchMock.mockReturnValue(deferred<Response>().promise);

    const { result } = renderHook(() => useSubmitQuickAction(), { wrapper: wrapperFor(client) });
    act(() => result.current.mutate(params('approve')));
    await waitFor(() => expect(result.current.isPending).toBe(true));
    expect(rowOf(client).approval_count).toBe(2);
  });

  it('sets CHANGES_REQUESTED without touching approval_count', async () => {
    const client = makeClient();
    client.setQueryData<PR[]>(['prs'], [makePR()]);
    fetchMock.mockReturnValue(deferred<Response>().promise);

    const { result } = renderHook(() => useSubmitQuickAction(), { wrapper: wrapperFor(client) });
    act(() => result.current.mutate(params('request_changes')));
    await waitFor(() => expect(rowOf(client).my_review_status).toBe('CHANGES_REQUESTED'));
    expect(rowOf(client).approval_count).toBe(2);
    expect(JSON.parse(fetchMock.mock.calls[0][1].body).body).toBe('please fix');
  });

  it('decrements approval_count when an approved user requests changes or comments', async () => {
    for (const action of ['request_changes', 'comment'] as const) {
      const client = makeClient();
      client.setQueryData<PR[]>(['prs'], [makePR({ my_review_status: 'APPROVED' })]);
      fetchMock.mockReturnValue(deferred<Response>().promise);
      const { result } = renderHook(() => useSubmitQuickAction(), { wrapper: wrapperFor(client) });
      act(() => result.current.mutate(params(action)));
      await waitFor(() => expect(rowOf(client).my_review_status).not.toBe('APPROVED'));
      expect(rowOf(client).approval_count).toBe(1);
      cleanup();
    }
  });

  it('rolls back only the target row fields, keeping updates that landed meanwhile', async () => {
    const client = makeClient();
    client.setQueryData<PR[]>(['prs'], [makePR({ title: 'old title' }), makePR({ number: 2 })]);
    const pending = deferred<Response>();
    fetchMock.mockReturnValue(pending.promise);

    const { result } = renderHook(() => useSubmitQuickAction(), { wrapper: wrapperFor(client) });
    act(() => result.current.mutate(params('approve')));
    await waitFor(() => expect(rowOf(client).my_review_status).toBe('APPROVED'));

    act(() => {
      client.setQueryData<PR[]>(['prs'], (old) =>
        old!.map((pr) => (pr.number === 1 ? { ...pr, title: 'new title' } : { ...pr, approval_count: 9 }))
      );
    });
    pending.resolve(new Response('{"error":"no","code":"no_permission"}', { status: 403, statusText: 'Forbidden' }));
    await waitFor(() => expect(result.current.isError).toBe(true));

    expect(rowOf(client)).toMatchObject({ title: 'new title', my_review_status: '', approval_count: 2 });
    expect(client.getQueryData<PR[]>(['prs'])![1].approval_count).toBe(9);
  });

  it('rolls back on error and exposes the parsed code', async () => {
    const client = makeClient();
    const original = [makePR()];
    client.setQueryData<PR[]>(['prs'], original);
    fetchMock.mockResolvedValue(
      new Response('{"error":"own pr","code":"own_pr"}', { status: 422, statusText: 'Unprocessable Entity' })
    );

    const { result } = renderHook(() => useSubmitQuickAction(), { wrapper: wrapperFor(client) });
    act(() => result.current.mutate(params('approve')));
    await waitFor(() => expect(result.current.isError).toBe(true));
    expect(rowOf(client)).toEqual(original[0]);
    expect((result.current.error as { code?: string }).code).toBe('own_pr');
  });
});
