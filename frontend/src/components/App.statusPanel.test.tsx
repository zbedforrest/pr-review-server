import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { StatusPanelFilter } from '@/types/status';
import App from './App';

const { trackMock } = vi.hoisted(() => ({ trackMock: vi.fn() }));

vi.mock('@/hooks/usePRs', () => ({
  usePRs: () => ({ data: [], isLoading: false, error: null }),
}));
vi.mock('@/hooks/useStatus', () => ({
  useStatus: () => ({ data: undefined }),
}));
vi.mock('@/hooks/useTelemetry', () => ({
  useTelemetry: () => ({ track: trackMock, trackSearch: vi.fn() }),
}));
vi.mock('@/utils/websocket', () => ({
  subscribeToWebSocketMessages: () => () => {},
  subscribeToWebSocketStatus: () => () => {},
}));
vi.mock('@tanstack/react-query-devtools', () => ({ ReactQueryDevtools: () => null }));
vi.mock('@/components/layout', () => ({
  Header: () => <header />,
  StatusBar: ({ onStatusCountClick }: { onStatusCountClick: (status: StatusPanelFilter) => void }) => (
    <div className="status-bar">
      <button onClick={() => onStatusCountClick('completed')}>13 completed</button>
      <button onClick={() => onStatusCountClick('generating')}>2 generating</button>
    </div>
  ),
}));
vi.mock('@/components/filters', () => ({
  FilterBar: () => <button>Filters</button>,
}));
vi.mock('@/components/prs/ReviewPRsSection', () => ({
  ReviewPRsSection: () => <section className="review-sections" />,
}));
vi.mock('@/components/prs/NeedsReReviewSection', () => ({
  NeedsReReviewSection: () => null,
}));

describe('App status panel', () => {
  beforeEach(() => {
    trackMock.mockReset();
  });

  afterEach(() => {
    cleanup();
  });

  const openPanel = () => {
    fireEvent.click(screen.getByText('13 completed'));
    expect(screen.getByTestId('status-pr-panel')).toBeTruthy();
    expect(trackMock).toHaveBeenCalledWith('status_count_panel', { label: 'open:completed' });
    trackMock.mockClear();
  };

  it('tracks the close of the previous status when switching to the other count', () => {
    render(<App />);
    openPanel();

    fireEvent.click(screen.getByText('2 generating'));

    expect(trackMock).toHaveBeenCalledWith('status_count_panel', { label: 'close:completed' });
    expect(trackMock).toHaveBeenCalledWith('status_count_panel', { label: 'open:generating' });
  });

  it('tracks the close when the X button closes the panel', () => {
    render(<App />);
    openPanel();

    fireEvent.click(screen.getByLabelText('Close Completed PRs'));

    expect(screen.queryByTestId('status-pr-panel')).toBeNull();
    expect(trackMock).toHaveBeenCalledWith('status_count_panel', { label: 'close:completed' });
  });

  it('tracks the close when Escape closes the panel', () => {
    render(<App />);
    openPanel();

    fireEvent.keyDown(document, { key: 'Escape' });

    expect(screen.queryByTestId('status-pr-panel')).toBeNull();
    expect(trackMock).toHaveBeenCalledWith('status_count_panel', { label: 'close:completed' });
  });

  it('tracks the close when the open count is clicked again', () => {
    render(<App />);
    openPanel();

    fireEvent.click(screen.getByText('13 completed'));

    expect(screen.queryByTestId('status-pr-panel')).toBeNull();
    expect(trackMock).toHaveBeenCalledWith('status_count_panel', { label: 'close:completed' });
    expect(trackMock).toHaveBeenCalledTimes(1);
  });
});
