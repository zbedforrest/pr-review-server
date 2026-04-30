import { useEffect, useMemo, useState } from 'react';
import { useStatus } from '@/hooks/useStatus';
import { formatUptime } from '@/utils/formatUptime';
import { formatTime } from '@/utils/formatDate';
import { deriveStatusTimes } from '@/utils/statusTime';

interface StatusBarProps {
  connectionStatus?: 'connected' | 'disconnected' | 'connecting';
}

export function StatusBar({ connectionStatus = 'connecting' }: StatusBarProps) {
  const { data: status, isLoading, error } = useStatus();
  const [nowMs, setNowMs] = useState(() => Date.now());

  useEffect(() => {
    const interval = setInterval(() => {
      setNowMs(Date.now());
    }, 1000);

    return () => clearInterval(interval);
  }, []);

  const derivedTimes = useMemo(
    () => (status && status.snapshot_received_at_ms
      ? deriveStatusTimes(status, nowMs, status.snapshot_received_at_ms)
      : null),
    [status, nowMs]
  );

  const getConnectionBadge = () => {
    switch (connectionStatus) {
      case 'connected':
        return <span className="status-bar__badge status-bar__badge--connected">Live</span>;
      case 'disconnected':
        return <span className="status-bar__badge status-bar__badge--disconnected">Offline</span>;
      case 'connecting':
      default:
        return <span className="status-bar__badge status-bar__badge--connecting">Connecting...</span>;
    }
  };

  if (isLoading) {
    return (
      <div className="status-bar">
        <div className="status-bar__item">
          <span className="status-bar__label">Status:</span>
          <span className="status-bar__value">Loading...</span>
        </div>
      </div>
    );
  }

  if (error) {
    return (
      <div className="status-bar status-bar--error">
        <div className="status-bar__item">
          <span className="status-bar__label">Status:</span>
          <span className="status-bar__value">Error loading status</span>
        </div>
      </div>
    );
  }

  if (!status) return null;

  const { counts, rate_limit, reviewer_running } = status;
  const uptimeSeconds = derivedTimes?.uptimeSeconds ?? status.uptime_seconds;
  const nextPollSeconds = derivedTimes?.nextPollSeconds ?? status.seconds_until_next_poll;

  return (
    <div className="status-bar status-bar--running">
      <div className="status-bar__item">
        <span className="status-bar__label">WS:</span>
        <span className="status-bar__value">
          {getConnectionBadge()}
        </span>
      </div>

      <div className="status-bar__item">
        <span className="status-bar__label">Status:</span>
        <span className="status-bar__value status-bar__value--running">
          🟢 Running
        </span>
      </div>

      <div className="status-bar__item">
        <span className="status-bar__label">PRs:</span>
        <span className="status-bar__value">
          {counts.completed} completed, {counts.generating} generating, {counts.pending} pending
          {counts.error > 0 && <span className="status-bar__error">, {counts.error} errors</span>}
        </span>
      </div>

      {reviewer_running && (
        <div className="status-bar__item">
          <span className="status-bar__label">Reviewer:</span>
          <span className="status-bar__value">🔄 Running</span>
        </div>
      )}

      {nextPollSeconds !== null && (
        <div className="status-bar__item">
          <span className="status-bar__label">Next Poll:</span>
          <span className="status-bar__value">{nextPollSeconds}s</span>
        </div>
      )}

      {uptimeSeconds !== undefined && (
        <div className="status-bar__item">
          <span className="status-bar__label">Uptime:</span>
          <span className="status-bar__value">{formatUptime(uptimeSeconds)}</span>
        </div>
      )}

      {rate_limit && (
        <div className="status-bar__item">
          <span className="status-bar__label">REST:</span>
          <span className="status-bar__value">
            {rate_limit.remaining}/{rate_limit.limit}
            {rate_limit.reset_at && ` (resets ${formatTime(rate_limit.reset_at)})`}
          </span>
        </div>
      )}

      {rate_limit && rate_limit.graphql_limit > 0 && (
        <div className="status-bar__item">
          <span className="status-bar__label">GraphQL:</span>
          <span className="status-bar__value">
            {rate_limit.graphql_remaining}/{rate_limit.graphql_limit}
            {rate_limit.graphql_reset_at && ` (resets ${formatTime(rate_limit.graphql_reset_at)})`}
          </span>
        </div>
      )}
    </div>
  );
}