import { useMemo } from 'react';
import type { PR } from '@/types/pr';

interface StatusBadgeProps {
  status: PR['status'];
  generatingSince?: string | null;
}

export function StatusBadge({ status, generatingSince }: StatusBadgeProps) {
  const elapsedTime = useMemo(() => {
    if (status !== 'generating' || !generatingSince) return null;

    const startTime = new Date(generatingSince).getTime();
    const now = Date.now();
    const elapsedSeconds = Math.floor((now - startTime) / 1000);

    if (elapsedSeconds < 60) return `${elapsedSeconds}s`;
    const elapsedMinutes = Math.floor(elapsedSeconds / 60);
    return `${elapsedMinutes}m`;
  }, [status, generatingSince]);

  const statusText = useMemo(() => {
    switch (status) {
      case 'pending':
        return 'Pending';
      case 'generating':
        return elapsedTime ? `Generating (${elapsedTime})` : 'Generating';
      case 'completed':
        return 'Completed';
      case 'error':
        return 'Error';
      default:
        return status;
    }
  }, [status, elapsedTime]);

  return (
    <span className={`status-badge status-badge--${status}`}>
      {statusText}
    </span>
  );
}
