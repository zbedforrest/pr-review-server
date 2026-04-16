import { cleanup, render, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ReviewPRsSection } from './ReviewPRsSection';
import type { PR } from '@/types/pr';

const usePRsMock = vi.fn();
const useCurrentUserMock = vi.fn();
const useTelemetryMock = vi.fn();

vi.mock('@/hooks/usePRs', () => ({
  usePRs: () => usePRsMock(),
}));

vi.mock('@/hooks/useCurrentUser', () => ({
  useCurrentUser: () => useCurrentUserMock(),
}));

vi.mock('@/hooks/useTelemetry', () => ({
  useTelemetry: () => useTelemetryMock(),
}));

vi.mock('./PRTable', () => ({
  PRTable: ({ prs }: { prs: PR[] }) => (
    <div>
      <div>PR table</div>
      <div data-testid="pr-table-rows">{prs.map((pr) => pr.title).join('|')}</div>
    </div>
  ),
}));

vi.mock('@/components/common', () => ({
  LoadingSpinner: () => <div>Loading</div>,
  ErrorMessage: ({ message }: { message: string }) => <div>{message}</div>,
}));

vi.mock('./AutoReviewToggle', () => ({
  AutoReviewToggle: () => <button>Auto Review</button>,
}));

vi.mock('../filters', () => ({
  FilterBar: () => <div>Filter Bar</div>,
}));

vi.mock('./triageUtils', () => ({
  categorizePR: () => 'low',
}));

describe('ReviewPRsSection', () => {
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
    usePRsMock.mockReturnValue({
      data: [],
      isLoading: false,
      error: null,
    });
    useCurrentUserMock.mockReturnValue({
      data: { github_username: 'alice' },
    });
  });

  afterEach(() => {
    cleanup();
  });

  it('tracks the review page view once on mount', async () => {
    const track = vi.fn();
    useTelemetryMock.mockReturnValue({ track });

    render(<ReviewPRsSection />);

    await waitFor(() => {
      expect(track).toHaveBeenCalledWith('view_review_prs_page');
    });

    expect(track).toHaveBeenCalledTimes(1);
  });

  it('applies search, team, and repo filters as an intersection for review PRs', () => {
    useTelemetryMock.mockReturnValue({ track: vi.fn() });
    usePRsMock.mockReturnValue({
      data: [
        makePR({
          number: 11,
          title: 'Viewer billing fix',
          repo: 'test-repo',
          via_teams: ['Viewer Team:pending'],
        }),
        makePR({
          number: 12,
          title: 'Viewer chat cleanup',
          repo: 'other-repo',
          via_teams: ['Viewer Team:pending'],
        }),
        makePR({
          number: 13,
          title: 'Creator billing fix',
          repo: 'test-repo',
          via_teams: ['Creator Team:pending'],
        }),
        makePR({
          number: 14,
          title: 'My own billing fix',
          repo: 'test-repo',
          via_teams: ['Viewer Team:pending'],
          is_mine: true,
        }),
      ],
      isLoading: false,
      error: null,
    });

    const { getByText, getAllByTestId } = render(
      <ReviewPRsSection
        searchTerm="billing"
        selectedTeams={['Viewer Team']}
        selectedRepos={['test-org/test-repo']}
      />
    );

    expect(getByText('PRs to Review (1)')).toBeTruthy();

    const tables = getAllByTestId('pr-table-rows');
    expect(tables[0].textContent).toContain('My own billing fix');
    expect(tables[1].textContent).toBe('Viewer billing fix');
  });

  it('matches personal team filters after username normalization', () => {
    useTelemetryMock.mockReturnValue({ track: vi.fn() });
    usePRsMock.mockReturnValue({
      data: [
        makePR({
          number: 21,
          title: 'Personally requested PR',
          via_teams: ['__PERSONAL__'],
        }),
        makePR({
          number: 22,
          title: 'Other team PR',
          via_teams: ['Viewer Team:pending'],
        }),
      ],
      isLoading: false,
      error: null,
    });

    const { getByText, getByTestId } = render(
      <ReviewPRsSection selectedTeams={['@alice']} />
    );

    expect(getByText('PRs to Review (1)')).toBeTruthy();
    expect(getByTestId('pr-table-rows').textContent).toContain('Personally requested PR');
  });
});
