import { render, waitFor } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { ReviewPRsSection } from './ReviewPRsSection';

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
  PRTable: () => <div>PR table</div>,
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

  it('tracks the review page view once on mount', async () => {
    const track = vi.fn();
    useTelemetryMock.mockReturnValue({ track });

    render(<ReviewPRsSection />);

    await waitFor(() => {
      expect(track).toHaveBeenCalledWith('view_review_prs_page');
    });

    expect(track).toHaveBeenCalledTimes(1);
  });
});
