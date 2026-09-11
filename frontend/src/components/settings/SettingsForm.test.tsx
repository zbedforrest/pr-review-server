import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Settings } from '@/api/settings';
import { useSettings } from '@/hooks/useSettings';
import { SettingsForm } from './SettingsForm';

const fetchMock = vi.fn();

const serverSettings: Settings = {
  auto_review_requested_prs: true,
  review_n_requests: 3,
  publish_enabled_authors: 'alice,bob',
  publish_inline_cap: 5,
  publish_inline_min_severity: 'medium',
  publish_show_unverified: false,
  publish_reply_mode: 'react',
  publish_reply_enabled_at: '2026-09-10T12:00:00Z',
  admin_logins: 'alice,carol',
  admin_logins_fixed: ['owner'],
  generate_html: true,
};

const jsonResponse = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });

interface HarnessProps {
  isAdmin?: boolean;
  currentLogin?: string;
  replyTotals?: React.ComponentProps<typeof SettingsForm>['replyTotals'];
}

function Harness({ isAdmin = true, currentLogin = 'alice', replyTotals }: HarnessProps) {
  const { data } = useSettings();
  if (!data) return null;
  return (
    <SettingsForm
      settings={data}
      isAdmin={isAdmin}
      currentLogin={currentLogin}
      knownLogins={new Set(['alice', 'bob'])}
      replyTotals={replyTotals}
    />
  );
}

const renderForm = (props: HarnessProps = {}, settings: Settings = serverSettings) => {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  client.setQueryData<Settings>(['settings'], settings);
  const view = render(
    <QueryClientProvider client={client}>
      <Harness {...props} />
    </QueryClientProvider>
  );
  return { client, ...view };
};

const section = (title: string) =>
  screen.getByRole('heading', { name: title }).closest('.settings-section') as HTMLElement;
const saveIn = (title: string) => within(section(title)).getByRole('button', { name: 'Save' }) as HTMLButtonElement;
const resetIn = (title: string) => within(section(title)).getByRole('button', { name: 'Reset' }) as HTMLButtonElement;
const statusIn = (title: string) => within(section(title)).getByRole('status');
const samples = () => screen.getByLabelText('First-pass samples') as HTMLInputElement;
const mode = (name: string) => screen.getByRole('radio', { name }) as HTMLInputElement;
const allControls = () =>
  [...screen.getAllByRole('checkbox'), ...screen.getAllByRole('radio'), ...screen.getAllByRole('spinbutton'), ...screen.getAllByRole('combobox'), ...screen.getAllByRole('textbox')] as (HTMLInputElement | HTMLSelectElement)[];

const postedBodies = () =>
  fetchMock.mock.calls
    .filter(([, init]) => (init as RequestInit | undefined)?.method === 'POST')
    .map(([, init]) => JSON.parse((init as RequestInit).body as string));

describe('SettingsForm', () => {
  beforeEach(() => {
    fetchMock.mockReset();
    vi.stubGlobal('fetch', fetchMock);
    vi.spyOn(window, 'confirm').mockReturnValue(true);
    vi.spyOn(console, 'error').mockImplementation(() => {});
  });
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it('renders the current values in every section', () => {
    renderForm();
    expect((screen.getByLabelText(/Automatically review PRs/) as HTMLInputElement).checked).toBe(true);
    expect(samples().value).toBe('3');
    expect(within(section('Publishing')).getByText('alice')).toBeTruthy();
    expect(within(section('Publishing')).getByText('bob')).toBeTruthy();
    expect((screen.getByLabelText('Inline comment cap') as HTMLInputElement).value).toBe('5');
    expect((screen.getByLabelText('Minimum inline severity') as HTMLSelectElement).value).toBe('medium');
    expect((screen.getByLabelText('Show unverified findings') as HTMLInputElement).checked).toBe(false);
    expect(mode('react').checked).toBe(true);
    expect(within(section('Admins')).getByText('alice')).toBeTruthy();
    expect(within(section('Admins')).getByText('carol')).toBeTruthy();
    const owner = screen.getByText('owner').closest('.settings-field__chip') as HTMLElement;
    expect(owner.textContent).toContain('from deploy');
    expect(screen.getByText('Turning replies off and on again resets this cutoff; replies written in the gap are skipped')).toBeTruthy();
  });

  it('offers the five reply modes in ladder order with their descriptions', () => {
    renderForm();
    const radios = screen.getAllByRole('radio') as HTMLInputElement[];
    expect(radios.map((r) => r.value)).toEqual(['off', 'observe', 'react', 'shadow', 'respond']);
    expect(screen.getByText(/never posts text/)).toBeTruthy();
    expect(screen.getByText(/posts replies/i)).toBeTruthy();
  });

  it('starts with Save and Reset disabled in every section', () => {
    renderForm();
    for (const title of ['Review', 'Publishing', 'Replies', 'Admins']) {
      expect(saveIn(title).disabled).toBe(true);
      expect(resetIn(title).disabled).toBe(true);
    }
  });

  it('renders every control disabled and the admin notice for a non-admin', () => {
    renderForm({ isAdmin: false, currentLogin: 'dave' });
    expect(screen.getByText('Read only. Admins: owner, alice, carol')).toBeTruthy();
    const controls = allControls();
    expect(controls.length).toBeGreaterThan(8);
    for (const control of controls) expect(control.disabled).toBe(true);
    expect(screen.getByRole('heading', { name: 'Admins' })).toBeTruthy();
    for (const title of ['Review', 'Publishing', 'Replies', 'Admins']) {
      expect(saveIn(title).disabled).toBe(true);
      expect(resetIn(title).disabled).toBe(true);
    }
  });

  it('tells a non-admin how to recover when no admin is configured', () => {
    renderForm({ isAdmin: false, currentLogin: 'dave' }, { ...serverSettings, admin_logins: '', admin_logins_fixed: [] });
    expect(
      screen.getByText('Read only. No admins are configured; the deployer must set ADMIN_LOGINS on the server.')
    ).toBeTruthy();
    expect(screen.queryByText(/Admins: /)).toBeNull();
  });

  it('posts only the changed keys of the saved section', async () => {
    fetchMock.mockResolvedValue(jsonResponse({ ...serverSettings, review_n_requests: 7 }));
    renderForm();
    fireEvent.change(samples(), { target: { value: '7' } });
    fireEvent.click(screen.getByLabelText('Show unverified findings'));
    expect(saveIn('Review').disabled).toBe(false);
    expect(saveIn('Publishing').disabled).toBe(false);

    fireEvent.click(saveIn('Review'));
    await waitFor(() => expect(postedBodies()).toEqual([{ review_n_requests: 7 }]));
    await waitFor(() => expect(saveIn('Review').disabled).toBe(true));
    expect(samples().value).toBe('7');
    expect((screen.getByLabelText('Show unverified findings') as HTMLInputElement).checked).toBe(true);
    expect(saveIn('Publishing').disabled).toBe(false);
  });

  it('announces Saving then Saved in the saved section and clears it on the next edit', async () => {
    let resolveSave!: (response: Response) => void;
    fetchMock.mockReturnValue(new Promise<Response>((resolve) => (resolveSave = resolve)));
    renderForm();
    expect(statusIn('Review').textContent).toBe('');
    fireEvent.change(samples(), { target: { value: '7' } });
    fireEvent.click(saveIn('Review'));
    await waitFor(() => expect(statusIn('Review').textContent).toBe('Saving'));
    expect(section('Review').getAttribute('aria-busy')).toBe('true');
    expect(statusIn('Publishing').textContent).toBe('');

    resolveSave(jsonResponse({ ...serverSettings, review_n_requests: 7 }));
    await waitFor(() => expect(statusIn('Review').textContent).toBe('Saved'));
    expect(section('Review').getAttribute('aria-busy')).toBe('false');
    expect(statusIn('Publishing').textContent).toBe('');

    fireEvent.change(samples(), { target: { value: '8' } });
    expect(statusIn('Review').textContent).toBe('');
  });

  it('does not announce Saved when the save fails', async () => {
    fetchMock.mockResolvedValue(new Response('review_n_requests must be at least 1\n', { status: 400 }));
    renderForm();
    fireEvent.change(samples(), { target: { value: '7' } });
    fireEvent.click(saveIn('Review'));
    await waitFor(() => expect(within(section('Review')).getByRole('alert')).toBeTruthy());
    expect(statusIn('Review').textContent).toBe('');
  });

  it('keeps an edit made in the same section while its save is in flight', async () => {
    let resolveSave!: (response: Response) => void;
    fetchMock.mockReturnValue(new Promise<Response>((resolve) => (resolveSave = resolve)));
    renderForm();
    fireEvent.change(samples(), { target: { value: '7' } });
    fireEvent.click(saveIn('Review'));
    await waitFor(() => expect(postedBodies()).toEqual([{ review_n_requests: 7 }]));

    const autoReview = () => screen.getByLabelText(/Automatically review PRs/) as HTMLInputElement;
    fireEvent.click(autoReview());
    expect(autoReview().checked).toBe(false);

    resolveSave(jsonResponse({ ...serverSettings, review_n_requests: 7 }));
    await waitFor(() => expect(samples().value).toBe('7'));
    await waitFor(() => expect(saveIn('Review').disabled).toBe(false));
    expect(autoReview().checked).toBe(false);
  });

  it('follows the server response after a save even when the server normalized the value', async () => {
    fetchMock.mockResolvedValue(jsonResponse({ ...serverSettings, admin_logins: 'alice,carol,dave' }));
    renderForm();
    const input = screen.getByLabelText('Admins') as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'Dave' } });
    fireEvent.keyDown(input, { key: 'Enter' });
    fireEvent.click(saveIn('Admins'));
    await waitFor(() => expect(postedBodies()).toEqual([{ admin_logins: 'alice,carol,dave' }]));
    await waitFor(() => expect(screen.getByText('dave')).toBeTruthy());
    expect(saveIn('Admins').disabled).toBe(true);
  });

  it('resets a section to the server values', () => {
    renderForm();
    fireEvent.change(samples(), { target: { value: '9' } });
    expect(samples().value).toBe('9');
    fireEvent.click(resetIn('Review'));
    expect(samples().value).toBe('3');
    expect(saveIn('Review').disabled).toBe(true);
  });

  it('will not save a sample count below one and says why', () => {
    renderForm();
    expect(samples().getAttribute('aria-invalid')).toBe('false');
    expect(within(section('Review')).queryByRole('alert')).toBeNull();
    fireEvent.change(samples(), { target: { value: '0' } });
    expect(saveIn('Review').disabled).toBe(true);
    expect(samples().getAttribute('aria-invalid')).toBe('true');
    const error = within(section('Review')).getByRole('alert');
    expect(error.textContent).toBe('Enter a whole number of 1 or more');
    expect(samples().getAttribute('aria-describedby')).toContain(error.id);
    fireEvent.change(samples(), { target: { value: '' } });
    expect(saveIn('Review').disabled).toBe(true);
    expect(within(section('Review')).getByRole('alert')).toBeTruthy();
    fireEvent.change(samples(), { target: { value: '2' } });
    expect(saveIn('Review').disabled).toBe(false);
    expect(samples().getAttribute('aria-invalid')).toBe('false');
    expect(within(section('Review')).queryByRole('alert')).toBeNull();
  });

  it('will not save a negative inline cap and says why', () => {
    renderForm();
    const cap = () => screen.getByLabelText('Inline comment cap') as HTMLInputElement;
    fireEvent.change(cap(), { target: { value: '-1' } });
    expect(saveIn('Publishing').disabled).toBe(true);
    expect(cap().getAttribute('aria-invalid')).toBe('true');
    const error = within(section('Publishing')).getByRole('alert');
    expect(error.textContent).toBe('Enter a whole number of 0 or more');
    expect(cap().getAttribute('aria-describedby')).toContain(error.id);
    fireEvent.change(cap(), { target: { value: '0' } });
    expect(saveIn('Publishing').disabled).toBe(false);
    expect(within(section('Publishing')).queryByRole('alert')).toBeNull();
  });

  it('asks before choosing respond and keeps the radio and the server untouched when cancelled', () => {
    vi.mocked(window.confirm).mockReturnValue(false);
    renderForm();
    fireEvent.click(mode('respond'));
    expect(window.confirm).toHaveBeenCalledTimes(1);
    expect(mode('respond').checked).toBe(false);
    expect(mode('react').checked).toBe(true);
    expect(saveIn('Replies').disabled).toBe(true);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('selects respond when the confirm is accepted', () => {
    renderForm();
    fireEvent.click(mode('respond'));
    expect(mode('respond').checked).toBe(true);
    expect(saveIn('Replies').disabled).toBe(false);
  });

  it('shows Active since from server data even while the draft has a different mode', () => {
    renderForm();
    const stamp = new Date('2026-09-10T12:00:00Z').toLocaleString();
    expect(screen.getByText(`Active since ${stamp}`)).toBeTruthy();
    fireEvent.click(mode('off'));
    expect(mode('off').checked).toBe(true);
    expect(screen.getByText(`Active since ${stamp}`)).toBeTruthy();
    expect(screen.queryByText('Not active')).toBeNull();
  });

  it('shows Not active when replies have never been switched on', () => {
    renderForm({}, { ...serverSettings, publish_reply_mode: 'off', publish_reply_enabled_at: '' });
    expect(screen.getByText('Not active')).toBeTruthy();
  });

  it('renders the server error under the section and keeps the draft', async () => {
    fetchMock.mockResolvedValue(new Response('review_n_requests must be at least 1\n', { status: 400 }));
    renderForm();
    fireEvent.change(samples(), { target: { value: '7' } });
    fireEvent.click(saveIn('Review'));
    await waitFor(() =>
      expect(within(section('Review')).getByRole('alert').textContent).toBe('review_n_requests must be at least 1')
    );
    expect(samples().value).toBe('7');
    expect(saveIn('Review').disabled).toBe(false);
    expect(within(section('Publishing')).queryByRole('alert')).toBeNull();
  });

  it('never renders generate_html', () => {
    const { container } = renderForm();
    expect(container.innerHTML).not.toContain('generate_html');
    expect(container.innerHTML.toLowerCase()).not.toContain('generate html');
  });

  it('asks before removing your own admin login and keeps it when cancelled', () => {
    vi.mocked(window.confirm).mockReturnValue(false);
    renderForm({ currentLogin: 'Alice' });
    fireEvent.click(within(section('Admins')).getByRole('button', { name: 'Remove alice' }));
    expect(window.confirm).toHaveBeenCalledWith('Remove your own admin access?');
    expect(within(section('Admins')).getByText('alice')).toBeTruthy();
    expect(saveIn('Admins').disabled).toBe(true);
  });

  it('removes another admin without asking', () => {
    renderForm();
    fireEvent.click(within(section('Admins')).getByRole('button', { name: 'Remove carol' }));
    expect(window.confirm).not.toHaveBeenCalled();
    expect(within(section('Admins')).queryByText('carol')).toBeNull();
    expect(saveIn('Admins').disabled).toBe(false);
  });

  it('disables the publishing policy and the Replies section while no author is enabled', () => {
    renderForm({}, { ...serverSettings, publish_enabled_authors: '', publish_reply_mode: 'off', publish_reply_enabled_at: '' });
    const notice = 'Nothing is posted, and no replies are processed, until at least one author is enabled';
    expect(within(section('Publishing')).getByText(notice)).toBeTruthy();
    expect(
      within(section('Replies')).getByText('Reply modes are available once at least one author is saved in Publishing.')
    ).toBeTruthy();
    expect(screen.queryAllByText(notice)).toHaveLength(1);
    expect((screen.getByLabelText('Inline comment cap') as HTMLInputElement).disabled).toBe(true);
    expect((screen.getByLabelText('Minimum inline severity') as HTMLSelectElement).disabled).toBe(true);
    expect((screen.getByLabelText('Show unverified findings') as HTMLInputElement).disabled).toBe(true);
    expect((screen.getByLabelText('Publish for authors') as HTMLInputElement).disabled).toBe(false);
    for (const radio of screen.getAllByRole('radio') as HTMLInputElement[]) expect(radio.disabled).toBe(true);
  });

  it('tells the admin to save Publishing once an author is added but not yet saved', () => {
    renderForm({}, { ...serverSettings, publish_enabled_authors: '', publish_reply_mode: 'off', publish_reply_enabled_at: '' });
    const input = screen.getByLabelText('Publish for authors') as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'carol' } });
    fireEvent.keyDown(input, { key: 'Enter' });
    expect(screen.queryByText(/until at least one author is enabled/)).toBeNull();
    expect(
      within(section('Replies')).getByText('Save the Publishing section above to enable reply modes.')
    ).toBeTruthy();
    for (const radio of screen.getAllByRole('radio') as HTMLInputElement[]) expect(radio.disabled).toBe(true);
    expect(saveIn('Publishing').disabled).toBe(false);
  });

  it('renders the reply totals strip when provided', () => {
    renderForm({ replyTotals: { total: 12, by_action: { react: 8, respond: 4 }, unlinked_roots: 1 } });
    expect(screen.getByText('12 replies, react: 8, respond: 4, 1 unlinked roots')).toBeTruthy();
    cleanup();
    renderForm({ replyTotals: null });
    expect(screen.queryByText(/unlinked roots/)).toBeNull();
  });
});
