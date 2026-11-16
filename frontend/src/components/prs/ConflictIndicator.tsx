import { memo } from 'react';
import type { PR } from '@/types/pr';

interface ConflictIndicatorProps {
  pr: Pick<PR, 'merge_state_status' | 'pr_state' | 'draft' | 'hidden'>;
}

export const ConflictIndicator = memo(function ConflictIndicator({ pr }: ConflictIndicatorProps) {
  if (pr.merge_state_status !== 'DIRTY' || pr.hidden) return null;
  if (pr.pr_state && pr.pr_state !== 'open') return null;
  const label = 'Merge conflicts with the base branch';
  return (
    <span className="pr-table__conflict" role="img" aria-label={label} title={label}>
      ⚔️
    </span>
  );
});
