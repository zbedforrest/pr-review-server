import { useSettings, useUpdateSettings } from '@/hooks/useSettings';
import { useTelemetry } from '@/hooks/useTelemetry';

export function AutoReviewToggle() {
  const { data: settings } = useSettings();
  const updateSettings = useUpdateSettings();
  const { track } = useTelemetry();

  const autoReviewEnabled = settings?.auto_review_requested_prs ?? true;

  const handleToggleAutoReview = () => {
    if (!settings || updateSettings.isPending) return;

    const next = !settings.auto_review_requested_prs;
    track('toggle_auto_review', { label: next ? 'on' : 'off' });
    updateSettings.mutate({
      auto_review_requested_prs: next,
    });
  };

  return (
    <button
      className={`column-toggle-btn ${autoReviewEnabled ? '' : 'column-toggle-btn--inactive'}`}
      onClick={handleToggleAutoReview}
      title={autoReviewEnabled ? 'Disable auto-review for requested PRs' : 'Enable auto-review for requested PRs'}
      disabled={!settings || updateSettings.isPending}
    >
      {autoReviewEnabled ? '🤖 Auto-Review ON' : '🤖 Auto-Review OFF'}
    </button>
  );
}
