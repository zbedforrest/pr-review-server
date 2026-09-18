import { memo } from 'react';
import type { PR } from '@/types/pr';

interface MergeReadyIndicatorProps {
  pr: Pick<PR, 'ready_to_merge' | 'approval_count' | 'review_decision' | 'merge_state_status' | 'pr_state' | 'draft' | 'hidden'>;
}

export const MergeReadyIndicator = memo(function MergeReadyIndicator({ pr }: MergeReadyIndicatorProps) {
  // Server decides readiness; the client only re-checks the cheap
  // invariants so a stale or older payload can never show a false check.
  if (!pr.ready_to_merge || pr.hidden || pr.draft) return null;
  if (pr.pr_state && pr.pr_state !== 'open') return null;
  const approvals = `${pr.approval_count} approval${pr.approval_count === 1 ? '' : 's'}`;
  const reviews = pr.review_decision === 'APPROVED' ? 'required reviews approved' : 'no required reviews outstanding';
  const label = `Ready to merge: ${approvals}, ${reviews}, required checks passing, no conflicts`;
  return (
    <span className="pr-table__merge-ready" role="img" aria-label={label} title={label}>
      ✅
    </span>
  );
});
