import { useStatus } from '@/hooks/useStatus';
import { formatUptime } from '@/utils/formatUptime';
import { formatTime } from '@/utils/formatDate';

export function StatusBar() {
  const { data: status, isLoading, error } = useStatus();

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

  const { counts, uptime_seconds, rate_limit, cbpr_running, seconds_until_next_poll } = status;

  return (
    <div className="status-bar status-bar--running">
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

      {cbpr_running && (
        <div className="status-bar__item">
          <span className="status-bar__label">CBPR:</span>
          <span className="status-bar__value">🔄 Running</span>
        </div>
      )}

      {seconds_until_next_poll !== undefined && (
        <div className="status-bar__item">
          <span className="status-bar__label">Next Poll:</span>
          <span className="status-bar__value">{seconds_until_next_poll}s</span>
        </div>
      )}

      {uptime_seconds !== undefined && (
        <div className="status-bar__item">
          <span className="status-bar__label">Uptime:</span>
          <span className="status-bar__value">{formatUptime(uptime_seconds)}</span>
        </div>
      )}

      {rate_limit && (
        <div className="status-bar__item">
          <span className="status-bar__label">Rate Limit:</span>
          <span className="status-bar__value">
            {rate_limit.remaining}/{rate_limit.limit}
            {rate_limit.reset_at && ` (resets ${formatTime(rate_limit.reset_at)})`}
          </span>
        </div>
      )}
    </div>
  );
}
