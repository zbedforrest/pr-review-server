import { TelemetryDashboard } from './TelemetryDashboard';
import '@/styles/main.scss';

export function UsageStatsPage() {
  return (
    <div className="app-container">
      <header className="app-header">
        <div className="app-header__branding">
          <h1>Usage Stats</h1>
        </div>
        <div className="app-header__actions">
          <a href="/" className="app-header__action-link">
            Back to Dashboard
          </a>
        </div>
      </header>
      <TelemetryDashboard />
    </div>
  );
}
