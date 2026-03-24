import { TelemetryDashboard } from './TelemetryDashboard';
import '@/styles/main.scss';

export function UsageStatsPage() {
  return (
    <div className="app-container">
      <header className="app-header">
        <h1>Usage Stats</h1>
        <a
          href="/"
          style={{
            padding: '8px 16px',
            fontSize: '14px',
            borderRadius: '6px',
            border: '1px solid #30363d',
            backgroundColor: '#21262d',
            color: '#c9d1d9',
            textDecoration: 'none',
            display: 'flex',
            alignItems: 'center',
            gap: '8px',
          }}
        >
          Back to Dashboard
        </a>
      </header>
      <TelemetryDashboard />
    </div>
  );
}
