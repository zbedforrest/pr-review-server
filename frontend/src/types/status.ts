export interface StatusCounts {
  completed: number;
  generating: number;
  pending: number;
  error: number;
}

/**
 * The status-bar counts that are clickable, opening a PR table filtered to
 * that status. Pending is deliberately left out: it routinely runs to
 * thousands of PRs, which is a queue depth rather than a list worth reading.
 */
export type StatusPanelFilter = 'completed' | 'generating';

export interface RecentCompletion {
  number: number;
  repo: string;
  reviewed_at: string;
}

export interface RateLimitInfo {
  remaining: number;
  limit: number;
  reset_at: string;
  is_limited: boolean;
  error: string;
  graphql_remaining: number;
  graphql_limit: number;
  graphql_reset_at: string;
}

export interface ServerStatus {
  uptime_seconds: number;
  server_time_unix: number;
  server_started_at_unix: number;
  reviewer_running: boolean;
  reviewer_duration_seconds: number;
  reviewer_started_at_unix: number | null;
  counts: StatusCounts;
  recent_completions: RecentCompletion[];
  missing_metadata_count: number;
  timestamp: number;
  seconds_until_next_poll: number;
  next_poll_at_unix: number | null;
  rate_limit: RateLimitInfo;
  snapshot_received_at_ms?: number;
}
