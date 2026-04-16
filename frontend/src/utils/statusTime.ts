import type { ServerStatus } from '@/types/status';

export interface DerivedStatusTimes {
  uptimeSeconds: number;
  nextPollSeconds: number | null;
  reviewerDurationSeconds: number | null;
}

export function deriveStatusTimes(
  status: ServerStatus,
  nowMs: number,
  snapshotReceivedAtMs: number
): DerivedStatusTimes {
  const elapsedSinceSnapshotSeconds = Math.max(0, Math.floor((nowMs - snapshotReceivedAtMs) / 1000));
  const derivedServerNowUnix = status.server_time_unix + elapsedSinceSnapshotSeconds;

  const uptimeSeconds = status.server_started_at_unix
    ? Math.max(status.uptime_seconds, derivedServerNowUnix - status.server_started_at_unix)
    : status.uptime_seconds;

  const nextPollSeconds = status.next_poll_at_unix !== null
    ? Math.max(0, status.next_poll_at_unix - derivedServerNowUnix)
    : status.seconds_until_next_poll;

  const reviewerDurationSeconds = status.reviewer_started_at_unix !== null
    ? Math.max(status.reviewer_duration_seconds, derivedServerNowUnix - status.reviewer_started_at_unix)
    : (status.reviewer_running ? status.reviewer_duration_seconds : null);

  return {
    uptimeSeconds,
    nextPollSeconds,
    reviewerDurationSeconds,
  };
}
