import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { PR } from '@/types/pr';
import App from './App';

const { usePRsMock } = vi.hoisted(() => ({ usePRsMock: vi.fn() }));

vi.mock('@/hooks/usePRs', () => ({
  usePRs: () => usePRsMock(),
  useDeletePR: () => ({ mutate: vi.fn(), isPending: false }),
  useSetPRHidden: () => ({ mutate: vi.fn(), isPending: false }),
  useTriggerReview: () => ({ mutate: vi.fn(), isPending: false }),
}));
vi.mock('@/hooks/useSettings', () => ({
  useSettings: () => ({ data: { auto_review_requested_prs: true, publish_enabled_authors: '*' } }),
}));
vi.mock('@/hooks/useTelemetry', () => ({
  useTelemetry: () => ({ track: vi.fn(), trackSearch: vi.fn() }),
}));
vi.mock('@/utils/websocket', () => ({
  subscribeToWebSocketMessages: () => () => {},
  subscribeToWebSocketStatus: () => () => {},
}));
vi.mock('@tanstack/react-query-devtools', () => ({ ReactQueryDevtools: () => null }));
vi.mock('@/components/layout', () => ({
  Header: () => (
    <header>
      <button>Refresh PRs</button>
    </header>
  ),
  StatusBar: () => <div className="status-bar" />,
}));
vi.mock('@/components/filters', () => ({
  FilterBar: () => <button>Filters</button>,
}));
vi.mock('@/components/prs/ReviewPRsSection', () => ({
  ReviewPRsSection: () => <section className="review-sections" />,
}));
vi.mock('@/components/common', () => ({
  CommitSha: () => <span>sha</span>,
}));
vi.mock('@/components/prs/CIStatusIndicator', () => ({ CIStatusIndicator: () => <span>ci</span> }));
vi.mock('@/components/prs/NotesCell', () => ({ NotesCell: () => <span>notes</span> }));
vi.mock('@/components/prs/RowActionsMenu', () => ({ RowActionsMenu: () => <span>actions</span> }));
vi.mock('@/components/prs/ReviewLinkMenu', () => ({ ReviewLinkMenu: () => <span>review menu</span> }));

const makePR = (partial: Partial<PR>): PR => ({
  owner: 'acme',
  repo: 'example',
  number: 1,
  commit_sha: 'abc123',
  last_reviewed_at: null,
  review_html_path: '',
  github_url: 'https://github.com/acme/example/pull/1',
  review_url: '',
  status: 'pending',
  title: 'Example PR',
  author: 'bob',
  generating_since: null,
  approval_count: 0,
  my_review_status: 'CHANGES_REQUESTED',
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
  needs_attention: true,
  ...partial,
});

const setPRs = (prs: PR[]) => usePRsMock.mockReturnValue({ data: prs, isLoading: false, error: null });

describe('App re-review attention', () => {
  beforeEach(() => {
    document.title = 'PRism';
    setPRs([]);
  });

  afterEach(() => {
    cleanup();
    document.title = '';
  });

  it('shows the same count in the tab title as in the section heading', () => {
    setPRs([makePR({ number: 1 }), makePR({ number: 2 }), makePR({ number: 3, needs_attention: false })]);

    render(<App />);

    expect(screen.getByRole('heading', { level: 2, name: 'Needs your re-review (2)' })).toBeTruthy();
    expect(document.title).toBe('(2) PRism');
  });

  it('keeps the plain title when nothing needs re-review', () => {
    setPRs([makePR({ number: 1, needs_attention: false })]);

    render(<App />);

    expect(screen.queryByRole('heading', { level: 2 })).toBeNull();
    expect(document.title).toBe('PRism');
  });

  it('places the section right after the header so its link is reached before the search controls', () => {
    setPRs([makePR({ number: 1 })]);

    const { container } = render(<App />);

    const section = container.querySelector('.needs-re-review');
    expect(section?.previousElementSibling?.className).toBe('status-bar');
    expect(section?.previousElementSibling?.previousElementSibling?.tagName).toBe('HEADER');
    expect(section?.nextElementSibling?.className).toBe('search-controls');

    const focusable = Array.from(container.querySelectorAll('a, button, input')).map(
      (el) => el.getAttribute('aria-label') ?? el.textContent ?? el.tagName
    );
    expect(focusable.indexOf('Review on GitHub')).toBeGreaterThan(focusable.indexOf('Refresh PRs'));
    expect(focusable.indexOf('Review on GitHub')).toBeLessThan(focusable.indexOf('Filters'));
  });
});
