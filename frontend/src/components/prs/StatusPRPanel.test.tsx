import { cleanup, fireEvent, render } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { StatusPRPanel } from './StatusPRPanel';
import type { PR } from '@/types/pr';

const usePRsMock = vi.fn();

vi.mock('@/hooks/usePRs', () => ({
  usePRs: () => usePRsMock(),
}));

vi.mock('./PRTable', () => ({
  PRTable: ({ prs }: { prs: PR[] }) => (
    <div data-testid="pr-table-rows">{prs.map((pr) => pr.title).join('|')}</div>
  ),
}));

describe('StatusPRPanel', () => {
  const makePR = (partial: Partial<PR>): PR => ({
    owner: 'test-org',
    repo: 'test-repo',
    number: 1,
    commit_sha: 'abc123',
    last_reviewed_at: null,
    review_html_path: '',
    github_url: 'https://github.com/test-org/test-repo/pull/1',
    review_url: '',
    status: 'pending',
    title: 'Example PR',
    author: 'alice',
    generating_since: null,
    approval_count: 0,
    my_review_status: '',
    draft: false,
    ci_state: 'unknown',
    ci_failed_checks: [],
    created_at: '2026-04-15T12:00:00Z',
    is_mine: false,
    via_teams: [],
    critical_count: 0,
    medium_count: 0,
    low_count: 0,
    notes: '',
    ...partial,
  });

  beforeEach(() => {
    usePRsMock.mockReturnValue({ data: [], isLoading: false, error: null });
  });

  afterEach(() => {
    cleanup();
  });

  it('shows only PRs in the clicked status, newest first', () => {
    usePRsMock.mockReturnValue({
      data: [
        makePR({ number: 1, title: 'Older done', status: 'completed', created_at: '2026-04-01T12:00:00Z' }),
        makePR({ number: 2, title: 'In flight', status: 'generating' }),
        makePR({ number: 3, title: 'Newer done', status: 'completed', created_at: '2026-04-10T12:00:00Z' }),
        makePR({ number: 4, title: 'Waiting', status: 'pending' }),
      ],
      isLoading: false,
      error: null,
    });

    const { getByText, getByTestId } = render(
      <StatusPRPanel status="completed" onClose={vi.fn()} />
    );

    expect(getByText('Completed (2)')).toBeTruthy();
    expect(getByTestId('pr-table-rows').textContent).toBe('Newer done|Older done');
  });

  it('ignores the Hidden section so the status is the only filter', () => {
    usePRsMock.mockReturnValue({
      data: [
        makePR({ number: 1, title: 'Visible done', status: 'completed' }),
        makePR({ number: 2, title: 'Hidden done', status: 'completed', hidden: true }),
      ],
      isLoading: false,
      error: null,
    });

    const { getByText, getByTestId } = render(
      <StatusPRPanel status="completed" onClose={vi.fn()} />
    );

    expect(getByText('Completed (2)')).toBeTruthy();
    expect(getByTestId('pr-table-rows').textContent).toContain('Hidden done');
  });

  it('spells out the server-wide count when it exceeds the visible rows', () => {
    usePRsMock.mockReturnValue({
      data: [makePR({ number: 1, title: 'Mine', status: 'generating' })],
      isLoading: false,
      error: null,
    });

    const { getByText } = render(
      <StatusPRPanel status="generating" serverCount={7} onClose={vi.fn()} />
    );

    expect(getByText('1 of 7 server-wide')).toBeTruthy();
  });

  it('omits the server-wide note when the counts agree', () => {
    usePRsMock.mockReturnValue({
      data: [makePR({ number: 1, title: 'Mine', status: 'generating' })],
      isLoading: false,
      error: null,
    });

    const { queryByText } = render(
      <StatusPRPanel status="generating" serverCount={1} onClose={vi.fn()} />
    );

    expect(queryByText(/server-wide/)).toBeNull();
  });

  it('closes on the X button', () => {
    const onClose = vi.fn();
    const { getByLabelText } = render(<StatusPRPanel status="completed" onClose={onClose} />);

    fireEvent.click(getByLabelText('Close Completed PRs'));

    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('closes on Escape', () => {
    const onClose = vi.fn();
    render(<StatusPRPanel status="completed" onClose={onClose} />);

    fireEvent.keyDown(document, { key: 'Escape' });

    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('leaves Escape to an open row menu', () => {
    const onClose = vi.fn();
    render(<StatusPRPanel status="completed" onClose={onClose} />);

    const menu = document.createElement('div');
    menu.setAttribute('role', 'menu');
    document.body.appendChild(menu);
    try {
      fireEvent.keyDown(document, { key: 'Escape' });
      expect(onClose).not.toHaveBeenCalled();
    } finally {
      menu.remove();
    }
  });

  it('keeps its Escape listener ahead of a row menu opened later, across re-renders', () => {
    // A fresh onClose identity on every parent render used to re-register the
    // panel's listener behind the row menu's, so Escape closed both at once.
    const onClose = vi.fn();
    const { rerender } = render(<StatusPRPanel status="completed" onClose={() => onClose()} />);
    rerender(<StatusPRPanel status="completed" onClose={() => onClose()} />);

    // Stand in for a row menu that opened after the panel: it appends its own
    // document listener, then removes itself when Escape reaches it.
    const menu = document.createElement('div');
    menu.setAttribute('role', 'menu');
    document.body.appendChild(menu);
    const menuListener = () => menu.remove();
    document.addEventListener('keydown', menuListener);

    try {
      fireEvent.keyDown(document, { key: 'Escape' });
      expect(onClose).not.toHaveBeenCalled();
      expect(document.querySelector('[role="menu"]')).toBeNull();

      // With the menu gone, Escape closes the panel.
      fireEvent.keyDown(document, { key: 'Escape' });
      expect(onClose).toHaveBeenCalledTimes(1);
    } finally {
      document.removeEventListener('keydown', menuListener);
      menu.remove();
    }
  });

  it('closes on Escape using the latest onClose after a re-render', () => {
    const stale = vi.fn();
    const fresh = vi.fn();
    const { rerender } = render(<StatusPRPanel status="completed" onClose={stale} />);
    rerender(<StatusPRPanel status="completed" onClose={fresh} />);

    fireEvent.keyDown(document, { key: 'Escape' });

    expect(stale).not.toHaveBeenCalled();
    expect(fresh).toHaveBeenCalledTimes(1);
  });

  it('shows an empty state when nothing in the user list has that status', () => {
    const { getByText, queryByTestId } = render(
      <StatusPRPanel status="generating" onClose={vi.fn()} />
    );

    expect(getByText('No generating PRs in your list')).toBeTruthy();
    expect(queryByTestId('pr-table-rows')).toBeNull();
  });
});
