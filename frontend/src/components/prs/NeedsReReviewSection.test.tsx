import { cleanup, render, screen, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { PR } from '@/types/pr';
import { NeedsReReviewSection } from './NeedsReReviewSection';

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
  useTelemetry: () => ({ track: vi.fn() }),
}));
vi.mock('@/components/common', () => ({
  CommitSha: () => <span>sha</span>,
}));
vi.mock('./CIStatusIndicator', () => ({ CIStatusIndicator: () => <span>ci</span> }));
vi.mock('./NotesCell', () => ({ NotesCell: () => <span>notes</span> }));
vi.mock('./RowActionsMenu', () => ({ RowActionsMenu: () => <span>actions</span> }));
vi.mock('./ReviewLinkMenu', () => ({ ReviewLinkMenu: () => <span>review menu</span> }));

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

describe('NeedsReReviewSection', () => {
  beforeEach(() => setPRs([]));
  afterEach(() => cleanup());

  it('renders nothing at all when no PR needs re-review', () => {
    setPRs([makePR({ number: 1, needs_attention: false }), makePR({ number: 2, hidden: true })]);

    const { container } = render(<NeedsReReviewSection />);

    expect(container.firstChild).toBeNull();
  });

  it('shows the count in an always-expanded heading with the explanation', () => {
    setPRs([makePR({ number: 1, title: 'First' }), makePR({ number: 2, title: 'Second' })]);

    render(<NeedsReReviewSection />);

    const heading = screen.getByRole('heading', { level: 2, name: 'Needs your re-review (2)' });
    expect(within(heading).queryByRole('button')).toBeNull();
    expect(
      screen.getByText('You requested changes. These PRs now have a different head commit.')
    ).toBeTruthy();
    expect(screen.getByText('First')).toBeTruthy();
    expect(screen.getByText('Second')).toBeTruthy();
  });

  it('badges every row as updated since the review, with the head-differs tooltip', () => {
    setPRs([makePR({ number: 1 }), makePR({ number: 2 })]);

    render(<NeedsReReviewSection />);

    const badges = screen.getAllByText('Updated since your review');
    expect(badges).toHaveLength(2);
    for (const badge of badges) {
      expect(badge.getAttribute('title')).toBe(
        'The current head differs from the commit you reviewed when requesting changes'
      );
    }
  });

  it('links each row to the PR files tab on GitHub in a new tab', () => {
    setPRs([makePR({ owner: 'acme', repo: 'example', number: 42 })]);

    render(<NeedsReReviewSection />);

    const link = screen.getByRole('link', { name: 'Review on GitHub' });
    expect(link.getAttribute('href')).toBe('https://github.com/acme/example/pull/42/files');
    expect(link.getAttribute('target')).toBe('_blank');
    expect(link.getAttribute('rel') ?? '').toContain('noreferrer');
  });

  it('adds and removes rows as the PR query data changes', () => {
    setPRs([makePR({ number: 1, title: 'Already flagged' })]);
    const { rerender } = render(<NeedsReReviewSection />);
    expect(screen.getByRole('heading', { level: 2, name: 'Needs your re-review (1)' })).toBeTruthy();

    setPRs([
      makePR({ number: 1, title: 'Already flagged' }),
      makePR({ number: 2, title: 'Author just pushed' }),
    ]);
    rerender(<NeedsReReviewSection />);
    expect(screen.getByRole('heading', { level: 2, name: 'Needs your re-review (2)' })).toBeTruthy();
    expect(screen.getByText('Author just pushed')).toBeTruthy();

    setPRs([
      makePR({ number: 1, title: 'Already flagged', needs_attention: false }),
      makePR({ number: 2, title: 'Author just pushed' }),
    ]);
    rerender(<NeedsReReviewSection />);
    expect(screen.getByRole('heading', { level: 2, name: 'Needs your re-review (1)' })).toBeTruthy();
    expect(screen.queryByText('Already flagged')).toBeNull();

    setPRs([makePR({ number: 2, title: 'Author just pushed', hidden: true })]);
    rerender(<NeedsReReviewSection />);
    expect(screen.queryByRole('heading', { level: 2 })).toBeNull();
  });
});
