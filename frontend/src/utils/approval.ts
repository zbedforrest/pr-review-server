import type { PR } from '@/types/pr';
import type { ApprovalTarget } from '@/types/approval';
import { filterAndSortPRs, type PRFilterCriteria } from './sectionFilters';

export const approvalKey = (pr: { owner: string; repo: string; number: number }) => `${pr.owner}/${pr.repo}/${pr.number}`.toLowerCase();
export function approvalScope(prs: PR[], filters: PRFilterCriteria, username?: string): PR[] {
  const seen = new Set<string>();
  return filterAndSortPRs(prs, filters, username).filter(pr => {
    const key = approvalKey(pr);
    if (seen.has(key)) return false;
    seen.add(key);
    return !pr.is_mine && !pr.hidden && (!pr.pr_state || pr.pr_state === 'open') &&
      !(pr.my_review_status === 'APPROVED' && pr.my_review_commit_sha === pr.commit_sha) && /^[0-9a-f]{40}$/i.test(pr.commit_sha);
  });
}
export function isApprovalCandidate(target: ApprovalTarget, now = Date.now()): boolean {
  return target.execution_status === 'completed' && target.decision === 'candidate' &&
    target.freshness_state === 'current' && !!target.valid_until && Date.parse(target.valid_until) > now;
}
export function approvalBucket(target: ApprovalTarget, now = Date.now()): string {
  if (['failed', 'timed_out', 'cancelled'].includes(target.execution_status)) return target.execution_status;
  if (target.execution_status !== 'completed') return target.execution_status;
  if (target.freshness_state === 'stale' || (target.decision === 'candidate' && !isApprovalCandidate(target, now))) return 'stale';
  return target.decision || 'insufficient_evidence';
}
export function safeEvidenceURL(value: string): string | undefined {
  try { const url = new URL(value, window.location.origin); if (url.protocol !== 'https:' && url.origin !== window.location.origin) return undefined;
    if (url.origin === window.location.origin && url.pathname.startsWith('/api/v1/approval-')) return url.href;
    if (url.protocol === 'https:' && url.hostname === 'github.com' && !url.username && !url.password) return url.href;
  } catch { return undefined; }
  return undefined;
}

export function reconcileApprovalTarget(target: ApprovalTarget, pr: PR, latest?: ApprovalTarget): ApprovalTarget {
  const current = latest?.target_id === target.target_id
    ? { ...target, ...latest, assessment: target.assessment, snapshot: target.snapshot }
    : { ...target };
  if (latest && latest.target_id === target.target_id) {
    const targetValidated = Date.parse(target.validated_at || '') || 0;
    const latestValidated = Date.parse(latest.validated_at || '') || 0;
    const freshness = targetValidated > latestValidated ? target : latest;
    current.validated_at = freshness.validated_at;
    current.valid_until = freshness.valid_until;
    current.freshness_state = freshness.freshness_state;
    if (target.freshness_state === 'stale' || latest.freshness_state === 'stale') {
      current.freshness_state = 'stale';
      current.reason_codes = Array.from(new Set([...(target.reason_codes || []), ...(latest.reason_codes || [])]));
    } else if (targetValidated === latestValidated && target.freshness_state === 'expired') {
      current.freshness_state = 'expired';
      current.valid_until = target.valid_until;
    }
  }
  // A non-candidate already decided on a condition is not changed by observing it again.
  const assessed = new Set(target.decision === 'candidate' ? [] : current.reason_codes || []);
  const unlisted = (reason: string) => !assessed.has(reason);
  const observed: [boolean, string][] = [
    [!!latest && latest.target_id !== target.target_id, 'superseded'],
    [pr.commit_sha !== target.revision, 'head_changed'],
    [!!pr.hidden, 'hidden'],
    [!!pr.draft && unlisted('pr_draft'), 'pr_draft'],
    [!!pr.pr_state && pr.pr_state !== 'open', 'closed'],
    [!!pr.is_mine, 'self_authored'],
    [pr.ci_state === 'failure' && unlisted('ci_failed'), 'ci_failed'],
    [pr.ci_state === 'pending' && unlisted('ci_pending'), 'ci_pending'],
    [(pr.review_decision === 'CHANGES_REQUESTED' || pr.my_review_status === 'CHANGES_REQUESTED') && unlisted('human_changes_requested'), 'human_changes_requested'],
    [['generating', 'agent_reviewing'].includes(pr.status) && unlisted('review_in_progress'), 'review_in_progress'],
    [pr.my_review_status === 'APPROVED' && pr.my_review_commit_sha === pr.commit_sha, 'already_approved'],
  ];
  const reason = observed.find(([seen]) => seen)?.[1];
  if (reason) return { ...current, freshness_state: 'stale', reason_codes: Array.from(new Set([...(current.reason_codes || []), reason])) };
  return current;
}
