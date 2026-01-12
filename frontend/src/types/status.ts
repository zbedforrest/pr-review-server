export interface StatusCounts {
  completed: number;
  generating: number;
  pending: number;
  error: number;
}

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
}

export interface ServerStatus {
  uptime_seconds: number;
  cbpr_running: boolean;
  cbpr_duration_seconds: number;
  counts: StatusCounts;
  recent_completions: RecentCompletion[];
  missing_metadata_count: number;
  timestamp: number;
  seconds_until_next_poll: number;
  rate_limit: RateLimitInfo;
}
