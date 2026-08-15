import { useState } from 'react';
import { useTelemetry } from '@/hooks/useTelemetry';
import { GenerateReviewForm } from './GenerateReviewForm';
import { ThemeSelector } from './ThemeSelector';

export function Header() {
  const [isPolling, setIsPolling] = useState(false);
  const { track } = useTelemetry();

  const handleTriggerPoll = async () => {
    track('refresh_poll');
    setIsPolling(true);
    try {
      await fetch('/api/poll/trigger', { method: 'POST' });
    } catch (error) {
      console.error('Failed to trigger poll:', error);
    } finally {
      // Keep spinner for a bit to indicate activity
      setTimeout(() => setIsPolling(false), 2000);
    }
  };

  return (
    <header className="app-header">
      <div className="app-header__branding">
        <h1 className="app-header__title">PRism</h1>
        <p className="app-header__subtitle">PR Review Dashboard</p>
      </div>
      <div className="app-header__actions">
        <GenerateReviewForm />
        <a
          href="/usage-stats"
          className="app-header__action-link"
        >
          Stats
        </a>
        <button
          onClick={handleTriggerPoll}
          disabled={isPolling}
          className="app-header__action-btn"
        >
          {isPolling ? 'Polling...' : 'Refresh PRs'}
        </button>
        <ThemeSelector />
      </div>
    </header>
  );
}
