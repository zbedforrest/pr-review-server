import { useState } from 'react';
import { useTelemetryStats } from '@/hooks/useTelemetryStats';
import './TelemetryDashboard.scss';

const TIME_RANGES = [
  { label: '1d', days: 1 },
  { label: '7d', days: 7 },
  { label: '30d', days: 30 },
];

export function TelemetryDashboard() {
  const [days, setDays] = useState(7);
  const { data: stats, isLoading } = useTelemetryStats(days);

  const maxActionCount = stats?.by_action?.length
    ? Math.max(...stats.by_action.map((a) => a.count))
    : 0;
  const maxDayCount = stats?.by_day?.length
    ? Math.max(...stats.by_day.map((d) => d.count))
    : 0;

  const topAction = stats?.by_action?.[0]?.action ?? '-';

  return (
    <div className="telemetry-dashboard">
      <div className="telemetry-dashboard__header">
        <h3>Usage Stats</h3>
        <div className="telemetry-dashboard__range-selector">
          {TIME_RANGES.map((r) => (
            <button
              key={r.days}
              className={days === r.days ? 'active' : ''}
              onClick={() => setDays(r.days)}
            >
              {r.label}
            </button>
          ))}
        </div>
      </div>

      {isLoading && <p className="telemetry-dashboard__loading">Loading stats...</p>}

      {stats && (
        <>
          {/* Summary cards */}
          <div className="telemetry-dashboard__summary">
            <div className="telemetry-dashboard__card">
              <span className="telemetry-dashboard__card-value">{stats.total_events}</span>
              <span className="telemetry-dashboard__card-label">Total Events</span>
            </div>
            <div className="telemetry-dashboard__card">
              <span className="telemetry-dashboard__card-value">{stats.active_users}</span>
              <span className="telemetry-dashboard__card-label">Active Users</span>
            </div>
            <div className="telemetry-dashboard__card">
              <span className="telemetry-dashboard__card-value">{topAction}</span>
              <span className="telemetry-dashboard__card-label">Top Action</span>
            </div>
          </div>

          {/* Action breakdown */}
          {stats.by_action.length > 0 && (
            <div className="telemetry-dashboard__section">
              <h4>Action Breakdown</h4>
              <div className="telemetry-dashboard__bars">
                {stats.by_action.map((a) => (
                  <div key={a.action} className="telemetry-dashboard__bar-row">
                    <span className="telemetry-dashboard__bar-label">{a.action}</span>
                    <div className="telemetry-dashboard__bar-track">
                      <div
                        className="telemetry-dashboard__bar-fill"
                        style={{ width: `${maxActionCount ? (a.count / maxActionCount) * 100 : 0}%` }}
                      />
                    </div>
                    <span className="telemetry-dashboard__bar-count">{a.count}</span>
                  </div>
                ))}
              </div>
            </div>
          )}

          {/* Daily activity */}
          {stats.by_day.length > 0 && (
            <div className="telemetry-dashboard__section">
              <h4>Daily Activity</h4>
              <div className="telemetry-dashboard__day-chart">
                {stats.by_day.map((d) => (
                  <div key={d.date} className="telemetry-dashboard__day-col">
                    <div className="telemetry-dashboard__day-bar-wrapper">
                      <div
                        className="telemetry-dashboard__day-bar"
                        style={{ height: `${maxDayCount ? (d.count / maxDayCount) * 100 : 0}%` }}
                        title={`${d.date}: ${d.count} events`}
                      />
                    </div>
                    <span className="telemetry-dashboard__day-label">
                      {d.date.slice(5)}
                    </span>
                  </div>
                ))}
              </div>
            </div>
          )}

          {/* Top searches */}
          {stats.top_searches.length > 0 && (
            <div className="telemetry-dashboard__section">
              <h4>Top Searches</h4>
              <ol className="telemetry-dashboard__ranked-list">
                {stats.top_searches.map((s) => (
                  <li key={s.label}>
                    <span className="telemetry-dashboard__ranked-text">{s.label}</span>
                    <span className="telemetry-dashboard__ranked-count">{s.count}</span>
                  </li>
                ))}
              </ol>
            </div>
          )}

          {/* Most interacted PRs */}
          {stats.top_prs.length > 0 && (
            <div className="telemetry-dashboard__section">
              <h4>Most Interacted PRs</h4>
              <ol className="telemetry-dashboard__ranked-list">
                {stats.top_prs.map((p) => (
                  <li key={`${p.owner}/${p.repo}/${p.number}`}>
                    <span className="telemetry-dashboard__ranked-text">
                      {p.owner}/{p.repo} #{p.number}
                    </span>
                    <span className="telemetry-dashboard__ranked-count">{p.count}</span>
                  </li>
                ))}
              </ol>
            </div>
          )}

          {stats.total_events === 0 && (
            <p className="telemetry-dashboard__empty">
              No telemetry data yet. Interact with the dashboard to generate events.
            </p>
          )}
        </>
      )}
    </div>
  );
}
