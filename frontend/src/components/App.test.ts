import { describe, expect, it } from 'vitest';
import { applyPRWebSocketMessage, applyStatusWebSocketMessage } from '@/utils/websocketCacheUpdates';
import type { PR } from '@/types/pr';
import type { ServerStatus } from '@/types/status';

function makePR(partial: Partial<PR>): PR {
  return {
    owner: 'owner',
    repo: 'repo',
    number: 1,
    commit_sha: 'abc123',
    last_reviewed_at: null,
    review_html_path: '',
    github_url: 'https://github.com/owner/repo/pull/1',
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
    created_at: null,
    is_mine: false,
    via_teams: [],
    critical_count: 0,
    medium_count: 0,
    low_count: 0,
    notes: '',
    ...partial,
  };
}

function makeStatus(partial: Partial<ServerStatus>): ServerStatus {
  return {
    uptime_seconds: 10,
    server_time_unix: 1000,
    server_started_at_unix: 990,
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

describe('App websocket cache helpers', () => {
  it('replaces cached status on status_snapshot messages', () => {
    const existing = makeStatus({ timestamp: 1000, counts: { completed: 1, generating: 0, pending: 2, error: 0 } });
    const incoming = makeStatus({ timestamp: 2000, counts: { completed: 3, generating: 1, pending: 4, error: 0 } });

    const next = applyStatusWebSocketMessage(existing, {
      type: 'status_snapshot',
      payload: incoming,
    });

    expect(next).toEqual(expect.objectContaining(incoming));
    expect(next?.snapshot_received_at_ms).toEqual(expect.any(Number));
  });

  it('ignores non-status websocket messages for status cache', () => {
    const existing = makeStatus({ timestamp: 1000 });

    const next = applyStatusWebSocketMessage(existing, {
      type: 'pr_updated',
      payload: makePR({ number: 2 }),
    });

    expect(next).toEqual(existing);
  });

  it('preserves is_mine when updating an existing PR', () => {
    const existing = [makePR({ number: 7, is_mine: true, title: 'Old title' })];
    const incoming = makePR({ number: 7, is_mine: false, title: 'New title' });

    const next = applyPRWebSocketMessage(existing, {
      type: 'pr_updated',
      payload: incoming,
    });

    expect(next).toEqual([
      expect.objectContaining({
        number: 7,
        title: 'New title',
        is_mine: true,
      }),
    ]);
  });
});
