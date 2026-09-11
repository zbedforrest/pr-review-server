import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Settings } from '@/api/settings';
import type { CurrentUser } from '@/api/user';
import { SettingsPage } from './SettingsPage';

const fetchMock = vi.fn();

const serverSettings: Settings = {
  auto_review_requested_prs: true,
  review_n_requests: 3,
  publish_enabled_authors: 'alice',
  publish_inline_cap: 5,
  publish_inline_min_severity: 'medium',
  publish_show_unverified: false,
  publish_reply_mode: 'react',
  publish_reply_enabled_at: '',
  admin_logins: 'alice',
  admin_logins_fixed: ['owner'],
};

const admin: CurrentUser = { id: 1, github_username: 'alice', github_avatar_url: '', is_admin: true };
const member: CurrentUser = { ...admin, id: 2, github_username: 'bob', is_admin: false };

const replies = { mode: 'react', total: 4, by_action: { react: 4 }, unlinked_roots: 1 };

const jsonResponse = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });

type Routes = Record<string, () => Promise<Response> | Response>;

const route = (routes: Routes) =>
  fetchMock.mockImplementation((url: string) => {
    const path = url.split('?')[0];
    const handler = routes[path];
    if (!handler) return Promise.resolve(new Response('not found', { status: 404 }));
    return Promise.resolve(handler());
  });

const requestedPaths = () => fetchMock.mock.calls.map(([url]) => (url as string).split('?')[0]);

const renderPage = () => {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  render(
    <QueryClientProvider client={client}>
      <SettingsPage />
    </QueryClientProvider>
  );
  return client;
};

const enabledControls = () =>
  [
    ...screen.queryAllByRole('button'),
    ...screen.queryAllByRole('checkbox'),
    ...screen.queryAllByRole('radio'),
    ...screen.queryAllByRole('spinbutton'),
    ...screen.queryAllByRole('combobox'),
    ...screen.queryAllByRole('textbox'),
  ].filter((el) => !(el as HTMLInputElement).disabled);

describe('SettingsPage', () => {
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

  it('renders the shell with a link back to the dashboard', () => {
    route({});
    renderPage();
    expect(screen.getByRole('heading', { name: 'Settings' })).toBeTruthy();
    expect(screen.getByRole('link', { name: 'Back to Dashboard' }).getAttribute('href')).toBe('/');
  });

  it('shows the spinner and no enabled control while the user is still loading', async () => {
    route({
      '/api/settings': () => jsonResponse(serverSettings),
      '/api/user': () => new Promise<Response>(() => {}),
      '/api/prs': () => jsonResponse([]),
    });
    renderPage();

    await waitFor(() => expect(requestedPaths()).toContain('/api/settings'));
    await waitFor(() => expect(fetchMock.mock.calls.length).toBeGreaterThanOrEqual(2));
    expect(screen.getByText('Loading...')).toBeTruthy();
    expect(enabledControls()).toEqual([]);
    expect(screen.queryByRole('button', { name: 'Save' })).toBeNull();
  });

  it('renders the form once the user and settings resolve', async () => {
    route({
      '/api/settings': () => jsonResponse(serverSettings),
      '/api/user': () => jsonResponse(admin),
      '/api/prs': () => jsonResponse([]),
      '/api/publish/replies': () => jsonResponse(replies),
    });
    renderPage();

    await waitFor(() => expect(screen.getAllByRole('button', { name: 'Save' }).length).toBe(4));
    expect(screen.queryByText('Loading...')).toBeNull();
    expect((screen.getByLabelText('First-pass samples') as HTMLInputElement).disabled).toBe(false);
    await waitFor(() => expect(screen.getByText(/4 replies/).textContent).toContain('react: 4'));
    expect(screen.getByText(/4 replies/).textContent).toContain('1 unlinked roots');
  });

  it('does not request the reply ledger for non-admins', async () => {
    route({
      '/api/settings': () => jsonResponse(serverSettings),
      '/api/user': () => jsonResponse(member),
      '/api/prs': () => jsonResponse([]),
      '/api/publish/replies': () => new Response('admin required', { status: 403 }),
    });
    renderPage();

    await waitFor(() => expect(screen.getAllByRole('button', { name: 'Save' }).length).toBe(4));
    expect(screen.getByText(/Read only\. Admins: owner, alice/)).toBeTruthy();
    expect(requestedPaths()).not.toContain('/api/publish/replies');
    expect(screen.queryByText(/replies,/)).toBeNull();
  });

  it('does not flag logins as unseen until the PR list has loaded', async () => {
    let resolvePRs: (response: Response) => void = () => {};
    route({
      '/api/settings': () => jsonResponse(serverSettings),
      '/api/user': () => jsonResponse(admin),
      '/api/prs': () => new Promise<Response>((resolve) => (resolvePRs = resolve)),
      '/api/publish/replies': () => jsonResponse(replies),
    });
    renderPage();

    await waitFor(() => expect(screen.getAllByRole('button', { name: 'Save' }).length).toBe(4));
    expect(screen.queryAllByText('not seen on any PR')).toEqual([]);

    resolvePRs(jsonResponse([{ author: 'alice' }]));
    await waitFor(() => expect(screen.getAllByText('not seen on any PR').length).toBe(1));
  });

  it('renders the error message when settings fail to load', async () => {
    route({
      '/api/settings': () => new Response('settings unavailable', { status: 500 }),
      '/api/user': () => jsonResponse(admin),
      '/api/prs': () => jsonResponse([]),
    });
    renderPage();

    await waitFor(() => expect(screen.getByText('settings unavailable')).toBeTruthy());
    expect(screen.queryByText('Loading...')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Save' })).toBeNull();
  });

  it('keeps the form and its unsaved draft when a settings refetch fails', async () => {
    let settingsFailing = false;
    route({
      '/api/settings': () =>
        settingsFailing ? new Response('upstream connect error', { status: 503 }) : jsonResponse(serverSettings),
      '/api/user': () => jsonResponse(admin),
      '/api/prs': () => jsonResponse([]),
      '/api/publish/replies': () => jsonResponse(replies),
    });
    const client = renderPage();

    await waitFor(() => expect(screen.getAllByRole('button', { name: 'Save' }).length).toBe(4));
    fireEvent.change(screen.getByLabelText('First-pass samples'), { target: { value: '7' } });

    settingsFailing = true;
    await client.refetchQueries({ queryKey: ['settings'] });

    await waitFor(() => expect(client.getQueryState(['settings'])?.error).toBeTruthy());
    expect((screen.getByLabelText('First-pass samples') as HTMLInputElement).value).toBe('7');
    expect(screen.getAllByRole('button', { name: 'Save' }).length).toBe(4);
    expect(screen.queryByText('upstream connect error')).toBeNull();
  });
});
