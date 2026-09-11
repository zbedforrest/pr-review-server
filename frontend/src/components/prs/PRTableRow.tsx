import { memo, useCallback, type MouseEvent } from 'react';
import type { PR } from '@/types/pr';
import { CommitSha } from '@/components/common';
import { useDeletePR, useSetPRHidden, useTriggerReview } from '@/hooks/usePRs';
import { useSettings } from '@/hooks/useSettings';
import { useTelemetry } from '@/hooks/useTelemetry';
import { CIStatusIndicator } from './CIStatusIndicator';
import { GenerateSplitButton } from './GenerateSplitButton';
import { NotesCell } from './NotesCell';
import { publishAllowedForAuthor } from './publishPolicy';
import { ReviewLinkMenu } from './ReviewLinkMenu';
import { RowActionsMenu } from './RowActionsMenu';
import { buildViaTeamParts } from '@/utils/teamFilters';

export type PRRowVariant = 'default' | 'attention';

interface PRTableRowProps {
  pr: PR;
  showViaTeams?: boolean;
  variant?: PRRowVariant;
}

export const PRTableRow = memo(function PRTableRow({
  pr,
  showViaTeams = true,
  variant = 'default'
}: PRTableRowProps) {
  const deleteMutation = useDeletePR();
  const setHiddenMutation = useSetPRHidden();
  const triggerReviewMutation = useTriggerReview();
  const { data: settings } = useSettings();
  const { track } = useTelemetry();
  // Until settings load we cannot know the pilot list; assume allowed so the
  // control does not flash to its disabled form on first paint.
  const publishAllowed = settings === undefined
    ? true
    : publishAllowedForAuthor(pr.author, settings.publish_enabled_authors);
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

  const handleToggleHidden = useCallback(() => {
    const hidden = !pr.hidden;
    track(hidden ? 'hide_pr' : 'unhide_pr', { pr_owner: pr.owner, pr_repo: pr.repo, pr_number: pr.number });
    setHiddenMutation.mutate({
      owner: pr.owner,
      repo: pr.repo,
      number: pr.number,
      hidden,
    });
  }, [pr.owner, pr.repo, pr.number, pr.hidden, setHiddenMutation, track]);

  const handleTriggerReview = useCallback((publish: boolean) => {
    track('trigger_review', { pr_owner: pr.owner, pr_repo: pr.repo, pr_number: pr.number, publish });
    triggerReviewMutation.mutate({
      owner: pr.owner,
      repo: pr.repo,
      number: pr.number,
      publish,
    });
  }, [pr.owner, pr.repo, pr.number, triggerReviewMutation, track]);

  const openOnGitHub = useCallback((url: string, label?: string) => (e: MouseEvent<HTMLAnchorElement>) => {
    track('open_pr_github', { pr_owner: pr.owner, pr_repo: pr.repo, pr_number: pr.number, label });
    // Opt-in same-tab: Alt/Option+click (and only Alt) navigates the current
    // tab instead of opening a new one. Plain click, Ctrl/Cmd/Shift/middle-click,
    // and any Alt+other-modifier combo keep the browser's default new-tab
    // behavior via target="_blank".
    if (e.altKey && !e.ctrlKey && !e.metaKey && !e.shiftKey) {
      e.preventDefault();
      window.location.assign(url);
    }
  }, [pr.owner, pr.repo, pr.number, track]);

  return (
    <tr className={variant === 'attention' ? 'pr-table__row--attention' : undefined}>
      <td>
        <a href={prUrl} target="_blank" rel="noopener noreferrer" title="Alt/Option-click to open in this tab" onClick={openOnGitHub(prUrl)}>
          {pr.owner}/{pr.repo} #{pr.number}
        </a>
        {pr.draft && <span className="pr-table__draft-indicator"> (Draft)</span>}
        <div className="pr-table__title">{pr.title}</div>
        {variant === 'attention' && (
          <div className="pr-table__attention">
            <span
              className="pr-table__attention-badge"
              title="The current head differs from the commit you reviewed when requesting changes"
            >
              Updated since your review
            </span>
            <a
              className="pr-table__attention-link"
              href={`${prUrl}/files`}
              target="_blank"
              rel="noopener noreferrer"
              title="Alt/Option-click to open in this tab"
              onClick={openOnGitHub(`${prUrl}/files`, 'needs_re_review')}
            >
              Review on GitHub
            </a>
          </div>
        )}
      </td>
      <td>{pr.author}</td>
      <td>
        <CommitSha sha={pr.commit_sha} owner={pr.owner} repo={pr.repo} />
      </td>
      <td className="pr-table__ci-status">
        <CIStatusIndicator state={pr.ci_state} failedChecks={pr.ci_failed_checks} prState={pr.pr_state} />
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
              const parts = buildViaTeamParts(pr.via_teams);
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
      <td className="pr-table__review-cell">
        {pr.status === 'error' ? (
          <span className="pr-table__review-error" title={pr.error_message || 'Review failed'}>
            ERROR
          </span>
        ) : pr.status === 'generating' || pr.status === 'agent_reviewing' ? (
          <span
            className={`pr-table__review-generating pr-table__review-generating--${
              pr.status === 'agent_reviewing' ? 'agent' : 'gemini'
            }`}
            title="AI review in progress"
          >
            Generating…
          </span>
        ) : reviewUrl ? (
          <ReviewLinkMenu
            pr={pr}
            reviewUrl={reviewUrl}
            onTriggerReview={handleTriggerReview}
            reviewPending={triggerReviewMutation.isPending}
            publishAllowed={publishAllowed}
          />
        ) : (
          // No up-to-date review: brand-new PR, or one whose prior review was
          // cleared server-side after a new commit made it stale.
          <GenerateSplitButton
            onGenerate={handleTriggerReview}
            pending={triggerReviewMutation.isPending}
            publishAllowed={publishAllowed}
          />
        )}
      </td>
      <td>
        <RowActionsMenu
          pr={pr}
          onTriggerReview={handleTriggerReview}
          onToggleHidden={handleToggleHidden}
          onDelete={handleDelete}
          reviewPending={triggerReviewMutation.isPending}
          hiddenPending={setHiddenMutation.isPending}
          deletePending={deleteMutation.isPending}
          publishAllowed={publishAllowed}
        />
      </td>
    </tr>
  );
});
