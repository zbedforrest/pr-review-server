import { cleanup, fireEvent, render } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { StatusBar } from './StatusBar';
import type { ServerStatus } from '@/types/status';

const useStatusMock = vi.fn();

vi.mock('@/hooks/useStatus', () => ({
  useStatus: () => useStatusMock(),
}));

describe('StatusBar', () => {
  const makeStatus = (partial: Partial<ServerStatus> = {}): ServerStatus => ({
    uptime_seconds: 60,
    server_time_unix: 1000,
    server_started_at_unix: 940,
    reviewer_running: false,
    reviewer_duration_seconds: 0,
    reviewer_started_at_unix: null,
    counts: { completed: 13, generating: 1, pending: 1082, error: 0 },
    recent_completions: [],
    missing_metadata_count: 0,
    timestamp: 1000,
    seconds_until_next_poll: 30,
    next_poll_at_unix: 1030,
    rate_limit: {
      remaining: 4999,
      limit: 5000,
      reset_at: '2026-04-15T12:00:00Z',
      is_limited: false,
      error: '',
      graphql_remaining: 11660,
      graphql_limit: 12500,
      graphql_reset_at: '2026-04-15T12:00:00Z',
    },
    ...partial,
  });

  beforeEach(() => {
    useStatusMock.mockReturnValue({ data: makeStatus(), isLoading: false, error: null });
  });

  afterEach(() => {
    cleanup();
  });

  it('reports the clicked status and its count', () => {
    const onStatusCountClick = vi.fn();
    const { getByText } = render(<StatusBar onStatusCountClick={onStatusCountClick} />);

    fireEvent.click(getByText('13 completed'));
    expect(onStatusCountClick).toHaveBeenCalledWith('completed', 13);

    fireEvent.click(getByText('1 generating'));
    expect(onStatusCountClick).toHaveBeenCalledWith('generating', 1);
  });

  it('marks the open status as pressed', () => {
    const { getByText } = render(
      <StatusBar onStatusCountClick={vi.fn()} activeStatusCount="completed" />
    );

    expect(getByText('13 completed').getAttribute('aria-pressed')).toBe('true');
    expect(getByText('1 generating').getAttribute('aria-pressed')).toBe('false');
  });

  it('leaves pending unclickable and the counts as plain text without a handler', () => {
    const { container, queryByText } = render(<StatusBar />);

    expect(container.querySelectorAll('.status-bar__count-btn').length).toBe(0);
    expect(queryByText(/13 completed, 1 generating, 1082 pending/)).toBeTruthy();
  });
});
