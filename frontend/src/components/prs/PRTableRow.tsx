import { memo, useCallback } from 'react';
import type { PR } from '@/types/pr';
import { CommitSha, StatusBadge, ReviewStatusEmoji } from '@/components/common';
import { useDeletePR } from '@/hooks/usePRs';

interface PRTableRowProps {
  pr: PR;
  showMyReview?: boolean;
}

export const PRTableRow = memo(function PRTableRow({ pr, showMyReview = false }: PRTableRowProps) {
  const deleteMutation = useDeletePR();
  const prUrl = `https://github.com/${pr.owner}/${pr.repo}/pull/${pr.number}`;
  const reviewUrl = pr.status === 'completed' && pr.review_url
    ? pr.review_url
    : null;

  const handleDelete = useCallback(() => {
    if (window.confirm(`Delete PR ${pr.owner}/${pr.repo}#${pr.number} from the system?`)) {
      deleteMutation.mutate({
        owner: pr.owner,
        repo: pr.repo,
        number: pr.number,
      });
    }
  }, [pr.owner, pr.repo, pr.number, deleteMutation]);

  return (
    <tr>
      <td>
        <a href={prUrl} target="_blank" rel="noopener noreferrer">
          {pr.owner}/{pr.repo} #{pr.number}
        </a>
        {pr.draft && <span className="pr-table__draft-indicator"> (Draft)</span>}
        <div className="pr-table__title">{pr.title}</div>
      </td>
      <td>{pr.author}</td>
      <td>
        <CommitSha sha={pr.commit_sha} owner={pr.owner} repo={pr.repo} />
      </td>
      <td>
        <StatusBadge status={pr.status} generatingSince={pr.generating_since} />
      </td>
      {showMyReview && (
        <td className="pr-table__review-emoji">
          <ReviewStatusEmoji status={pr.my_review_status} />
        </td>
      )}
      <td className={`pr-table__approval-count ${pr.approval_count > 0 ? 'pr-table__approval-count--positive' : 'pr-table__approval-count--zero'}`}>
        {pr.approval_count}
      </td>
      <td>
        {reviewUrl ? (
          <a href={reviewUrl} target="_blank" rel="noopener noreferrer">
            View Review
          </a>
        ) : (
          <span>-</span>
        )}
      </td>
      <td>
        <button
          className="pr-table__delete-btn"
          onClick={handleDelete}
          disabled={deleteMutation.isPending}
          title="Remove from system"
        >
          {deleteMutation.isPending ? 'Deleting...' : 'Delete'}
        </button>
      </td>
    </tr>
  );
});
