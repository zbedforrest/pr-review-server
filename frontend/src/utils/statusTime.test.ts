import { describe, expect, it } from 'vitest';
import type { ServerStatus } from '@/types/status';
import { deriveStatusTimes } from './statusTime';

function makeStatus(partial: Partial<ServerStatus>): ServerStatus {
  return {
    uptime_seconds: 120,
    server_time_unix: 1000,
    server_started_at_unix: 880,
    reviewer_running: false,
    reviewer_duration_seconds: 0,
    reviewer_started_at_unix: null,
    counts: {
      completed: 1,
      generating: 0,
      pending: 2,
      error: 0,
    },
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
      graphql_remaining: 4999,
      graphql_limit: 5000,
      graphql_reset_at: '2026-04-15T12:00:00Z',
    },
    ...partial,
  };
}

describe('deriveStatusTimes', () => {
  it('ticks uptime and next poll locally from absolute timestamps', () => {
    const status = makeStatus({});

    const derived = deriveStatusTimes(
      status,
      5_000,
      0
    );

    expect(derived.uptimeSeconds).toBe(125);
    expect(derived.nextPollSeconds).toBe(25);
  });

  it('never lets next poll go negative', () => {
    const status = makeStatus({
      next_poll_at_unix: 1002,
      seconds_until_next_poll: 2,
    });

    const derived = deriveStatusTimes(
      status,
      10_000,
      0
    );

    expect(derived.nextPollSeconds).toBe(0);
  });

  it('falls back to legacy server-computed values when absolute times are absent', () => {
    const status = makeStatus({
      server_started_at_unix: 0,
      next_poll_at_unix: null,
      uptime_seconds: 42,
      seconds_until_next_poll: 9,
    });

    const derived = deriveStatusTimes(status, 50_000, 0);

    expect(derived.uptimeSeconds).toBe(42);
    expect(derived.nextPollSeconds).toBe(9);
  });

  it('derives reviewer duration from reviewer_started_at_unix when available', () => {
    const status = makeStatus({
      reviewer_running: true,
      reviewer_duration_seconds: 7,
      reviewer_started_at_unix: 995,
    });

    const derived = deriveStatusTimes(status, 5_000, 0);

    expect(derived.reviewerDurationSeconds).toBe(10);
  });
});
