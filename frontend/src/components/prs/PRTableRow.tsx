import { memo, useCallback, useMemo, type MouseEvent } from 'react';
import type { PR, ReviewProfile } from '@/types/pr';
import { APIError } from '@/api/client';
import { newRequestId, type QuickAction, type QuickActionResponse } from '@/api/prActions';
import { CommitSha } from '@/components/common';
import { toast } from '@/components/common/toastStore';
import { useCurrentUser } from '@/hooks/useCurrentUser';
import { useSubmitQuickAction } from '@/hooks/usePRActions';
import { useDeletePR, useSetPRHidden, useTriggerReview } from '@/hooks/usePRs';
import { useSettings } from '@/hooks/useSettings';
import { useTelemetry } from '@/hooks/useTelemetry';
import { CIStatusIndicator } from './CIStatusIndicator';
import { ConfidenceBadge } from './ConfidenceBadge';
import { GenerateSplitButton } from './GenerateSplitButton';
import { MergeReadyIndicator } from './MergeReadyIndicator';
import { NotesCell } from './NotesCell';
import { publishAllowedForAuthor } from './publishPolicy';
import { PROFILE_LABELS } from './reviewProfiles';
import { ReviewLinkMenu } from './ReviewLinkMenu';
import { RowActionsMenu, type QuickActionsWiring } from './RowActionsMenu';
import { buildViaTeamParts } from '@/utils/teamFilters';

export type PRRowVariant = 'default' | 'attention';

const QUICK_ACTION_TOAST: Record<QuickAction, { verb: string; show: typeof toast.success }> = {
  approve: { verb: 'Approved', show: toast.success },
  request_changes: { verb: 'Requested changes on', show: toast.error },
  comment: { verb: 'Commented on', show: toast.info },
};

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
  const quickActionMutation = useSubmitQuickAction();
  const { data: currentUser } = useCurrentUser();
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
  const liteProfile = reviewUrl && (pr.review_run?.profile === 'lite' || pr.review_run?.profile === 'lite_plus')
    ? pr.review_run.profile
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

  const handleTriggerReview = useCallback((publish: boolean, profile?: ReviewProfile) => {
    track('trigger_review', { pr_owner: pr.owner, pr_repo: pr.repo, pr_number: pr.number, publish, profile });
    triggerReviewMutation.mutate({
      owner: pr.owner,
      repo: pr.repo,
      number: pr.number,
      publish,
      profile,
    });
  }, [pr.owner, pr.repo, pr.number, triggerReviewMutation, track]);

  const submitQuickAction = useCallback(async (action: QuickAction, body: string, expectedHeadSha: string): Promise<QuickActionResponse> => {
    const opts = { pr_owner: pr.owner, pr_repo: pr.repo, pr_number: pr.number };
    try {
      const result = await quickActionMutation.mutateAsync({
        owner: pr.owner,
        repo: pr.repo,
        number: pr.number,
        action,
        body,
        expected_head_sha: expectedHeadSha,
        request_id: newRequestId(),
      });
      track(`quick_action_${action}`, { ...opts, label: 'ok' });
      const { verb, show } = QUICK_ACTION_TOAST[action];
      show({ title: `${verb} ${pr.owner}/${pr.repo} #${pr.number} as @${result.actor}`, href: result.html_url });
      return result;
    } catch (err) {
      const code = err instanceof APIError && err.code ? err.code : 'network';
      track(`quick_action_${action}`, { ...opts, label: code });
      throw err;
    }
  }, [pr.owner, pr.repo, pr.number, quickActionMutation, track]);

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

  const trackCopyLink = useCallback(() => {
    track('quick_action_copy_link', { pr_owner: pr.owner, pr_repo: pr.repo, pr_number: pr.number });
  }, [pr.owner, pr.repo, pr.number, track]);

  const quickActions = useMemo<QuickActionsWiring | undefined>(() => {
    if (!currentUser?.quick_actions_enabled) return undefined;
    return {
      user: currentUser,
      onSubmit: submitQuickAction,
      onOpenGitHub: openOnGitHub(prUrl, 'quick_actions'),
      onCopyLink: trackCopyLink,
      onDialogClose: quickActionMutation.reset,
      pending: quickActionMutation.isPending,
      error: quickActionMutation.error ?? null,
    };
  }, [currentUser, submitQuickAction, openOnGitHub, prUrl, trackCopyLink, quickActionMutation.reset, quickActionMutation.isPending, quickActionMutation.error]);

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
        <MergeReadyIndicator pr={pr} />
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
      <td className="pr-table__confidence">
        <ConfidenceBadge score={pr.merge_confidence} size="row" />
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
          <>
            <ReviewLinkMenu
              pr={pr}
              reviewUrl={reviewUrl}
              onTriggerReview={handleTriggerReview}
              reviewPending={triggerReviewMutation.isPending}
              publishAllowed={publishAllowed}
            />
            {liteProfile && (
              <span
                className={`pr-table__profile-chip pr-table__profile-chip--${liteProfile}`}
                title={`Review profile: ${pr.review_run?.profile_label || PROFILE_LABELS[liteProfile]}`}
              >
                {PROFILE_LABELS[liteProfile]}
              </span>
            )}
          </>
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
          quickActions={quickActions}
        />
      </td>
    </tr>
  );
});
