import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { CurrentUser } from '@/api/user';
import { PublishOptOutBanner } from './PublishOptOutBanner';

const fetchMock = vi.fn();

const user = (partial: Partial<CurrentUser>): CurrentUser => ({
  id: 1,
  github_username: 'alice',
  github_avatar_url: '',
  is_admin: false,
  ...partial,
});

const jsonResponse = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });

function renderBanner(current: CurrentUser | undefined) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  if (current) client.setQueryData<CurrentUser>(['currentUser'], current);
  render(
    <QueryClientProvider client={client}>
      <PublishOptOutBanner />
    </QueryClientProvider>
  );
  return client;
}

const requests = () => fetchMock.mock.calls.map(([url, init]) => `${(init as RequestInit | undefined)?.method ?? 'GET'} ${url}`);

describe('PublishOptOutBanner', () => {
  beforeEach(() => {
    fetchMock.mockReset();
    vi.stubGlobal('fetch', fetchMock);
  });
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it('renders nothing for a user outside the pilot', () => {
    renderBanner(user({ publish_enrolled: false, publish_opted_out: false }));
    expect(screen.queryByRole('status')).toBeNull();
  });

  it('renders nothing while the user is unknown', () => {
    fetchMock.mockReturnValue(new Promise(() => {}));
    renderBanner(undefined);
    expect(screen.queryByRole('status')).toBeNull();
  });

  it('offers Leave to an enrolled user and names how they are enrolled', async () => {
    fetchMock.mockResolvedValue(jsonResponse(user({ publish_enrolled: true, publish_opted_out: true, enrolled_via: 'xo-team' })));
    renderBanner(user({ publish_enrolled: true, publish_opted_out: false, enrolled_via: 'xo-team' }));

    expect(screen.getByRole('status').textContent).toContain('via team xo-team');
    expect(screen.getByRole('status').textContent).toContain('intentional');
    fireEvent.click(screen.getByRole('button', { name: 'Leave' }));

    await waitFor(() => expect(requests()).toEqual(['POST /api/me/publish-opt-out']));
    await screen.findByRole('button', { name: 'Rejoin' });
    expect(screen.getByRole('status').textContent).toContain('PRism comments are off for your PRs');
  });

  it('offers Rejoin to an opted-out user', async () => {
    fetchMock.mockResolvedValue(jsonResponse(user({ publish_enrolled: true, publish_opted_out: false, enrolled_via: 'login' })));
    renderBanner(user({ publish_enrolled: true, publish_opted_out: true, enrolled_via: 'login' }));

    fireEvent.click(screen.getByRole('button', { name: 'Rejoin' }));

    await waitFor(() => expect(requests()).toEqual(['DELETE /api/me/publish-opt-out']));
    await screen.findByRole('button', { name: 'Leave' });
    expect(screen.getByRole('status').textContent).toContain('by login');
  });

  it('shows the server error and keeps the button when leaving fails', async () => {
    fetchMock.mockResolvedValue(new Response('Failed to update settings: db down', { status: 500 }));
    renderBanner(user({ publish_enrolled: true, publish_opted_out: false, enrolled_via: '*' }));

    fireEvent.click(screen.getByRole('button', { name: 'Leave' }));

    await waitFor(() => expect(screen.getByRole('status').textContent).toContain('db down'));
    expect(screen.getByRole('button', { name: 'Leave' })).toBeTruthy();
  });

  it('carries the anchor the comment footer links to', () => {
    renderBanner(user({ publish_enrolled: true, publish_opted_out: false, enrolled_via: 'login' }));
    expect(screen.getByRole('status').id).toBe('prism-comments');
  });
});
