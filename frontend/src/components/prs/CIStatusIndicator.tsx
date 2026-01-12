import { memo } from 'react';

interface CIStatusIndicatorProps {
  state: 'success' | 'failure' | 'pending' | 'unknown';
  failedChecks: string[];
}

export const CIStatusIndicator = memo(function CIStatusIndicator({ state, failedChecks }: CIStatusIndicatorProps) {
  const getIcon = () => {
    switch (state) {
      case 'success':
        return '●'; // Green circle
      case 'failure':
        return '●'; // Red circle
      case 'pending':
        return '●'; // Yellow circle
      default:
        return '○'; // Gray circle outline
    }
  };

  const getTitle = () => {
    if (state === 'failure' && failedChecks.length > 0) {
      return `Failed checks:\n${failedChecks.join('\n')}`;
    }
    switch (state) {
      case 'success':
        return 'All checks passed';
      case 'failure':
        return 'Some checks failed';
      case 'pending':
        return 'Checks in progress';
      default:
        return 'No CI checks';
    }
  };

  return (
    <span
      className={`ci-status ci-status--${state}`}
      title={getTitle()}
    >
      {getIcon()}
    </span>
  );
});
