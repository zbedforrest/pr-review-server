import { renderHook } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { PR } from '@/types/pr';
import { useNeedsReReview } from './useNeedsReReview';

const usePRsMock = vi.fn();

vi.mock('@/hooks/usePRs', () => ({
  usePRs: () => usePRsMock(),
}));

const makePR = (partial: Partial<PR>): PR => ({
  owner: 'acme',
  repo: 'example',
  number: 1,
  commit_sha: 'abc123',
  last_reviewed_at: null,
  review_html_path: '',
  github_url: '',
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
  ...partial,
});

describe('useNeedsReReview', () => {
  beforeEach(() => {
    usePRsMock.mockReturnValue({ data: undefined, isLoading: true, error: null });
  });

  it('returns nothing while the PR query has no data', () => {
    const { result } = renderHook(() => useNeedsReReview());

    expect(result.current.rows).toEqual([]);
    expect(result.current.keys.size).toBe(0);
    expect(result.current.count).toBe(0);
  });

  it('keeps only flagged PRs that are not hidden', () => {
    usePRsMock.mockReturnValue({
      data: [
        makePR({ number: 1, title: 'Flagged', needs_attention: true }),
        makePR({ number: 2, title: 'Not flagged' }),
        makePR({ number: 3, title: 'Flagged but hidden', needs_attention: true, hidden: true }),
        makePR({ number: 4, title: 'Older payload without the field', needs_attention: undefined }),
      ],
      isLoading: false,
      error: null,
    });

    const { result } = renderHook(() => useNeedsReReview());

    expect(result.current.rows.map((pr) => pr.title)).toEqual(['Flagged']);
    expect(result.current.count).toBe(1);
    expect(result.current.keys).toEqual(new Set(['acme/example/1']));
  });

  it('sorts oldest PR first, keeps ties in input order, and puts missing dates last', () => {
    usePRsMock.mockReturnValue({
      data: [
        makePR({ number: 1, title: 'Undated first', created_at: null, needs_attention: true }),
        makePR({ number: 2, title: 'Newest', created_at: '2026-05-01T00:00:00Z', needs_attention: true }),
        makePR({ number: 3, title: 'Tie A', created_at: '2026-04-01T00:00:00Z', needs_attention: true }),
        makePR({ number: 4, title: 'Undated second', created_at: null, needs_attention: true }),
        makePR({ number: 5, title: 'Tie B', created_at: '2026-04-01T00:00:00Z', needs_attention: true }),
        makePR({ number: 6, title: 'Oldest', created_at: '2026-03-01T00:00:00Z', needs_attention: true }),
      ],
      isLoading: false,
      error: null,
    });

    const { result } = renderHook(() => useNeedsReReview());

    expect(result.current.rows.map((pr) => pr.title)).toEqual([
      'Oldest',
      'Tie A',
      'Tie B',
      'Newest',
      'Undated first',
      'Undated second',
    ]);
    expect(result.current.count).toBe(6);
  });

  it('keys every returned row and nothing else', () => {
    usePRsMock.mockReturnValue({
      data: [
        makePR({ owner: 'acme', repo: 'example', number: 7, needs_attention: true }),
        makePR({ owner: 'acme', repo: 'other', number: 7, needs_attention: true }),
        makePR({ owner: 'acme', repo: 'other', number: 8 }),
      ],
      isLoading: false,
      error: null,
    });

    const { result } = renderHook(() => useNeedsReReview());

    expect(result.current.keys).toEqual(new Set(['acme/example/7', 'acme/other/7']));
  });
});
