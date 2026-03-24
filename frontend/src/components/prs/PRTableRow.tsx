import { memo, useCallback } from 'react';
import type { PR } from '@/types/pr';
import { CommitSha, StatusBadge } from '@/components/common';
import { useDeletePR, useTriggerReview } from '@/hooks/usePRs';
import { useTelemetry } from '@/hooks/useTelemetry';
import { CIStatusIndicator } from './CIStatusIndicator';
import { NotesCell } from './NotesCell';
import { VIA_TEAMS_PERSONAL } from '@/constants';

interface PRTableRowProps {
  pr: PR;
  showReviewColumns?: boolean;
  showViaTeams?: boolean;
}

export const PRTableRow = memo(function PRTableRow({
  pr,
  showReviewColumns = true,
  showViaTeams = true
}: PRTableRowProps) {
  const deleteMutation = useDeletePR();
  const triggerReviewMutation = useTriggerReview();
  const { track } = useTelemetry();
  const prUrl = `https://github.com/${pr.owner}/${pr.repo}/pull/${pr.number}`;
  const reviewUrl = pr.status === 'completed' && pr.review_url
    ? pr.review_url
    : null;

  const handleDelete = useCallback(() => {
    track('delete_pr', { pr_owner: pr.owner, pr_repo: pr.repo, pr_number: pr.number });
    deleteMutation.mutate({
      owner: pr.owner,
      repo: pr.repo,
      number: pr.number,
    });
  }, [pr.owner, pr.repo, pr.number, deleteMutation, track]);

  const handleTriggerReview = useCallback(() => {
    track('trigger_review', { pr_owner: pr.owner, pr_repo: pr.repo, pr_number: pr.number });
    triggerReviewMutation.mutate({
      owner: pr.owner,
      repo: pr.repo,
      number: pr.number,
    });
  }, [pr.owner, pr.repo, pr.number, triggerReviewMutation, track]);

  return (
    <tr>
      <td>
        <a href={prUrl} onClick={() => track('open_pr_github', { pr_owner: pr.owner, pr_repo: pr.repo, pr_number: pr.number })}>
          {pr.owner}/{pr.repo} #{pr.number}
        </a>
        {pr.draft && <span className="pr-table__draft-indicator"> (Draft)</span>}
        <div className="pr-table__title">{pr.title}</div>
      </td>
      <td>{pr.author}</td>
      <td>
        <CommitSha sha={pr.commit_sha} owner={pr.owner} repo={pr.repo} />
      </td>
      {showReviewColumns && (
        <td>
          <StatusBadge status={pr.status} generatingSince={pr.generating_since} />
        </td>
      )}
      <td className="pr-table__ci-status">
        <CIStatusIndicator state={pr.ci_state} failedChecks={pr.ci_failed_checks} />
      </td>
      <td className={`pr-table__approval-count ${pr.approval_count > 0 ? 'pr-table__approval-count--positive' : 'pr-table__approval-count--zero'}`}>
        {pr.approval_count}
      </td>
      <td className="pr-table__my-review">
        {pr.my_review_status === 'APPROVED' && <span className="pr-table__my-review--approved" title="You approved this PR">✓</span>}
        {pr.my_review_status === 'CHANGES_REQUESTED' && <span className="pr-table__my-review--changes" title="You requested changes">✗</span>}
        {pr.my_review_status === 'COMMENTED' && <span className="pr-table__my-review--commented" title="You commented">💬</span>}
        {!pr.my_review_status && <span className="pr-table__my-review--none" title="No review yet">-</span>}
      </td>
      {showViaTeams && (
        <td className="pr-table__via-teams">
          {pr.via_teams && pr.via_teams.length > 0 ? (
            (() => {
              const hasPersonal = pr.via_teams.includes(VIA_TEAMS_PERSONAL);
              const teamEntries = pr.via_teams
                .filter(t => t !== VIA_TEAMS_PERSONAL)
                .map(t => {
                  const colonIdx = t.indexOf(':');
                  if (colonIdx >= 0) {
                    return { name: t.slice(0, colonIdx), status: t.slice(colonIdx + 1) };
                  }
                  return { name: t, status: 'pending' };
                });
              const parts = [
                ...(hasPersonal ? [{ name: '@you', status: 'personal' }] : []),
                ...teamEntries,
              ];
              return (
                <span title={parts.map(p => `${p.name} (${p.status})`).join(', ')}>
                  {parts.map((p, i) => (
                    <span key={i}>
                      {i > 0 && ', '}
                      <span className={`pr-table__via-teams--${p.status}`}>{p.name}</span>
                    </span>
                  ))}
                </span>
              );
            })()
          ) : (
            <span className="pr-table__via-teams--none" title="Via team (auto-assigned)">-</span>
          )}
        </td>
      )}
      <td>
        <NotesCell
          owner={pr.owner}
          repo={pr.repo}
          number={pr.number}
          notes={pr.notes || ''}
        />
      </td>
      {showReviewColumns && (
        <td>
          {reviewUrl ? (
            <a href={reviewUrl} onClick={() => track('view_review', { pr_owner: pr.owner, pr_repo: pr.repo, pr_number: pr.number })}>
              View Review
            </a>
          ) : (
            <span>-</span>
          )}
        </td>
      )}
      <td>
        <div style={{ display: 'flex', gap: '4px', flexWrap: 'wrap' }}>
          <button
            className="pr-table__action-btn"
            onClick={handleTriggerReview}
            disabled={triggerReviewMutation.isPending || pr.status === 'generating'}
            title="Generate or regenerate review for this PR"
          >
            {triggerReviewMutation.isPending ? 'Triggering...' : '🔄 Review'}
          </button>
          <button
            className="pr-table__delete-btn"
            onClick={handleDelete}
            disabled={deleteMutation.isPending}
            title="Remove from system"
          >
            {deleteMutation.isPending ? 'Deleting...' : 'Delete'}
          </button>
        </div>
      </td>
    </tr>
  );
});
