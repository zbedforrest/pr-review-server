import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { createRef } from 'react';
import { APIError } from '@/api/client';
import type { PR } from '@/types/pr';
import { QuickActionDialog } from './QuickActionDialog';

const makePR = (partial: Partial<PR> = {}): PR => ({
  owner: 'acme',
  repo: 'example',
  number: 123,
  commit_sha: 'abc1234def',
  last_reviewed_at: null,
  review_html_path: '',
  github_url: '',
  review_url: '',
  status: 'completed',
  title: 'Fix race in poller lease renewal',
  author: 'bob',
  generating_since: null,
  approval_count: 0,
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

type Props = React.ComponentProps<typeof QuickActionDialog>;

const renderDialog = (overrides: Partial<Props> = {}) => {
  const props: Props = {
    pr: makePR(),
    action: 'approve',
    login: 'alice',
    onSubmit: vi.fn(),
    pending: false,
    error: null,
    onClose: vi.fn(),
    ...overrides,
  };
  const view = render(<QuickActionDialog {...props} />);
  return { props, view };
};

const textarea = () => screen.getByRole('textbox') as HTMLTextAreaElement;
const primary = (name: RegExp) => screen.getByRole('button', { name }) as HTMLButtonElement;

describe('QuickActionDialog', () => {
  afterEach(() => cleanup());

  it('titles the dialog by action and shows who it posts as', () => {
    renderDialog({ action: 'request_changes' });
    const dialog = screen.getByRole('dialog');
    expect(dialog.getAttribute('aria-modal')).toBe('true');
    expect(screen.getByRole('heading').textContent).toBe('Request changes on acme/example #123');
    expect(screen.getByText(/Posts to GitHub as @alice/).textContent).toContain('Reviewed head abc1234');
  });

  it('request changes submit stays disabled until text is entered', () => {
    const { props } = renderDialog({ action: 'request_changes' });
    expect(primary(/^Request changes$/).disabled).toBe(true);
    fireEvent.change(textarea(), { target: { value: '   ' } });
    expect(primary(/^Request changes$/).disabled).toBe(true);
    fireEvent.change(textarea(), { target: { value: 'Please add a test' } });
    expect(primary(/^Request changes$/).disabled).toBe(false);
    fireEvent.click(primary(/^Request changes$/));
    expect(props.onSubmit).toHaveBeenCalledWith('Please add a test', 'abc1234def');
  });

  it('approve submits with empty body and focuses the primary button initially', () => {
    const { props } = renderDialog();
    expect(document.activeElement).toBe(primary(/^Approve$/));
    fireEvent.click(primary(/^Approve$/));
    expect(props.onSubmit).toHaveBeenCalledWith('', 'abc1234def');
  });

  it('focuses the textarea initially for comment', () => {
    renderDialog({ action: 'comment' });
    expect(document.activeElement).toBe(textarea());
    expect(primary(/^Comment$/).disabled).toBe(true);
  });

  it('Ctrl+Enter and Cmd+Enter submit, plain Enter does not', () => {
    const { props, view } = renderDialog({ action: 'comment' });
    fireEvent.change(textarea(), { target: { value: 'nit' } });
    fireEvent.keyDown(textarea(), { key: 'Enter', ctrlKey: true });
    expect(props.onSubmit).toHaveBeenCalledTimes(1);
    view.rerender(<QuickActionDialog {...props} pending={true} />);
    view.rerender(<QuickActionDialog {...props} pending={false} />);
    fireEvent.keyDown(textarea(), { key: 'Enter', metaKey: true });
    expect(props.onSubmit).toHaveBeenCalledTimes(2);
    fireEvent.keyDown(textarea(), { key: 'Enter' });
    expect(props.onSubmit).toHaveBeenCalledTimes(2);
  });

  it('ignores a second submit before the pending state has rendered', () => {
    const { props, view } = renderDialog();
    fireEvent.click(primary(/^Approve$/));
    fireEvent.click(primary(/^Approve$/));
    fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Enter', ctrlKey: true });
    expect(props.onSubmit).toHaveBeenCalledTimes(1);
    view.rerender(<QuickActionDialog {...props} pending={true} />);
    view.rerender(<QuickActionDialog {...props} pending={false} error={new APIError('no', 403, 'Forbidden', 'no_permission')} />);
    fireEvent.click(primary(/^Approve$/));
    expect(props.onSubmit).toHaveBeenCalledTimes(2);
  });

  it('Escape cancels and does not reach document listeners', () => {
    const { props } = renderDialog();
    const docListener = vi.fn();
    document.addEventListener('keydown', docListener);
    fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Escape' });
    document.removeEventListener('keydown', docListener);
    expect(props.onClose).toHaveBeenCalledTimes(1);
    expect(docListener).not.toHaveBeenCalled();
  });

  it('shows head-moved confirmation and resubmits with the new sha', () => {
    const { props } = renderDialog({
      error: new APIError('head moved', 409, 'Conflict', 'head_moved', { head_sha: 'def5678abc' }),
      headMovedTo: 'def5678abc',
    });
    expect(screen.getByRole('status').textContent).toBe(
      'The PR head moved from abc1234 to def5678 since this row loaded. Approve the new head?'
    );
    expect(screen.queryByRole('alert')).toBeNull();
    fireEvent.click(primary(/^Approve def5678$/));
    expect(props.onSubmit).toHaveBeenCalledWith('', 'def5678abc');
  });

  it('keeps the head it opened with when the row refreshes underneath it', () => {
    const onSubmit = vi.fn();
    const { view } = renderDialog({ onSubmit });
    view.rerender(
      <QuickActionDialog pr={makePR({ commit_sha: 'def5678abc' })} action="approve" login="alice" onSubmit={onSubmit} pending={false} error={null} onClose={vi.fn()} />
    );
    expect(screen.getByText(/Reviewed head abc1234/)).toBeTruthy();
    fireEvent.click(primary(/^Approve$/));
    expect(onSubmit).toHaveBeenCalledWith('', 'abc1234def');
  });

  it('still shows the head-moved notice after the row catches up to the new head', () => {
    const error = new APIError('head moved', 409, 'Conflict', 'head_moved', { head_sha: 'def5678abc' });
    const { view } = renderDialog();
    view.rerender(
      <QuickActionDialog pr={makePR({ commit_sha: 'def5678abc' })} action="approve" login="alice" onSubmit={vi.fn()} pending={false} error={error} headMovedTo="def5678abc" onClose={vi.fn()} />
    );
    expect(screen.getByRole('status').textContent).toContain('moved from abc1234 to def5678');
    expect(screen.getByRole('button', { name: 'Approve def5678' })).toBeTruthy();
  });

  it('shows an error when head_moved carries no usable replacement sha', () => {
    renderDialog({ error: new APIError('head moved', 409, 'Conflict', 'head_moved') });
    expect(screen.queryByRole('status')).toBeNull();
    expect(screen.getByRole('alert').textContent).toContain('The PR head moved since this row loaded');
    cleanup();
    renderDialog({ error: new APIError('head moved', 409, 'Conflict', 'head_moved', { head_sha: 'abc1234def' }), headMovedTo: 'abc1234def' });
    expect(screen.getByRole('alert')).toBeTruthy();
  });

  it('keeps text and shows error on failure', () => {
    const { view } = renderDialog({ action: 'request_changes' });
    fireEvent.change(textarea(), { target: { value: 'needs work' } });
    view.rerender(
      <QuickActionDialog
        pr={makePR()}
        action="request_changes"
        login="alice"
        onSubmit={vi.fn()}
        pending={false}
        error={new APIError('cannot', 422, 'Unprocessable', 'own_pr')}
        onClose={vi.fn()}
      />
    );
    expect(textarea().value).toBe('needs work');
    const alert = screen.getByRole('alert');
    expect(alert.textContent).toContain('GitHub does not allow requesting changes on your own PR.');
    expect(screen.getByRole('link', { name: /Open on GitHub/ }).getAttribute('href')).toBe('https://github.com/acme/example/pull/123');
  });

  it('maps every error code to its copy and links sign-in for reauth', () => {
    const cases: [string, RegExp][] = [
      ['no_permission', /^boom/],
      ['reauth_required', /authorization expired/],
      ['pr_closed', /closed on GitHub/],
      ['draft_not_green', /^boom/],
      ['github_error', /GitHub returned an error: boom/],
    ];
    for (const [code, re] of cases) {
      renderDialog({ error: new APIError('boom', 500, 'x', code) });
      expect(screen.getByRole('alert').textContent).toMatch(re);
      cleanup();
    }
    renderDialog({ error: new APIError('', 403, 'Forbidden', 'no_permission') });
    expect(screen.getByRole('alert').textContent).toContain('cannot review this repository');
    cleanup();
    renderDialog({ error: new APIError('', 422, 'x', 'draft_not_green', { prism: 'green', greptile: 'red' }) });
    expect(screen.getByRole('alert').textContent).toContain('PRism and Greptile green');
    cleanup();
    renderDialog({ error: new APIError('expired', 428, 'x', 'reauth_required') });
    expect(screen.getByRole('link', { name: /Sign in/ }).getAttribute('href')).toBe('/login');
    cleanup();
    renderDialog({ error: new APIError('slow down', 429, 'x', 'rate_limited', { retry_after_seconds: '90' }) });
    expect(screen.getByRole('alert').textContent).toContain('try again in 2 minutes');
    cleanup();
    renderDialog({ error: new TypeError('Failed to fetch') });
    expect(screen.getByRole('alert').textContent).toContain('GitHub returned an error: Failed to fetch');
  });

  it('disables everything and relabels the button while pending', () => {
    renderDialog({ pending: true });
    expect(primary(/Approving…/).disabled).toBe(true);
    expect(textarea().disabled).toBe(true);
    expect((screen.getByRole('button', { name: 'Cancel' }) as HTMLButtonElement).disabled).toBe(true);
  });

  it('shows the counter only past 18,000 characters and caps at 20,000', () => {
    renderDialog({ action: 'comment' });
    expect(textarea().maxLength).toBe(20000);
    fireEvent.change(textarea(), { target: { value: 'x'.repeat(18000) } });
    expect(screen.queryByText(/20,000/)).toBeNull();
    fireEvent.change(textarea(), { target: { value: 'x'.repeat(18001) } });
    expect(screen.getByText('18,001 / 20,000')).toBeTruthy();
  });

  it('traps Tab inside the dialog', () => {
    renderDialog({ action: 'comment' });
    const close = screen.getByRole('button', { name: 'Close' });
    const approve = primary(/^Comment$/);
    fireEvent.change(textarea(), { target: { value: 'x' } });
    close.focus();
    fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Tab', shiftKey: true });
    expect(document.activeElement).toBe(approve);
    fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Tab' });
    expect(document.activeElement).toBe(close);
  });

  it('returns focus to the opener on close', () => {
    const opener = document.createElement('button');
    document.body.appendChild(opener);
    const ref = createRef<HTMLElement>() as React.RefObject<HTMLElement>;
    (ref as { current: HTMLElement | null }).current = opener;
    const { view } = renderDialog({ returnFocusRef: ref });
    expect(document.activeElement).not.toBe(opener);
    view.unmount();
    expect(document.activeElement).toBe(opener);
    opener.remove();
  });
});
